package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/ops"
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
	// PairingKnown is true when the Worker can tell whether a pairing code is
	// pending. When it is false an empty PendingPairingCode means "not known",
	// not "none pending".
	PairingKnown bool
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
}

// LiveStatus implements [LiveSource].
func (s LiveStatusSource) LiveStatus(context.Context) (LiveStatus, error) {
	var out LiveStatus
	if s.Connector != nil {
		out.Connector = s.Connector.Status()
	}
	if s.Policy != nil {
		out.HubPreviewOnly = s.Policy.HubPreviewOnly()
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
	Paired            bool   `json:"paired"`
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
	PreviewOnly    bool   `json:"preview_only"`
	PreviewOnlyHub bool   `json:"preview_only_hub"`
	TmuxSocket     string `json:"tmux_socket"`
	// Sessions is the number of tmux sessions on the socket; nil when it
	// could not be counted.
	Sessions *int `json:"sessions"`
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
			r.PreviewOnlyHub = enr.Policy.PreviewOnly
			if fp, err := identity.FingerprintOf(enr.Pin.Pub); err == nil {
				r.HubFingerprint = fp.String()
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
		if r.Owner == "" && live.Connector.Welcome.Owner != "" {
			r.Owner = live.Connector.Welcome.Owner
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
		{"Hub fingerprint", orDash(r.HubFingerprint, "none (not paired)")},
		{"Worker fingerprint", orDash(r.WorkerFingerprint, "none yet (created at the first pairing)")},
		{"Owner", r.ownerText()},
		{"Pairing code", r.pairingText()},
		{"Share mode", r.shareText()},
		{"Preview only", r.previewText()},
		{"tmux socket", orDash(r.TmuxSocket, "none")},
		{"Sessions", r.sessionsText()},
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
		fmt.Fprintf(&b, "%-*s  %s\n", width+1, row[0]+":", row[1])
	}
	for _, p := range r.Problems {
		fmt.Fprintf(&b, "%-*s  %s\n", width+1, "Problem:", p)
	}
	_, err := io.WriteString(w, b.String())
	return err
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

func (r StatusReport) ownerText() string {
	switch {
	case !r.Paired:
		return "not paired"
	case r.Owner == "":
		return "paired (the Hub has not named the owner yet)"
	}
	return "paired to " + r.Owner
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

func (r StatusReport) sessionsText() string {
	if r.Sessions == nil {
		return "unknown"
	}
	return fmt.Sprint(*r.Sessions)
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
