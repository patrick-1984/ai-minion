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
	// Paused is whether the running Worker enforces a pause of its Hub
	// traffic; nil when it does not report it (a Worker without pause.v1).
	Paused *bool
	// NewSessionsBlocked is whether the running Worker refuses new sessions
	// (allow_new_sessions); nil when it does not report it (an older Worker).
	NewSessionsBlocked *bool
	// Instance, StateDir, Version and Capabilities identify the answering
	// Worker (tray W10a); an older Worker leaves Version and Capabilities
	// empty.
	Instance     string
	StateDir     string
	Version      string
	Capabilities []string
	// SessionsLostOnStop is how many sessions a stop would end; nil when
	// they cannot be counted. CountReported is false for an older Worker.
	SessionsLostOnStop *int
	CountReported      bool
}

// LiveElsewhereError is what a [LiveSource] returns when the running Worker
// of this user uses another state directory: it is not this profile's Worker,
// but the status names it (live_elsewhere) instead of reporting "stopped".
type LiveElsewhereError struct {
	StateDir string
	Instance string
}

func (e *LiveElsewhereError) Error() string {
	return "the running Worker of this user uses the state directory " + e.StateDir
}

// Is makes a LiveElsewhereError count as [ErrNoLiveWorker] for this profile.
func (e *LiveElsewhereError) Is(target error) bool { return target == ErrNoLiveWorker }

// LiveDeniedError is what a [LiveSource] returns when the control socket is
// not trusted (it belongs to another user, or its directory is open): a
// Worker may run, but this user cannot ask it.
type LiveDeniedError struct{ Err error }

func (e *LiveDeniedError) Error() string { return e.Err.Error() }
func (e *LiveDeniedError) Unwrap() error { return e.Err }

// Problem codes of [StatusReport.ProblemCodes].
const (
	// ProblemControlSocketDenied: the control socket is not trusted.
	ProblemControlSocketDenied = "control_socket_denied"
)

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
	// Pause is the pause of this Worker; nil reports none (pause unknown).
	Pause *PauseService
	// Counter counts the sessions a stop would end; nil reports none.
	Counter *SessionCounter
}

// LiveStatus implements [LiveSource].
func (s LiveStatusSource) LiveStatus(ctx context.Context) (LiveStatus, error) {
	var out LiveStatus
	if s.Connector != nil {
		out.Connector = s.Connector.Status()
		out.ApprovalKnown = true
	}
	if s.Policy != nil {
		out.HubPreviewOnly = s.Policy.HubPreviewOnly()
	}
	out.RunMode = s.RunMode
	if s.Pause != nil {
		paused := s.Pause.Paused()
		out.Paused = &paused
	}
	if s.Policy != nil {
		blocked, _ := s.Policy.NewSessionsBlocked()
		out.NewSessionsBlocked = &blocked
	}
	if s.Counter != nil {
		cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		if n, known, _ := s.Counter.CountLost(cctx); known {
			out.SessionsLostOnStop = &n
		}
		cancel()
		out.CountReported = true
	}
	// The local status shows the details the hello leaves out (a socket
	// path, a user name, the error itself): only this machine reads it.
	if s.Host != nil {
		out.Host = *s.Host
		out.Host.Reason = s.Host.localReason()
	}
	out.ListError = s.Health.LocalText()
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
	// Pause reads the saved pause (pause.json); nil means not available.
	Pause func() (PauseState, error)
	// NewSessions is the tighten-merged allow_new_sessions of the local
	// sources (flag, env, file) as this command resolves them; nil means not
	// available.
	NewSessions *config.Tightened
	// StateDir is the profile (state directory) this status is about, and
	// DefaultStateDir whether it is the user's default one; CLIVersion is the
	// version of the minion that builds the report (tray W10a).
	StateDir        string
	DefaultStateDir bool
	CLIVersion      string
	// Now is the clock of a timed pause; nil means time.Now.
	Now func() time.Time
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

// StatusNewSessions is the local block of new sessions created through
// Minion: Blocked is the tighten-only merge of the flag, the environment and
// the config file (Sources are the ones that block); InForce says whether the
// running Worker enforces exactly that (true when no Worker runs: it applies
// it at its start; null when a running Worker does not report it).
type StatusNewSessions struct {
	Blocked bool     `json:"blocked"`
	Sources []string `json:"sources"`
	InForce *bool    `json:"in_force"`
}

// StatusProfile is the state directory of a status report.
type StatusProfile struct {
	StateDir string `json:"state_dir"`
	Default  bool   `json:"default"`
}

// StatusLive is the running Worker of the profile.
type StatusLive struct {
	Running  bool   `json:"running"`
	Instance string `json:"instance,omitempty"`
	Version  string `json:"version,omitempty"`
	StateDir string `json:"state_dir,omitempty"`
}

// StatusLiveElsewhere is a running Worker of this user on another profile.
type StatusLiveElsewhere struct {
	StateDir string `json:"state_dir"`
	Instance string `json:"instance,omitempty"`
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
	PreviewOnly    bool   `json:"preview_only"`
	PreviewOnlyHub bool   `json:"preview_only_hub"`
	TmuxSocket     string `json:"tmux_socket"`
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
	// Paused is the saved pause of the Hub traffic (pause.json, tray W3a),
	// PausedUntil the end of a timed pause (RFC 3339, empty: until resumed).
	// PauseInForce says whether the running Worker enforces the saved state:
	// true when no Worker runs (it applies the file at its start) or the
	// running one reports the same state, false when it reports another, and
	// null for a saved pause that the running Worker cannot report (a Worker
	// without pause support).
	Paused       bool   `json:"paused"`
	PausedUntil  string `json:"paused_until"`
	PauseInForce *bool  `json:"pause_in_force"`
	// NewSessions is the local block of new sessions (allow_new_sessions,
	// tray W4a).
	NewSessions StatusNewSessions `json:"new_sessions"`
	// Profile is the state directory this status is about (tray W10a).
	Profile StatusProfile `json:"profile"`
	// Live says whether a Worker runs on this profile and which; LiveElsewhere
	// names a running Worker of this user on another state directory.
	Live          StatusLive           `json:"live"`
	LiveElsewhere *StatusLiveElsewhere `json:"live_elsewhere"`
	// CLIVersion is the version of this minion; Version above is the same,
	// Live.Version the daemon's.
	CLIVersion string `json:"cli_version"`
	// Capabilities are the local features of the running Worker (for example
	// pause.v1); empty when none runs or it is too old to say.
	Capabilities []string `json:"capabilities"`
	// ProblemCodes are machine-readable codes of some Problems.
	ProblemCodes []string `json:"problem_codes,omitempty"`
	// SessionsLostOnStop is how many sessions a stop of the running Worker
	// would end (ConPTY; 0 for tmux and psmux, whose sessions outlive it);
	// null when they cannot be counted or no Worker runs.
	SessionsLostOnStop *int `json:"sessions_lost_on_stop"`
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

		Profile:      StatusProfile{StateDir: in.StateDir, Default: in.DefaultStateDir},
		CLIVersion:   in.CLIVersion,
		Capabilities: []string{},
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

	var live *LiveStatus
	r.Connection, live = connectionOf(ctx, in, &r, problem)
	r.PreviewOnly = in.PreviewOnly || r.PreviewOnlyHub
	pauseOf(in, &r, live, problem)
	newSessionsOf(in, &r, live)
	return r
}

// newSessionsOf fills the new-session block from the resolved setting and
// the live answer.
func newSessionsOf(in StatusInputs, r *StatusReport, live *LiveStatus) {
	r.NewSessions.Sources = []string{}
	if in.NewSessions != nil {
		r.NewSessions.Blocked = in.NewSessions.Blocked
		r.NewSessions.Sources = sourceNames(in.NewSessions.Sources)
	}
	inForce := true
	switch {
	case live == nil:
	case live.NewSessionsBlocked == nil:
		// An older Worker cannot block: in force exactly when nothing is.
		inForce = !r.NewSessions.Blocked
	default:
		inForce = *live.NewSessionsBlocked == r.NewSessions.Blocked
	}
	r.NewSessions.InForce = &inForce
}

// pauseOf fills the pause fields from the saved state and the live answer.
func pauseOf(in StatusInputs, r *StatusReport, live *LiveStatus, problem func(string, ...any)) {
	var saved PauseState
	if in.Pause != nil {
		p, err := in.Pause()
		if err != nil {
			problem("cannot read the pause: %v", err)
			return
		}
		saved = p
	}
	now := time.Now
	if in.Now != nil {
		now = in.Now
	}
	r.Paused = saved.Active(now())
	if r.Paused && !saved.Until.IsZero() {
		r.PausedUntil = saved.Until.UTC().Format(time.RFC3339)
	}
	inForce := true
	switch {
	case live == nil:
	case live.Paused == nil:
		// A Worker without pause support never pauses: in force exactly
		// when there is nothing to enforce.
		if r.Paused {
			r.PauseInForce = nil
			return
		}
	default:
		inForce = *live.Paused == r.Paused
	}
	r.PauseInForce = &inForce
}

// connectionOf fills the connection part and, from a live Worker, the fields
// that are fresher than the stored ones.
func connectionOf(ctx context.Context, in StatusInputs, r *StatusReport, problem func(string, ...any)) (StatusConnection, *LiveStatus) {
	var live *LiveStatus
	if in.Live != nil {
		ls, err := in.Live.LiveStatus(ctx)
		var elsewhere *LiveElsewhereError
		var denied *LiveDeniedError
		switch {
		case err == nil:
			live = &ls
		case errors.As(err, &elsewhere):
			r.LiveElsewhere = &StatusLiveElsewhere{StateDir: elsewhere.StateDir, Instance: elsewhere.Instance}
		case errors.Is(err, ErrNoLiveWorker):
		case errors.As(err, &denied):
			problem("cannot ask the running Worker: %v", err)
			r.ProblemCodes = append(r.ProblemCodes, ProblemControlSocketDenied)
		default:
			problem("cannot ask the running Worker: %v", err)
		}
	}
	if live != nil {
		r.Live = StatusLive{Running: true, Instance: live.Instance, Version: live.Version, StateDir: live.StateDir}
		if live.StateDir == "" {
			r.Live.StateDir = in.StateDir
		}
		r.Capabilities = append(r.Capabilities, live.Capabilities...)
		r.SessionsLostOnStop = live.SessionsLostOnStop
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
		r.RunMode = live.RunMode
		r.SessionHost, r.SessionsHidden, r.SessionHostReason = live.Host.Kind, live.Host.Hidden, live.Host.Reason
		r.SessionsListError = live.ListError
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
		return c, live
	}
	if in.HubURL == "" {
		return StatusConnection{State: ConnNone, Reason: "no Hub URL is configured"}, nil
	}
	return StatusConnection{
		State:  ConnUnknown,
		Reason: "the connection state is known only to the running Worker and is not available here",
	}, nil
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
		{"Run mode", r.runModeText()},
		{"Session host", r.sessionHostText()},
		{"tmux socket", orDash(r.TmuxSocket, "none")},
		{"Sessions", r.sessionsText()},
	}
	if r.Alarm == AlarmHubIdentityChanged {
		for i, row := range rows {
			if row[0] == "Hub check" {
				rows = append(rows[:i+1], append([][2]string{{"Hub identity", "CHANGED - not connecting; see the Worker log"}}, rows[i+1:]...)...)
				break
			}
		}
	}
	if r.NewSessions.Blocked {
		for i, row := range rows {
			if row[0] == "Preview only" {
				rows = append(rows[:i+1], append([][2]string{{"New sessions", r.newSessionsText()}}, rows[i+1:]...)...)
				break
			}
		}
	}
	if r.Paused {
		for i, row := range rows {
			if row[0] == "Connection" {
				rows = append(rows[:i+1], append([][2]string{{"Paused", r.pauseText()}}, rows[i+1:]...)...)
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

// previewPlatforms maps the platforms that are a preview in release 1 to the
// name the report shows.
var previewPlatforms = map[string]string{"windows": "Windows"}

func (r StatusReport) newSessionsText() string {
	t := "blocked (allow_new_sessions off, set by " + strings.Join(r.NewSessions.Sources, ", ") + ")"
	if r.NewSessions.InForce != nil && !*r.NewSessions.InForce {
		t += "; the running Worker does not enforce it yet"
	}
	return t
}

func (r StatusReport) pauseText() string {
	t := "yes, no Hub traffic"
	if r.PausedUntil != "" {
		t += " until " + r.PausedUntil
	} else {
		t += " until resumed (minion worker resume)"
	}
	if r.PauseInForce != nil && !*r.PauseInForce {
		t += "; the running Worker does not enforce it yet"
	} else if r.PauseInForce == nil {
		t += "; the running Worker does not support pause"
	}
	return t
}

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
