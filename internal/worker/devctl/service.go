package devctl

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"runtime"
	"sync"
	"time"
)

// Share modes as the Worker state names them (store.ShareMode values).
const (
	ShareNone = "none"
)

// Disable reasons (classes only).
const (
	ReasonLocal = "local"
	ReasonHub   = "hub"
	ReasonShare = "share"
)

// Config assembles a [Service].
type Config struct {
	// GOOS is the system the Worker runs on; "" is runtime.GOOS. Device
	// control works only on android.
	GOOS string
	// Bridge is the client of the app's bridge socket; nil means none.
	Bridge *Client
	// ShareMode returns the share mode in force (none, all, select, later).
	// Device control needs none: a dedicated, unshared Worker.
	ShareMode func(ctx context.Context) (string, error)
	// Panes maps the pane pids of the session host to their sessions.
	Panes func(ctx context.Context) (map[int]string, error)
	// PPID reads a parent pid; nil is [ProcPPID].
	PPID func(int) (int, error)
	// Audit receives one entry per operation; nil keeps nothing.
	Audit *AuditLog
	Now   func() time.Time
	Log   *slog.Logger
}

// Service decides the policy of device control, relays to the bridge, maps the
// caller to its session and audits every operation. It is safe for concurrent
// use.
type Service struct {
	c Config

	mu      sync.Mutex
	streams map[int]context.CancelCauseFunc
	nextID  int
	lastOK  time.Time
	lastErr string
}

// New returns a service for c.
func New(c Config) *Service {
	if c.GOOS == "" {
		c.GOOS = runtime.GOOS
	}
	if c.PPID == nil {
		c.PPID = ProcPPID
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.Log == nil {
		c.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if c.Audit == nil {
		c.Audit = NewAuditLog("", nil)
	}
	return &Service{c: c, streams: map[int]context.CancelCauseFunc{}}
}

// Report is the device_control block of `worker status --json` and the
// result of adb_status.
type Report struct {
	// Available is true on Android when the app's bridge answers and the app
	// build has the feature.
	Available bool `json:"available"`
	// Enabled is the owner's on-device choice as the app reports it.
	Enabled bool `json:"enabled"`
	// State is the bridge state, or OFF when the bridge is not reachable.
	State         string `json:"state"`
	Port          int    `json:"port,omitempty"`
	KeyStorage    string `json:"key_storage,omitempty"`
	ActiveStreams int    `json:"active_streams"`
	LastOK        string `json:"last_ok,omitempty"`
	LastError     string `json:"last_error,omitempty"`
	// Shared is true when the Worker shares sessions: device control is then
	// refused (and disabled when it was on).
	Shared bool `json:"shared"`
	// Reason is the honest text why device control cannot be used now, ""
	// when it is ready.
	Reason string `json:"reason,omitempty"`
}

// Report returns the status without auditing it.
func (s *Service) Report(ctx context.Context) Report {
	r := Report{State: StateOff}
	shared, _ := s.shared(ctx)
	r.Shared = shared
	s.mu.Lock()
	r.ActiveStreams = len(s.streams)
	if !s.lastOK.IsZero() {
		r.LastOK = s.lastOK.UTC().Format(time.RFC3339)
	}
	r.LastError = s.lastErr
	s.mu.Unlock()
	if s.c.GOOS != "android" {
		r.Reason = Reason(ErrUnsupportedOS)
		return r
	}
	if s.c.Bridge == nil {
		r.Reason = Reason(ErrBridgeDown)
		return r
	}
	st, err := s.c.Bridge.Status(ctx)
	if err != nil {
		r.Reason = err.Error()
		return r
	}
	r.Available = st.FeatureEnabled
	r.Enabled = st.FeatureEnabled && st.Enabled
	r.State, r.Port, r.KeyStorage = st.State, st.Port, st.KeyStorage
	if r.State == "" {
		r.State = StateOff
	}
	if st.LastError != "" && r.LastError == "" {
		r.LastError = st.LastError
	}
	if r.LastOK == "" {
		r.LastOK = st.LastOK
	}
	switch {
	case !st.FeatureEnabled:
		r.Reason = Reason(ErrNotAvailable)
	case !st.Enabled:
		r.Reason = Reason(ErrDisabled)
	case shared:
		r.Reason = Reason(ErrShared)
	default:
		if code := stateCode(st.State); code != "" {
			r.Reason = Reason(code)
		}
	}
	return r
}

// Status is the audited status of a caller (adb_status, `minion adb status`).
func (s *Service) Status(ctx context.Context) Report {
	start := s.c.Now()
	r := s.Report(ctx)
	s.audit(ctx, AuditStatus, ResultOK, start)
	return r
}

// Recent returns the last audit entries for the Hub panel.
func (s *Service) Recent() []Entry { return s.c.Audit.Recent() }

// stateCode maps a bridge state that is not READY to the error code a call
// gets.
func stateCode(state string) string {
	switch state {
	case StateReady:
		return ""
	case StateUnpaired, StatePaired:
		return ErrNotPaired
	case StateWaitingWiFi:
		return ErrNeedsWiFi
	case StateEndpointUnverified:
		return ErrEndpointUnverified
	}
	return ErrDisabled
}

func (s *Service) shared(ctx context.Context) (bool, error) {
	if s.c.ShareMode == nil {
		return true, errors.New("devctl: no share state")
	}
	m, err := s.c.ShareMode(ctx)
	if err != nil {
		return true, err // fail closed
	}
	return m != ShareNone, nil
}

// check is the policy at call time (DESIGN 4.6): Android, the app's bridge,
// the build flag, enabled, unshared, READY. A shared Worker with device
// control on is disabled here (fail closed).
func (s *Service) check(ctx context.Context) error {
	if s.c.GOOS != "android" {
		return &Error{Code: ErrUnsupportedOS}
	}
	if s.c.Bridge == nil {
		return &Error{Code: ErrBridgeDown}
	}
	st, err := s.c.Bridge.Status(ctx)
	if err != nil {
		return err
	}
	if !st.FeatureEnabled {
		return &Error{Code: ErrNotAvailable}
	}
	if !st.Enabled {
		return &Error{Code: ErrDisabled}
	}
	if shared, _ := s.shared(ctx); shared {
		_ = s.disable(ctx, ReasonShare) // refused either way; a failure is logged
		return &Error{Code: ErrShared}
	}
	if code := stateCode(st.State); code != "" {
		return &Error{Code: code}
	}
	return nil
}

// Run runs shell, screencap or input for the caller in ctx. It returns the
// remote exit code; a Minion-side failure is an *Error.
func (s *Service) Run(ctx context.Context, op, command string, timeout time.Duration, sink Sink) (int, error) {
	start := s.c.Now()
	auditOp := map[string]string{OpShell: AuditShell, OpScreencap: AuditScreencap, OpInput: AuditInput}[op]
	if auditOp == "" {
		return 0, &Error{Code: ErrBadRequest}
	}
	if err := s.check(ctx); err != nil {
		s.fail(ctx, auditOp, err, start)
		return 0, err
	}
	ctx, id := s.track(ctx)
	defer s.untrack(id)
	code, err := s.c.Bridge.Run(ctx, op, command, timeout, sink)
	if err != nil {
		if cause := context.Cause(ctx); cause != nil && errors.Is(cause, errDisabled) {
			err = &Error{Code: ErrDisabled, Message: "device control disabled"}
		}
		s.fail(ctx, auditOp, err, start)
		return 0, err
	}
	s.mu.Lock()
	s.lastOK = s.c.Now()
	s.mu.Unlock()
	s.audit(ctx, auditOp, ResultExit(code), start)
	return code, nil
}

var errDisabled = errors.New("device control disabled")

func (s *Service) track(ctx context.Context) (context.Context, int) {
	ctx, cancel := context.WithCancelCause(ctx)
	s.mu.Lock()
	s.nextID++
	id := s.nextID
	s.streams[id] = cancel
	s.mu.Unlock()
	return ctx, id
}

func (s *Service) untrack(id int) {
	s.mu.Lock()
	if cancel, ok := s.streams[id]; ok {
		cancel(nil)
		delete(s.streams, id)
	}
	s.mu.Unlock()
}

// closeStreams ends every open stream at once: its caller gets "device control
// disabled".
func (s *Service) closeStreams() {
	s.mu.Lock()
	for id, cancel := range s.streams {
		cancel(errDisabled)
		delete(s.streams, id)
	}
	s.mu.Unlock()
}

// Disable closes every stream and asks the app to disable device control. The
// Hub can disable, never enable.
func (s *Service) Disable(ctx context.Context, reason string) error {
	start := s.c.Now()
	err := s.disable(ctx, reason)
	if err != nil {
		s.fail(ctx, AuditDisable, err, start)
		return err
	}
	s.audit(ctx, AuditDisable, ResultOK, start)
	return nil
}

func (s *Service) disable(ctx context.Context, reason string) error {
	s.closeStreams()
	if s.c.GOOS != "android" {
		return nil // nothing to disable
	}
	if s.c.Bridge == nil {
		return &Error{Code: ErrBridgeDown}
	}
	if err := s.c.Bridge.Disable(ctx, reason); err != nil {
		s.c.Log.Warn("device control could not be disabled in the app", "reason", reason, "code", CodeOf(err))
		return err
	}
	s.c.Log.Info("device control disabled", "reason", reason)
	return nil
}

// ErrShareRefused is returned by [Service.GuardShare].
var ErrShareRefused = errors.New("device control is on: it needs a dedicated, unshared Worker; disable device control in the Minion app (or on the Hub) before sharing")

// GuardShare refuses a change to a share mode other than none while device
// control is enabled. A bridge that cannot be asked does not refuse (the
// policy check of every call and [Service.ShareChanged] still fail closed).
func (s *Service) GuardShare(ctx context.Context, mode string) error {
	if mode == ShareNone || s.c.GOOS != "android" || s.c.Bridge == nil {
		return nil
	}
	st, err := s.c.Bridge.Status(ctx)
	if err != nil {
		return nil
	}
	if st.FeatureEnabled && st.Enabled {
		return ErrShareRefused
	}
	return nil
}

// ShareChanged is told the share mode after every committed change: anything
// but none disables device control and closes its streams.
func (s *Service) ShareChanged(ctx context.Context, mode string) {
	if mode == ShareNone {
		return
	}
	s.mu.Lock()
	open := len(s.streams) > 0
	s.mu.Unlock()
	if !open && (s.c.GOOS != "android" || s.c.Bridge == nil) {
		return
	}
	start := s.c.Now()
	if err := s.disable(ctx, ReasonShare); err == nil {
		s.audit(ctx, AuditDisable, ResultOK, start)
	}
}

// Watch checks the share mode every interval until ctx ends and disables
// device control when the Worker became shared by a path that did not go
// through the guard (for example `worker config share all` while the bridge
// could not be asked).
func (s *Service) Watch(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		shared, _ := s.shared(ctx)
		if !shared || s.c.GOOS != "android" || s.c.Bridge == nil {
			continue
		}
		st, err := s.c.Bridge.Status(ctx)
		if err != nil || !st.Enabled {
			continue
		}
		start := s.c.Now()
		if s.disable(ctx, ReasonShare) == nil {
			s.audit(ctx, AuditDisable, ResultOK, start)
		}
	}
}

func (s *Service) fail(ctx context.Context, op string, err error, start time.Time) {
	code := CodeOf(err)
	if code == "" {
		if errors.Is(err, context.Canceled) {
			code = "cancelled"
		} else {
			code = ErrInternal
		}
	}
	s.mu.Lock()
	s.lastErr = code
	s.mu.Unlock()
	var res string
	switch code {
	case ErrEndpointUnverified:
		res = ResultDenied("provenance")
	case ErrShared, ErrDisabled, ErrNotAvailable, ErrUnsupportedOS, ErrBadToken:
		res = ResultDenied(code)
	default:
		res = ResultError(code)
	}
	s.audit(ctx, op, res, start)
}

func (s *Service) audit(ctx context.Context, op, result string, start time.Time) {
	e := Entry{Op: op, Result: result, Transport: TransportTLSLoopback,
		Time: start.UTC().Format(time.RFC3339Nano), DurationMS: s.c.Now().Sub(start).Milliseconds(),
		Actor: ActorLocal, Session: SessionUnknown}
	if c, ok := CallerFrom(ctx); ok {
		if c.Actor != "" {
			e.Actor = c.Actor
		}
		if c.PID > 0 && s.c.Panes != nil {
			if panes, err := s.c.Panes(context.WithoutCancel(ctx)); err == nil {
				e.Session = MapSession(c.PID, s.c.PPID, panes)
			}
		}
	}
	if err := s.c.Audit.Record(e); err != nil {
		s.c.Log.Warn("device control audit could not be written", "error", err)
	}
}
