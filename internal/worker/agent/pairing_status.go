package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/worker/localapi"
)

// FeaturePairingStatus is the local capability of a Worker whose status
// carries the pairing code it shows, with its attempt and expiry (tray W8a).
// A client that finds it may read the code from "minion worker status --json"
// instead of from the console.
const FeaturePairingStatus = "pairing_status.v1"

// PendingPairing is the pairing code a Worker shows and waits for.
type PendingPairing struct {
	// Code is the pairing code, for example MN-K7Q4-XY12. It is a secret
	// that is shown to the human who owns the Worker (AD-20); the local
	// status sends it to the OS user that owns the control socket only.
	Code string
	// Attempt identifies this code: it changes whenever the code changes.
	Attempt string
	// ExpiresAt is when the code stops being valid.
	ExpiresAt time.Time
}

// PairingStatus is the status source of the Worker's own pairing (the
// auto-pairing of the connector module). The [Pairer] feeds it through its
// OnCode and OnDone hooks, and the local socket reads it for the status.
// The zero value is not usable; use [NewPairingStatus].
type PairingStatus struct {
	mu  sync.Mutex
	cur PendingPairing
	now func() time.Time
	// run is the pairing run in progress (autoPair), nil between runs.
	run *pairingRun
	// cancelledFor is the Hub address whose pairing the owner cancelled in
	// this process: no new run starts for it (tray W8b).
	cancelledFor string
}

// pairingRun is one run of the Worker's own pairing: its cancel and the
// channel that is closed when it has returned.
type pairingRun struct {
	cancel context.CancelCauseFunc
	done   chan struct{}
	hubURL string
}

// NewPairingStatus returns a source with no code pending.
func NewPairingStatus() *PairingStatus {
	return &PairingStatus{now: time.Now}
}

// OnCode records the code that is shown now. An empty code means the code
// shown before was retired and no new one is shown yet.
func (s *PairingStatus) OnCode(code, attempt string, expires time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if code == "" {
		s.cur = PendingPairing{}
		return
	}
	s.cur = PendingPairing{Code: code, Attempt: attempt, ExpiresAt: expires}
}

// OnDone records that the pairing run ended: paired, stopped or failed.
func (s *PairingStatus) OnDone() {
	s.mu.Lock()
	s.cur = PendingPairing{}
	s.mu.Unlock()
}

// Pending returns the code that is shown and still valid, if any. A code
// whose expiry passed is not reported, even before the pairer replaced it.
func (s *PairingStatus) Pending() (PendingPairing, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cur.Code == "" {
		return PendingPairing{}, false
	}
	if !s.cur.ExpiresAt.IsZero() && !s.now().Before(s.cur.ExpiresAt) {
		return PendingPairing{}, false
	}
	return s.cur, true
}

// Cancel pairing (tray tickets v3, C9 and W8b): "minion worker pair cancel
// --attempt A" asks the running Worker to stop the pairing whose code has the
// attempt id A. It is its own action, separate from leaving the Hub.

const (
	// OpLocalPairCancel cancels the Worker's own pairing (local-only).
	OpLocalPairCancel = "worker_pair_cancel"
	// FeaturePairCancel is the local capability of [OpLocalPairCancel].
	FeaturePairCancel = "pair_cancel.v1"
	// PairCancelFile records in the state directory that the owner cancelled
	// the pairing with a Hub address: the Worker does not pair by itself with
	// that address again, also not after a restart, until a new
	// "minion worker join <url> --background" (or a reset) removes it.
	PairCancelFile = "pair-cancel.json"
)

// errPairingCancelled is the cause of the context of a pairing run that the
// owner cancelled. A run that the Hub completed meanwhile does not commit
// its enrollment when it sees it ([Pairer.finish]).
var errPairingCancelled = errors.New("the pairing was cancelled on this Worker")

// LocalPairCancelArgs are the arguments of [OpLocalPairCancel].
type LocalPairCancelArgs struct {
	// Attempt is the attempt id of the code that is cancelled, as the status
	// showed it (pairing.attempt). Required.
	Attempt string `json:"attempt"`
	// Instance, when set, must be the running Worker's instance.
	Instance string `json:"instance,omitempty"`
}

// PairCancelResult is the reply of [OpLocalPairCancel].
type PairCancelResult struct {
	Cancelled bool   `json:"cancelled"`
	Attempt   string `json:"attempt"`
	// ResumeDeleted says a pairing resume record of the code was deleted.
	ResumeDeleted bool `json:"resume_deleted"`
	// Cleared are the config keys removed because a background join created
	// them (C11); Kept are the Hub keys that stay (set by someone else, or
	// changed since). Both sorted.
	Cleared []string `json:"cleared,omitempty"`
	Kept    []string `json:"kept,omitempty"`
}

// PairCancel is the content of [PairCancelFile].
type PairCancel struct {
	Version int       `json:"version"`
	Server  string    `json:"server"`
	Attempt string    `json:"attempt,omitempty"`
	At      time.Time `json:"at"`
}

// ReadPairCancel reads the cancel record of stateDir; ok is false when there
// is none.
func ReadPairCancel(stateDir string) (rec PairCancel, ok bool, err error) {
	b, err := os.ReadFile(filepath.Join(stateDir, PairCancelFile))
	if errors.Is(err, fs.ErrNotExist) {
		return PairCancel{}, false, nil
	}
	if err != nil {
		return PairCancel{}, false, fmt.Errorf("read %s: %w", PairCancelFile, err)
	}
	if err := json.Unmarshal(b, &rec); err != nil {
		return PairCancel{}, false, fmt.Errorf("the pairing cancel record %s is not valid: %w", filepath.Join(stateDir, PairCancelFile), err)
	}
	return rec, true, nil
}

// RemovePairCancel removes the cancel record; a missing one is fine.
func RemovePairCancel(stateDir string) error {
	if err := os.Remove(filepath.Join(stateDir, PairCancelFile)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove %s: %w", PairCancelFile, err)
	}
	return nil
}

// pairingCancelledFor reports whether the owner cancelled the pairing with
// hubURL. An unreadable record counts as cancelled: the Worker never pairs
// against the owner's last word.
func pairingCancelledFor(stateDir, hubURL string) bool {
	if stateDir == "" {
		return false
	}
	rec, ok, err := ReadPairCancel(stateDir)
	if err != nil {
		return true
	}
	return ok && rec.Server == hubURL
}

// track registers a pairing run with hubURL and returns its context, which
// [PairingStatus.cancelAttempt] ends, and the function that ends the run.
func (s *PairingStatus) track(ctx context.Context, hubURL string) (context.Context, func()) {
	cctx, cancel := context.WithCancelCause(ctx)
	r := &pairingRun{cancel: cancel, done: make(chan struct{}), hubURL: hubURL}
	s.mu.Lock()
	s.run = r
	s.mu.Unlock()
	return cctx, func() {
		s.mu.Lock()
		if s.run == r {
			s.run = nil
		}
		s.mu.Unlock()
		cancel(nil)
		close(r.done)
	}
}

// cancelled reports whether the owner cancelled the pairing with hubURL in
// this process.
func (s *PairingStatus) cancelled(hubURL string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cancelledFor != "" && s.cancelledFor == hubURL
}

// cancelAttempt ends the run whose current code has the attempt id attempt,
// atomically with the check: ok is false when no such code is shown (another
// code replaced it, the run ended, or it never existed). It returns the code
// it cancelled, the run's Hub address and the channel closed when the run has
// returned.
func (s *PairingStatus) cancelAttempt(attempt string) (p PendingPairing, hubURL string, done <-chan struct{}, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur := s.cur
	if attempt == "" || cur.Code == "" || cur.Attempt != attempt || s.run == nil ||
		(!cur.ExpiresAt.IsZero() && !s.now().Before(cur.ExpiresAt)) {
		return PendingPairing{}, "", nil, false
	}
	s.run.cancel(errPairingCancelled)
	s.cancelledFor = s.run.hubURL
	return cur, s.run.hubURL, s.run.done, true
}

// pairCancelWait bounds the wait for the cancelled run to return.
const pairCancelWait = 15 * time.Second

// pairCancelOp serves [OpLocalPairCancel]. Under the enrollment lock (the
// one a pairing holds while it commits) it checks that the Worker is still not
// paired and that attempt is the code shown now, ends that run and records
// the cancel; then it deletes the code's resume record and removes the Hub
// settings that a background join created and nobody changed since (C11).
// The Worker key and the share choice are kept. A Worker that was paired
// meanwhile, or an attempt that is no longer current, is refused with
// conflict/state_changed and nothing is changed.
func (l localExecutor) pairCancelOp(ctx context.Context, rq localapi.Request) (json.RawMessage, error) {
	if l.pairing == nil || l.enrollments == nil {
		return nil, apiErr(proto.CodeUnsupportedOp, "", "this Worker does not pair by itself, so there is no pairing to cancel")
	}
	var args LocalPairCancelArgs
	if len(rq.Args) > 0 {
		if err := json.Unmarshal(rq.Args, &args); err != nil {
			return nil, apiErr(proto.CodeInvalidArgument, "", "invalid pairing cancel request")
		}
	}
	if args.Attempt == "" {
		return nil, apiErr(proto.CodeInvalidArgument, "", "give the pairing attempt to cancel (pairing.attempt of minion worker status --json)")
	}
	if args.Instance != "" && args.Instance != l.instance {
		return nil, apiErr(proto.CodeConflict, "state_changed", "the Worker restarted since its status was read; nothing was changed")
	}
	res, pend, done, err := l.cancelPairingLocked(ctx, args.Attempt)
	if err != nil {
		return nil, err
	}
	// The run returns without committing (finish sees the cause); only then
	// is its resume record deleted, so that an attempt in flight cannot write
	// it again afterwards.
	select {
	case <-done:
	case <-ctx.Done():
	case <-time.After(pairCancelWait):
		l.log.Warn("the cancelled pairing did not stop in time; its resume record is deleted anyway")
	}
	if pc, perr := identity.ParsePairingCode(pend.Code); perr == nil {
		deleted, derr := l.enrollments.deletePairResumeOf(context.WithoutCancel(ctx), pc.PairingID(), pend.Code)
		if derr != nil {
			return nil, fmt.Errorf("the pairing was cancelled, but its resume record was not deleted: %w", derr)
		}
		res.ResumeDeleted = deleted
	}
	l.log.Info("the pairing was cancelled by the owner; this Worker does not pair by itself with this Hub address again "+
		"until minion worker join <url> --background", "attempt", args.Attempt)
	return json.Marshal(res)
}

// cancelPairingLocked is the part of [localExecutor.pairCancelOp] that runs
// under the enrollment lock.
func (l localExecutor) cancelPairingLocked(ctx context.Context, attempt string) (PairCancelResult, PendingPairing, <-chan struct{}, error) {
	res := PairCancelResult{Attempt: attempt}
	release, err := l.enrollments.lockEnroll(ctx)
	if err != nil {
		return res, PendingPairing{}, nil, err
	}
	defer release()
	if _, enrolled, err := l.enrollments.Load(ctx); err != nil {
		return res, PendingPairing{}, nil, fmt.Errorf("read the enrollment: %w", err)
	} else if enrolled {
		return res, PendingPairing{}, nil, apiErr(proto.CodeConflict, "state_changed",
			"this Worker was paired meanwhile, so there is no pairing to cancel; nothing was changed (leaving the Hub is a separate action: minion worker leave)")
	}
	pend, hubURL, done, ok := l.pairing.cancelAttempt(attempt)
	if !ok {
		return res, PendingPairing{}, nil, apiErr(proto.CodeConflict, "state_changed",
			"the pairing attempt "+attempt+" is no longer the one shown (a new code replaced it, or the pairing ended); nothing was changed")
	}
	res.Cancelled = true
	// The run is cancelled: from here on the cancel stands, also when a
	// file below cannot be written (the error says so).
	if err := writeStateJSON(l.stateDir, PairCancelFile, ".pair-cancel-*.tmp",
		PairCancel{Version: 1, Server: hubURL, Attempt: attempt, At: time.Now().UTC()}); err != nil {
		return res, pend, done, fmt.Errorf("the pairing was stopped, but the cancel was not saved (it may start again at the next start): %w", err)
	}
	if res.Cleared, res.Kept, err = clearBackgroundJoinConfig(l.stateDir); err != nil {
		return res, pend, done, fmt.Errorf("the pairing was cancelled, but the Hub settings of the background join were not removed: %w", err)
	}
	return res, pend, done, nil
}

// clearBackgroundJoinConfig removes from the config file of stateDir the
// server and hub_fingerprint values that the join record (C11) says a
// background join created, each only while it still has that value, and
// pairing_display=status that such a join set. Values provisioned before the
// join, or edited after it, are kept and reported. The join record goes once
// something was removed. It holds the config lock.
func clearBackgroundJoinConfig(stateDir string) (cleared, kept []string, err error) {
	release, err := lockConfig(filepath.Join(stateDir, configLockName))
	if err != nil {
		return nil, nil, err
	}
	defer release()
	f, err := config.LoadFile(filepath.Join(stateDir, WorkerConfigFile))
	if err != nil {
		return nil, nil, err
	}
	// An unreadable record proves nothing about who set the values: kept.
	pin, ok, perr := ReadJoinPin(stateDir)
	ok = ok && perr == nil && pin.Source == JoinPinSourceBackground
	for _, k := range []struct {
		key     string
		created func(string) bool
	}{
		{config.KeyServer, pin.CreatedServer},
		{config.KeyHubFingerprint, pin.CreatedFingerprint},
		{config.KeyPairingDisplay, func(v string) bool { return v == config.PairingDisplayStatus }},
	} {
		v, has, lerr := f.Lookup(k.key)
		if lerr != nil {
			return nil, nil, lerr
		}
		switch {
		case !has:
		case ok && k.created(strings.TrimSpace(v)):
			f.Delete(k.key)
			cleared = append(cleared, k.key)
		case k.key != config.KeyPairingDisplay:
			kept = append(kept, k.key)
		}
	}
	sort.Strings(cleared)
	sort.Strings(kept)
	if len(cleared) == 0 {
		return nil, kept, nil
	}
	if err := f.Save(); err != nil {
		return nil, kept, fmt.Errorf("write the config file: %w", err)
	}
	if err := RemoveJoinPin(stateDir); err != nil {
		return cleared, kept, err
	}
	return cleared, kept, nil
}

// writeStateJSON writes v as the file name of stateDir durably and
// owner-only (temporary file matching pattern, fsync, rename, fsync of the
// directory).
func writeStateJSON(stateDir, name, pattern string, v any) (err error) {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return fmt.Errorf("create the state directory: %w", err)
	}
	f, err := os.CreateTemp(stateDir, pattern)
	if err != nil {
		return fmt.Errorf("write %s: %w", name, err)
	}
	tmp := f.Name()
	defer func() {
		if err != nil {
			_ = f.Close()
			_ = os.Remove(tmp)
		}
	}()
	if _, err := f.Write(append(b, '\n')); err != nil {
		return fmt.Errorf("write %s: %w", name, err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("write %s (fsync): %w", name, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("write %s: %w", name, err)
	}
	if err := os.Rename(tmp, filepath.Join(stateDir, name)); err != nil {
		return fmt.Errorf("write %s: %w", name, err)
	}
	if err := syncDirectory(stateDir); err != nil {
		return fmt.Errorf("write %s (fsync of the directory): %w", name, err)
	}
	return nil
}
