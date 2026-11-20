package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/worker/store"
	"github.com/patrick-1984/ai-minion/internal/worker/term"
)

// Modes of a Worker as `minion worker status` reports them.
const (
	// ModeSolo is a Worker without a Hub URL.
	ModeSolo = "solo"
	// ModeHub is a Worker with a Hub URL.
	ModeHub = "hub"
)

// ConnUnknown is the connection state of a Worker that has a Hub URL when no
// running Worker was asked or none answered: the state of the connector is
// live data that only the running Worker knows.
const ConnUnknown = "unknown"

// ConnNone is the connection state of a Worker without a Hub URL.
const ConnNone = "none"

// The values of [StatusReport.PairingState].
const (
	// PairingPending: the Worker shows a pairing code and waits for it.
	PairingPending = "pending"
	// PairingNone: a running Worker has no pairing code pending.
	PairingNone = "none"
	// PairingUnknown: no running Worker answered, or it cannot tell.
	PairingUnknown = "unknown"
)

// The values of [StatusReport.Approval].
const (
	// ApprovalApproved: the Worker is connected and the Hub serves it.
	ApprovalApproved = "approved"
	// ApprovalPending: the Worker is connected and waits for its owner or an
	// admin to approve it.
	ApprovalPending = "pending"
	// ApprovalUnknown: only the welcome of a live connection tells.
	ApprovalUnknown = "unknown"
)

// ErrNoLiveWorker is returned by a [LiveSource] (and by the local client of
// the CLI) when no running Worker answers. It is not a failure: status then
// reports what the state directory and the settings say, and a settings
// change applies at the next start.
var ErrNoLiveWorker = errors.New("agent: no running Worker answered")

// LiveStatus is what only a running Worker knows. The local socket of the
// Worker (AD-22) serves it; [LiveSource] builds it inside the Worker.
type LiveStatus struct {
	// Connector is the snapshot of the Hub connector.
	Connector Status
	// HubPreviewOnly is true when the Hub restricts this Worker to preview.
	HubPreviewOnly bool
	// PendingPairingCode is the pairing code the Worker shows and waits for,
	// empty when there is none. It is printed for the human (AD-20).
	PendingPairingCode string
	// ApprovalKnown is true when Connector.Welcome.Pending is reported: a
	// Worker of an older version does not send it, and its false then means
	// "not known", not "approved".
	ApprovalKnown bool
	// PairingKnown is true when the Worker can tell whether a pairing code is
	// pending. When it is false an empty PendingPairingCode means "not known",
	// not "none pending".
	PairingKnown bool
	// RunMode is how the running Worker runs (RunMode*); empty when an older
	// Worker answered.
	RunMode string
	// Host is the session host of the running Worker and whether its sessions
	// are hidden; zero when not known. ListError is the latest listing error.
	Host      SessionHostInfo
	ListError string
	// LogSinks is the state of the remote log sinks (syslog, otel); nil
	// from an older Worker.
	LogSinks []ops.LogSinkStatus
	// Paused is the pause the running Worker applies; PauseKnown is false for
	// an older Worker, which cannot pause.
	Paused, PauseKnown bool
	// NewSessions is allow_new_sessions (on or off) as the running Worker
	// enforces it; empty from an older Worker.
	NewSessions string
}

// LiveSource answers for a running Worker. It returns [ErrNoLiveWorker] when
// none is running.
type LiveSource interface {
	LiveStatus(ctx context.Context) (LiveStatus, error)
}

// LiveStatusSource is the [LiveSource] a running Worker builds from its own
// services; the local socket serves its result.
type LiveStatusSource struct {
	// Connector is the Hub connector; nil means the Worker has none.
	Connector *Connector
	// Policy is the local policy; nil means no Hub restriction is known.
	Policy *Policy
	// Pairing returns the pending pairing code, if any; nil means none.
	Pairing func() (code string, ok bool)
	// RunMode is how this Worker runs; Host its session host; Health the
	// outcome of its latest look at the sessions. Each may be empty or nil.
	RunMode string
	Host    *SessionHostInfo
	Health  *SessionListHealth
	// LogSinks reports the remote log sinks; nil reports none.
	LogSinks func() []ops.LogSinkStatus
	// Pause is the pause of the Hub traffic; nil means the Worker cannot pause.
	Pause *PauseService
}

// LiveStatus implements [LiveSource].
func (s LiveStatusSource) LiveStatus(context.Context) (LiveStatus, error) {
	var out LiveStatus
	if s.Connector != nil {
		out.Connector = s.Connector.Status()
		out.ApprovalKnown = true
	}
	if s.Policy != nil {
		out.HubPreviewOnly = s.Policy.HubPreviewOnly()
		out.NewSessions = config.AllowNewSessionsOn
		if !s.Policy.NewSessionsAllowed() {
			out.NewSessions = config.AllowNewSessionsOff
		}
	}
	out.RunMode = s.RunMode
	if s.Pause != nil {
		out.Paused, out.PauseKnown = s.Pause.Paused(), true
	}
	// The local status shows the details the hello leaves out (a socket
	// path, a user name, the error itself): only this machine reads it.
	if s.Host != nil {
		out.Host = *s.Host
		out.Host.Reason = s.Host.localReason()
	}
	out.ListError = s.Health.LocalText()
	if s.LogSinks != nil {
		out.LogSinks = s.LogSinks()
	}
	if s.Pairing != nil {
		out.PairingKnown = true
		if code, ok := s.Pairing(); ok {
			out.PendingPairingCode = code
		}
	}
	return out, nil
}

// Ports of the status report. Each is satisfied by a type the Worker already
// has, so that the report is built the same way inside the Worker and from
// the state directory by the CLI.
type (
	// StatusEnrollment reads the enrollment; *Enrollments implements it.
	StatusEnrollment interface {
		Load(ctx context.Context) (Enrollment, bool, error)
	}
	// StatusKeys loads the Worker key; *Enrollments implements it.
	StatusKeys interface {
		LoadKey() (*identity.Key, error)
	}
	// StatusSessions lists the sessions of the terminal host; term.SessionHost
	// implements it.
	StatusSessions interface {
		ListSessions(ctx context.Context) ([]term.Session, error)
	}
)

// StatusInputs is everything [BuildStatus] reads. A nil port means "not
// available" and the matching fields stay empty; a port that fails adds a
// line to [StatusReport.Problems] instead of failing the report, because
// status is what a person runs when something is wrong.
type StatusInputs struct {
	// Version is the release version of this binary.
	Version string
	// Name is the Worker's display name.
	Name string
	// HubURL is the configured Hub URL; empty means Solo.
	HubURL string
	// TmuxSocket is the resolved tmux socket path.
	TmuxSocket string
	// PreviewOnly is the local preview-only setting.
	PreviewOnly bool
	// Platform is the operating system (runtime.GOOS). A platform that is
	// still a preview (Windows) is labelled so in the report.
	Platform string

	Enrollment StatusEnrollment
	Keys       StatusKeys
	Share      ShareSource
	Sessions   StatusSessions
	// Live answers for a running Worker; nil or [ErrNoLiveWorker] means none.
	Live LiveSource
	// StateDir is where the saved pause is read when no running Worker
	// answers; empty reads none.
	StateDir string
	// NewSessions is allow_new_sessions (on or off) from the settings: what
	// the next start enforces, reported when no Worker runs.
	NewSessions string
}

// StatusConnection is the connection part of the report.
type StatusConnection struct {
	// State is idle, connecting, connected or backoff for a live Worker, and
	// [ConnUnknown] or [ConnNone] otherwise.
	State string `json:"state"`
	// Live is true when a running Worker answered.
	Live bool `json:"live"`
	// Reason explains idle, unknown and none.
	Reason string `json:"reason,omitempty"`
	// LastError is the last connection failure, empty after a success.
	LastError   string         `json:"last_error,omitempty"`
	LastErrorAt *ops.Timestamp `json:"last_error_at,omitempty"`
	// ConnectedSince is set while connected.
	ConnectedSince *ops.Timestamp `json:"connected_since,omitempty"`
}

// StatusShare is the share part of the report.
type StatusShare struct {
	// Mode is all, none, select or later.
	Mode string `json:"mode"`
	// Configured is false until a share choice was saved.
	Configured bool `json:"configured"`
	// Sessions are the selected session names (used by mode select).
	Sessions []string `json:"sessions"`
}

// StatusTLS is the TLS path of a connection: the state (TLSPath*) and the
// text.
type StatusTLS struct {
	State string `json:"state"`
	Line  string `json:"line"`
}

// StatusReport is the content of `minion worker status`; `--json` prints it
// as is. Field names are part of the CLI contract.
type StatusReport struct {
	Version string `json:"version"`
	// Platform and Preview are set only on a platform that is a preview
	// (Windows, R98); the text report says "Windows (preview)" there.
	Platform string `json:"platform,omitempty"`
	Preview  bool   `json:"preview,omitempty"`
	Name     string `json:"name"`
	// Mode is [ModeSolo] or [ModeHub].
	Mode       string           `json:"mode"`
	HubURL     string           `json:"hub_url"`
	Connection StatusConnection `json:"connection"`
	// HubFingerprint is the pinned Hub key, empty when not paired.
	HubFingerprint string `json:"hub_fingerprint"`
	// WorkerFingerprint is the Worker's own key, empty before the key exists.
	WorkerFingerprint string `json:"worker_fingerprint"`
	// HubFingerprintWords and WorkerFingerprintWords are the same keys as
	// six words (v0.11.0).
	HubFingerprintWords    string `json:"hub_fingerprint_words,omitempty"`
	WorkerFingerprintWords string `json:"worker_fingerprint_words,omitempty"`
	// HubCheck is how the pinned Hub was checked (none, fingerprint, invite,
	// mismatch), HubCheckAt when, HubCheckAnchor the full fingerprint that
	// was checked; HubCheckText says it in words. Empty when not paired.
	HubCheck       string         `json:"hub_check,omitempty"`
	HubCheckAt     *ops.Timestamp `json:"hub_check_at,omitempty"`
	HubCheckAnchor string         `json:"hub_check_anchor,omitempty"`
	HubCheckText   string         `json:"hub_check_text,omitempty"`
	// Alarm is "hub_identity_changed" while the running Worker refuses the
	// root key the Hub answers with.
	Alarm string `json:"alarm,omitempty"`
	// TLS is the TLS path of the running Worker's latest connection.
	TLS *StatusTLS `json:"tls,omitempty"`
	// Paired says the Worker is enrolled with a Hub (a pin is stored).
	Paired bool `json:"paired"`
	// WorkerID is the id the Hub gave this Worker, empty until the first
	// welcome.
	WorkerID string `json:"worker_id"`
	// Approval is [ApprovalApproved], [ApprovalPending] or [ApprovalUnknown].
	Approval string `json:"approval"`
	// Owner is the Hub user the Worker belongs to, empty until the first
	// welcome.
	Owner string `json:"owner"`
	// PendingPairingCode is the code the Worker waits for, empty when there is
	// none or when it is not known (see PairingState).
	PendingPairingCode string `json:"pending_pairing_code"`
	// PairingState is [PairingPending], [PairingNone] or [PairingUnknown].
	// Only a running Worker knows it, so a report without a live answer says
	// unknown rather than "none".
	PairingState string      `json:"pairing_state"`
	Share        StatusShare `json:"share"`
	// PreviewOnly is the effective flag: the local setting or the Hub's
	// restriction. PreviewOnlyHub says the Hub contributes to it.
	PreviewOnly    bool `json:"preview_only"`
	PreviewOnlyHub bool `json:"preview_only_hub"`
	// Paused is the pause (minion worker pause): the running Worker's, else
	// the saved choice, which a start applies.
	Paused bool `json:"paused"`
	// NewSessions is allow_new_sessions as the running Worker enforces it (on
	// or off); "unknown" when a running Worker cannot confirm it; the saved
	// value when no Worker runs.
	NewSessions string `json:"new_sessions"`
	TmuxSocket  string `json:"tmux_socket"`
	// Sessions is the number of tmux sessions on the socket; nil when it
	// could not be counted.
	Sessions *int `json:"sessions"`
	// RunMode is how the running Worker runs: "foreground console",
	// "Windows service", "systemd service" or "launchd service"; empty when
	// no running Worker answered.
	RunMode string `json:"run_mode,omitempty"`
	// SessionHost is tmux, psmux or conpty; SessionsHidden and
	// SessionHostReason say whether new sessions are hidden from the person
	// and why. Only a running Worker reports them.
	SessionHost       string `json:"session_host,omitempty"`
	SessionsHidden    bool   `json:"sessions_hidden,omitempty"`
	SessionHostReason string `json:"session_host_reason,omitempty"`
	// SessionsListError is why the running Worker's latest look at its
	// sessions failed; empty when it worked.
	SessionsListError string `json:"sessions_list_error,omitempty"`
	// LogSinks is the state of the remote log sinks of the running Worker
	// (syslog, otel); empty when no running Worker answered.
	LogSinks []ops.LogSinkStatus `json:"log_sinks,omitempty"`
	// Problems lists what could not be read, in plain English.
	Problems []string `json:"problems,omitempty"`
}

// BuildStatus collects the report. It never returns an error: a source that
// fails is reported in Problems.
func BuildStatus(ctx context.Context, in StatusInputs) StatusReport {
	r := StatusReport{
		Version:    in.Version,
		Name:       in.Name,
		Mode:       ModeSolo,
		HubURL:     in.HubURL,
		TmuxSocket: in.TmuxSocket,
		Share:      StatusShare{Mode: string(store.ShareLater), Sessions: []string{}},

		PairingState: PairingUnknown,
		Approval:     ApprovalUnknown,
	}
	if in.HubURL != "" {
		r.Mode = ModeHub
	}
	if _, ok := previewPlatforms[in.Platform]; ok {
		r.Platform, r.Preview = in.Platform, true
	}
	problem := func(format string, a ...any) { r.Problems = append(r.Problems, fmt.Sprintf(format, a...)) }

	if in.Enrollment != nil {
		enr, ok, err := in.Enrollment.Load(ctx)
		switch {
		case err != nil:
			problem("cannot read the enrollment: %v", err)
		case ok:
			r.Paired = true
			r.Owner = enr.Owner
			r.WorkerID = enr.WorkerID
			r.PreviewOnlyHub = enr.Policy.PreviewOnly
			r.HubCheck = enr.Check()
			r.HubCheckText = hubCheckLine(enr)
			r.HubCheckAnchor = enr.HubCheckAnchor
			if !enr.HubCheckAt.IsZero() {
				t := ops.Timestamp(enr.HubCheckAt.UTC())
				r.HubCheckAt = &t
			}
			if fp, err := identity.FingerprintOf(enr.Pin.Pub); err == nil {
				r.HubFingerprint = fp.String()
				r.HubFingerprintWords = fp.Words()
			} else {
				problem("the pinned Hub key is invalid: %v", err)
			}
		}
	}
	if in.Keys != nil {
		switch k, err := in.Keys.LoadKey(); {
		case errors.Is(err, ErrNoWorkerKey):
		case err != nil:
			problem("cannot read the Worker key: %v", err)
		default:
			r.WorkerFingerprint = k.Fingerprint().String()
			r.WorkerFingerprintWords = k.Fingerprint().Words()
		}
	}
	if in.Share != nil {
		sel, err := in.Share.LoadShareContext(ctx)
		if err != nil {
			problem("cannot read the share selection: %v", err)
		} else {
			r.Share = StatusShare{Mode: string(sel.Mode), Configured: sel.Configured, Sessions: nonNil(sel.Sessions)}
		}
	}
	if in.Sessions != nil {
		sessions, err := in.Sessions.ListSessions(ctx)
		if err != nil {
			problem("cannot count the tmux sessions: %v", err)
		} else {
			n := len(sessions)
			r.Sessions = &n
		}
	}

	if in.StateDir != "" {
		paused, err := ReadPause(in.StateDir)
		if err != nil {
			problem("%v", err)
		}
		r.Paused = paused
	}
	r.NewSessions = orDash(in.NewSessions, NewSessionsUnknown)
	r.Connection = connectionOf(ctx, in, &r, problem)
	r.PreviewOnly = in.PreviewOnly || r.PreviewOnlyHub
	return r
}

// connectionOf fills the connection part and, from a live Worker, the fields
// that are fresher than the stored ones.
func connectionOf(ctx context.Context, in StatusInputs, r *StatusReport, problem func(string, ...any)) StatusConnection {
	var live *LiveStatus
	if in.Live != nil {
		switch ls, err := in.Live.LiveStatus(ctx); {
		case err == nil:
			live = &ls
		case errors.Is(err, ErrNoLiveWorker):
		default:
			problem("cannot ask the running Worker: %v", err)
			r.NewSessions = NewSessionsUnknown
		}
	}
	if live != nil {
		r.PendingPairingCode = live.PendingPairingCode
		switch {
		case live.PendingPairingCode != "":
			r.PairingState = PairingPending
		case live.PairingKnown:
			r.PairingState = PairingNone
		}
		r.PreviewOnlyHub = live.HubPreviewOnly
		if live.PauseKnown {
			r.Paused = live.Paused
		}
		r.NewSessions = orDash(live.NewSessions, NewSessionsUnknown)
		r.RunMode = live.RunMode
		r.SessionHost, r.SessionsHidden, r.SessionHostReason = live.Host.Kind, live.Host.Hidden, live.Host.Reason
		r.SessionsListError = live.ListError
		r.LogSinks = live.LogSinks
		r.Alarm = live.Connector.Alarm
		if live.Connector.TLSPath != "" {
			r.TLS = &StatusTLS{State: live.Connector.TLSPath, Line: live.Connector.TLSLine}
		}
		if r.Owner == "" && live.Connector.Welcome.Owner != "" {
			r.Owner = live.Connector.Welcome.Owner
		}
		if r.WorkerID == "" {
			r.WorkerID = live.Connector.Welcome.WorkerID
		}
		if live.Connector.State == ConnConnected && live.ApprovalKnown {
			r.Approval = ApprovalApproved
			if live.Connector.Welcome.Pending {
				r.Approval = ApprovalPending
			}
		}
		c := StatusConnection{
			State:     string(live.Connector.State),
			Live:      true,
			Reason:    live.Connector.Reason,
			LastError: live.Connector.LastError,
		}
		if c.State == "" {
			c.State = string(ConnIdle)
		}
		if !live.Connector.LastErrorAt.IsZero() && c.LastError != "" {
			t := ops.Timestamp(live.Connector.LastErrorAt.UTC())
			c.LastErrorAt = &t
		}
		if !live.Connector.ConnectedSince.IsZero() && live.Connector.State == ConnConnected {
			t := ops.Timestamp(live.Connector.ConnectedSince.UTC())
			c.ConnectedSince = &t
		}
		return c
	}
	if in.HubURL == "" {
		return StatusConnection{State: ConnNone, Reason: "no Hub URL is configured"}
	}
	return StatusConnection{
		State:  ConnUnknown,
		Reason: "the connection state is known only to the running Worker and is not available here",
	}
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// WriteJSON writes the report as indented JSON followed by a newline.
func (r StatusReport) WriteJSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}

// WriteText writes the report for a person: one labelled line per fact.
func (r StatusReport) WriteText(w io.Writer) error {
	rows := [][2]string{
		{"Version", r.Version},
		{"Name", r.Name},
		{"Mode", r.modeText()},
		{"Hub URL", orDash(r.HubURL, "none (Solo)")},
		{"Connection", r.connectionText()},
		{"Last error", r.lastErrorText()},
		{"Hub fingerprint", orDash(r.HubFingerprintWords, "none (not paired)")},
		{"Hub fingerprint (full)", orDash(r.HubFingerprint, "none (not paired)")},
		{"Hub check", orDash(r.HubCheckText, "none (not paired)")},
		{"TLS", r.tlsText()},
		{"Worker fingerprint", orDash(r.WorkerFingerprintWords, "none yet (created at the first pairing)")},
		{"Worker fingerprint (full)", orDash(r.WorkerFingerprint, "none yet (created at the first pairing)")},
		{"Owner", r.ownerText()},
		{"Worker id", orDash(r.WorkerID, "none yet (given at the first welcome)")},
		{"Approval", r.approvalText()},
		{"Pairing code", r.pairingText()},
		{"Share mode", r.shareText()},
		{"Preview only", r.previewText()},
		{"Paused", r.pausedText()},
		{"New sessions", r.newSessionsText()},
		{"Run mode", r.runModeText()},
		{"Session host", r.sessionHostText()},
		{"tmux socket", orDash(r.TmuxSocket, "none")},
		{"Sessions", r.sessionsText()},
	}
	for _, st := range r.LogSinks {
		rows = append(rows, [2]string{"Log export (" + st.Name + ")", logSinkText(st)})
	}
	if r.Alarm == AlarmHubIdentityChanged {
		for i, row := range rows {
			if row[0] == "Hub check" {
				rows = append(rows[:i+1], append([][2]string{{"Hub identity", "CHANGED - not connecting; see the Worker log"}}, rows[i+1:]...)...)
				break
			}
		}
	}
	if r.Preview {
		rows = append(rows[:1], append([][2]string{{"Platform", r.platformText()}}, rows[1:]...)...)
	}
	width := 0
	for _, row := range rows {
		if len(row[0]) > width {
			width = len(row[0])
		}
	}
	var b strings.Builder
	for _, row := range rows {
		// Values may come from the Hub (owner, worker id) or from rows an
		// older version stored: one line, no control characters.
		fmt.Fprintf(&b, "%-*s  %s\n", width+1, row[0]+":", proto.SafeText(row[1], 600))
	}
	for _, p := range r.Problems {
		fmt.Fprintf(&b, "%-*s  %s\n", width+1, "Problem:", proto.SafeText(p, 600))
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// logSinkText is one remote log sink for a person: off, or the destination
// and level with what was sent and dropped.
func logSinkText(st ops.LogSinkStatus) string {
	if !st.Enabled {
		if st.LastError != "" {
			return "off (" + st.LastError + ")"
		}
		return "off"
	}
	var dropped uint64
	for _, n := range st.Dropped {
		dropped += n
	}
	out := fmt.Sprintf("%s at %s and above; sent %d, queued %d, dropped %d", st.Destination, st.Level, st.Sent, st.Queued, dropped)
	if st.LastError != "" {
		out += "; last error: " + st.LastError
	}
	return out
}

// previewPlatforms maps the platforms that are a preview in release 1 to the
// name the report shows.
var previewPlatforms = map[string]string{"windows": "Windows"}

func (r StatusReport) platformText() string {
	name := previewPlatforms[r.Platform]
	if name == "" {
		name = r.Platform
	}
	return name + " (preview)"
}

func (r StatusReport) modeText() string {
	if r.Mode == ModeHub {
		return "Hub"
	}
	return "Solo"
}

func (r StatusReport) connectionText() string {
	c := r.Connection
	out := c.State
	switch {
	case c.State == string(ConnConnected) && c.ConnectedSince != nil:
		out += " (since " + c.ConnectedSince.Time().Format(time.RFC3339) + ")"
	case c.Reason != "":
		out += " (" + c.Reason + ")"
	}
	return out
}

// lastErrorText says "unknown" where the last error is live data that is not
// available: a Hub-mode report that no running Worker answered.
func (r StatusReport) lastErrorText() string {
	if r.Connection.LastError == "" && !r.Connection.Live && r.Mode == ModeHub {
		return "unknown"
	}
	return orDash(r.Connection.LastError, "none")
}

// pairingText says "unknown" where no running Worker could tell, and never
// turns missing data into "none pending".
func (r StatusReport) pairingText() string {
	switch r.PairingState {
	case PairingPending:
		return r.PendingPairingCode
	case PairingNone:
		return "none pending"
	}
	if r.Connection.Live {
		return "unknown (the running Worker does not report a pairing code)"
	}
	return "unknown (only a running Worker knows it)"
}

func (r StatusReport) approvalText() string {
	switch r.Approval {
	case ApprovalPending:
		return "pending (ask the owner or an admin to approve this Worker in the Hub)"
	case ApprovalApproved:
		return "approved"
	}
	return "unknown (only a connected Worker knows it)"
}

func (r StatusReport) ownerText() string {
	switch {
	case !r.Paired:
		return "not paired"
	case r.Owner == "":
		return "paired (the Hub has not named the owner yet)"
	}
	return "paired to " + r.Owner
}

// tlsText is the TLS path row: the running Worker's latest connection.
func (r StatusReport) tlsText() string {
	switch {
	case r.TLS != nil:
		return r.TLS.Line
	case r.Mode != ModeHub:
		return "none (Solo)"
	}
	return "known only to the running Worker"
}

func (r StatusReport) shareText() string {
	out := r.Share.Mode
	if !r.Share.Configured {
		out += " (not decided yet)"
	}
	if r.Share.Mode == string(store.ShareSelect) {
		out += " (" + orDash(strings.Join(r.Share.Sessions, ", "), "no session selected") + ")"
	}
	return out
}

func (r StatusReport) previewText() string {
	switch {
	case !r.PreviewOnly:
		return "no"
	case r.PreviewOnlyHub:
		return "yes (the Hub restricts this Worker)"
	}
	return "yes"
}

// NewSessionsUnknown is the value of StatusReport.NewSessions when a running
// Worker cannot confirm what it enforces.
const NewSessionsUnknown = "unknown"

func (r StatusReport) newSessionsText() string {
	switch r.NewSessions {
	case config.AllowNewSessionsOn:
		return "allowed"
	case config.AllowNewSessionsOff:
		return "blocked (allow_new_sessions off)"
	}
	return "unknown (the running Worker cannot confirm it)"
}

func (r StatusReport) pausedText() string {
	if r.Paused {
		return "yes (no Hub traffic; resume with: minion worker resume)"
	}
	return "no"
}

func (r StatusReport) sessionsText() string {
	out := "unknown"
	if r.Sessions != nil {
		out = fmt.Sprint(*r.Sessions)
	}
	if r.SessionsListError != "" {
		out += " (the running Worker could not list them at its latest look: " + r.SessionsListError + ")"
	}
	return out
}

func (r StatusReport) runModeText() string {
	if r.RunMode != "" {
		return r.RunMode
	}
	return "unknown (only a running Worker knows it)"
}

// sessionHostText is "psmux (visible): ..." or "ConPTY inside the Worker
// (hidden): ...", as the Hub's Worker page shows it.
func (r StatusReport) sessionHostText() string {
	if r.SessionHost == "" {
		return "unknown (only a running Worker knows it)"
	}
	label := r.SessionHost
	if label == "conpty" {
		label = "ConPTY inside the Worker"
	}
	vis := "visible"
	if r.SessionsHidden {
		vis = "hidden"
	}
	out := label + " (" + vis + ")"
	if reason := strings.TrimPrefix(strings.TrimPrefix(r.SessionHostReason, "ConPTY inside the Worker"), ": "); reason != "" {
		out += ": " + reason
	}
	return out
}

func orDash(v, empty string) string {
	if v == "" {
		return empty
	}
	return v
}

var (
	_ StatusEnrollment = (*Enrollments)(nil)
	_ StatusKeys       = (*Enrollments)(nil)
	_ StatusSessions   = term.SessionHost(nil)
	_ LiveSource       = LiveStatusSource{}
)
