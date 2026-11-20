package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/patrick-1984/ai-minion/internal/proto"
)

// ReasonPaused is the Status.Reason of an idle connector whose Worker is
// paused (minion worker pause): it does not connect until resumed.
const ReasonPaused = "paused (minion worker resume)"

// PauseFile is the file in the state directory that holds the pause. It lives
// outside worker.db, so that no schema migration is needed. One local choice,
// until resumed: there is no timer.
const PauseFile = "pause.json"

// OpLocalPause is the op of the local control socket that pauses or resumes
// the running Worker ({"paused": true|false}). Like the other local ops it is
// not part of the op catalog: a Hub cannot call it.
const OpLocalPause = "worker_pause"

// PauseSettleTimeout bounds the wait of a pause for the Hub link and the
// automatic pairing to end.
const PauseSettleTimeout = 15 * time.Second

// ReadPause reads the pause of the state directory; a missing file is
// "not paused".
func ReadPause(stateDir string) (bool, error) {
	b, err := os.ReadFile(filepath.Join(stateDir, PauseFile))
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		// An unreadable choice is not a resume: stay paused.
		return true, fmt.Errorf("the pause file %s cannot be read, so the Worker stays paused: %w", filepath.Join(stateDir, PauseFile), err)
	}
	var p struct {
		Paused *bool `json:"paused"`
	}
	if err := json.Unmarshal(b, &p); err != nil || p.Paused == nil {
		return true, fmt.Errorf("the pause file %s is not valid, so the Worker stays paused; fix it or run minion worker resume", filepath.Join(stateDir, PauseFile))
	}
	return *p.Paused, nil
}

// WritePause saves the pause durably (temporary file, fsync, rename, owner
// only). When it returns nil the choice survives a crash and a restart.
func WritePause(stateDir string, paused bool) error {
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(map[string]bool{"paused": paused})
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(stateDir, ".pause-*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, werr := f.Write(append(b, '\n'))
	serr := f.Sync()
	cerr := f.Close()
	if err := errors.Join(werr, serr, cerr); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, filepath.Join(stateDir, PauseFile)); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if d, err := os.Open(stateDir); err == nil { // best effort: not possible on Windows
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

// PauseGate is the pause switch the connector reads. Every change closes the
// channel Paused returned before it, so that the connector's backoff, idle
// and connection waits wake up at once.
type PauseGate struct {
	mu      sync.Mutex
	paused  bool
	changed chan struct{}
	// work counts the Hub work outside the connector (automatic pairing,
	// retirements) of the current unpaused interval.
	work atomic.Int64
}

// NewPauseGate returns a gate in the given state.
func NewPauseGate(paused bool) *PauseGate {
	return &PauseGate{paused: paused, changed: make(chan struct{})}
}

// Paused reports whether Hub traffic is paused, and the channel that is closed
// at the next change. A nil gate is never paused.
func (g *PauseGate) Paused() (bool, <-chan struct{}) {
	if g == nil {
		return false, nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.paused, g.changed
}

// Set changes the state and wakes the waiters when it changed.
func (g *PauseGate) Set(paused bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.paused == paused {
		return
	}
	g.paused = paused
	close(g.changed)
	g.changed = make(chan struct{})
}

// waitUnpaused returns when the gate is not paused, or with ctx's error.
func waitUnpaused(ctx context.Context, g *PauseGate) error {
	for {
		paused, ch := g.Paused()
		if !paused {
			return ctx.Err()
		}
		select {
		case <-ch:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// cancelOnPause returns a context that a pause ends; stop releases it and
// reports whether a pause ended it.
func cancelOnPause(ctx context.Context, g *PauseGate) (context.Context, func() (paused bool)) {
	cctx, cancel := context.WithCancelCause(ctx)
	done := make(chan struct{})
	go func() {
		for {
			paused, ch := g.Paused()
			if paused {
				cancel(errPaused)
				return
			}
			select {
			case <-ch:
			case <-done:
				return
			case <-cctx.Done():
				return
			}
		}
	}()
	return cctx, func() bool {
		close(done)
		paused := errors.Is(context.Cause(cctx), errPaused)
		cancel(nil)
		return paused
	}
}

// PauseService applies pause and resume in the running Worker: the pause
// file and the connector gate. A pause is acknowledged only after the Hub link
// is closed and no automatic pairing runs any more.
type PauseService struct {
	dir  string
	log  *slog.Logger
	gate *PauseGate
	// settled reports whether the connector idles as paused (no link, no
	// attempt in flight); nil means there is no connector.
	settled func() bool
	// settle bounds the wait for it.
	settle time.Duration

	mu sync.Mutex
}

// NewPauseService reads the pause file of dir and returns the service with its
// gate in the saved state: a Worker that starts paused makes no Hub traffic
// from its first cycle. A file that cannot be read pauses too.
func NewPauseService(dir string, log *slog.Logger) *PauseService {
	paused, err := ReadPause(dir)
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	if err != nil {
		log.Warn("the saved pause cannot be read; the Worker stays paused", "error", err.Error())
	}
	if paused {
		log.Info("the Worker starts paused: no Hub traffic until minion worker resume")
	}
	return &PauseService{dir: dir, log: log, gate: NewPauseGate(paused), settle: PauseSettleTimeout}
}

// Gate returns the switch the connector reads.
func (p *PauseService) Gate() *PauseGate { return p.gate }

// Paused reports whether the Worker is paused now.
func (p *PauseService) Paused() bool {
	v, _ := p.gate.Paused()
	return v
}

// Set pauses or resumes. The choice is saved first (a failure changes
// nothing), then the gate is set, and a pause waits until the link is closed
// and the pairing has ended; if that does not happen in time the Worker stays
// paused, but the call fails, so that the pause is never reported as done.
func (p *PauseService) Set(ctx context.Context, paused bool) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := WritePause(p.dir, paused); err != nil {
		return fmt.Errorf("cannot save the pause: %w", err)
	}
	p.gate.Set(paused)
	p.log.Info("pause changed", "paused", paused)
	if !paused {
		return nil
	}
	deadline := time.NewTimer(p.settle)
	defer deadline.Stop()
	for p.gate.work.Load() > 0 || (p.settled != nil && !p.settled()) {
		select {
		case <-time.After(20 * time.Millisecond):
		case <-deadline.C:
			return errors.New("paused, but the Hub link or a pairing did not end within " + p.settle.String())
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

// beginWork admits one piece of Hub work for the current unpaused interval:
// its context ends at the next pause, and a pause is acknowledged only after
// end was called. ok is false when the Worker is paused now. end reports
// whether a pause ended the work.
func (g *PauseGate) beginWork(ctx context.Context) (wctx context.Context, end func() (paused bool), ok bool) {
	if g == nil {
		return ctx, func() bool { return false }, true
	}
	// Counted before the gate is read: a pause that sets the gate after this
	// read waits for the count, one that set it before is seen here.
	g.work.Add(1)
	if p, _ := g.Paused(); p {
		g.work.Add(-1)
		return nil, nil, false
	}
	wctx, stop := cancelOnPause(ctx, g)
	return wctx, func() bool {
		paused := stop()
		g.work.Add(-1)
		return paused
	}, true
}

// pauseRefusal is the error of a local request that needs the Hub while the
// Worker is paused.
func pauseRefusal() error {
	return apiErr(proto.CodePaused, "", "the Worker is paused: it makes no Hub requests until minion worker resume")
}
