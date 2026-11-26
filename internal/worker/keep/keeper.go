package keep

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"regexp"
	"sync"
	"time"

	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/worker/store"
	"github.com/patrick-1984/ai-minion/internal/worker/term"
)

// Timing of the keeper.
const (
	// DefaultSaveInterval is the latest a kept session's layout is saved
	// again, whether or not it changed.
	DefaultSaveInterval = 60 * time.Second
	// DefaultProbeInterval is how often the layouts are compared with the
	// saved ones; a change is saved at the next probe.
	DefaultProbeInterval = 15 * time.Second
	// DefaultHostWait is the retry of a restore pass while the managed
	// terminal server is not running yet.
	DefaultHostWait = 3 * time.Second
	// restoreQuiet is how long a restored session is not saved: its tools are
	// still starting, and a save then would not see them.
	restoreQuiet = 2 * time.Minute
)

// AuditOpRestore is the op name of the local audit row of a restore.
const AuditOpRestore = "session_restore"

// Store is the Worker database as the keeper uses it. *store.Store
// implements it.
type Store interface {
	KeepSession(ctx context.Context, name, instanceID string) error
	UnkeepSession(ctx context.Context, name string) (bool, error)
	KeptSessions(ctx context.Context) ([]store.KeptSession, error)
	KeptNames(ctx context.Context) (map[string]bool, error)
	SaveKeptLayout(ctx context.Context, name, instanceID, layout string) (bool, error)
	RenameKeptSession(ctx context.Context, from, to string) (bool, error)
	AppendAudit(ctx context.Context, rec store.AuditRecord) error
}

var _ Store = (*store.Store)(nil)

// Config assembles a [Keeper].
type Config struct {
	Host    term.SessionHost
	Layouts term.LayoutKeeper
	Store   Store
	// Managed is set for a terminal server that a supervisor starts (the
	// Android app): a restore pass then waits until it runs, and runs again
	// when it was started again while the Worker kept running.
	Managed bool
	// Home is the user's home folder: the folder of a pane whose saved folder
	// is gone, and the root of the tools' session files.
	Home string
	// ProcRoot is the process table, "" for /proc.
	ProcRoot string
	// WorkerID names this Worker in the audit target; nil or "" leaves it out.
	WorkerID func() string
	Logger   *slog.Logger
	// Intervals; zero means the defaults.
	SaveInterval, ProbeInterval, HostWait time.Duration
	// Now is the clock; nil means time.Now.
	Now func() time.Time
}

// Keeper saves and restores the kept sessions. It is safe for concurrent use.
type Keeper struct {
	c    Config
	log  *slog.Logger
	wake chan struct{}

	mu sync.Mutex // one pass at a time
	// restored is true once the start restore pass ran; server is the
	// terminal server seen after it.
	restored bool
	server   string
	// lastSig and lastSave are the layout signature and the time of the last
	// save of each session; quiet holds restored sessions until when.
	lastSig  map[string]string
	lastSave map[string]time.Time
	quiet    map[string]time.Time
}

// New returns a keeper.
func New(c Config) (*Keeper, error) {
	if c.Host == nil || c.Layouts == nil || c.Store == nil {
		return nil, errors.New("keep: the keeper needs a host, its layouts and the store")
	}
	if c.ProcRoot == "" {
		c.ProcRoot = "/proc"
	}
	if c.SaveInterval <= 0 {
		c.SaveInterval = DefaultSaveInterval
	}
	if c.ProbeInterval <= 0 {
		c.ProbeInterval = DefaultProbeInterval
	}
	if c.HostWait <= 0 {
		c.HostWait = DefaultHostWait
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	log := c.Logger
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Keeper{
		c: c, log: log, wake: make(chan struct{}, 1),
		lastSig: map[string]string{}, lastSave: map[string]time.Time{}, quiet: map[string]time.Time{},
	}, nil
}

// Run restores the missing kept sessions once the terminal host is up, then
// saves the kept sessions until ctx ends. In the managed mode it restores
// again after the terminal server was started again.
func (k *Keeper) Run(ctx context.Context) {
	for {
		wait := k.c.ProbeInterval
		if !k.Tick(ctx) {
			wait = k.c.HostWait
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-k.wake:
			t.Stop()
		case <-t.C:
		}
	}
}

// Notify asks for a save pass soon (a keep change).
func (k *Keeper) Notify() {
	select {
	case k.wake <- struct{}{}:
	default:
	}
}

// Tick runs one pass: the restore pass when it is due, then the save pass.
// It reports false while a due restore waits for the terminal host.
func (k *Keeper) Tick(ctx context.Context) bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	server, _ := k.c.Layouts.ServerID(ctx)
	due := !k.restored || k.c.Managed && server != "" && k.server != "" && server != k.server
	if due {
		if _, err := k.restoreLocked(ctx); errors.Is(err, term.ErrHostDown) {
			return false
		} else if err != nil {
			k.log.Warn("keep: restore pass failed", "error", err.Error())
			return false
		}
		k.restored = true
		server, _ = k.c.Layouts.ServerID(ctx)
	}
	if server != "" {
		k.server = server
	}
	if err := k.saveLocked(ctx, ""); err != nil {
		k.log.Warn("keep: save pass failed", "error", err.Error())
	}
	return true
}

// Keep adds the session to the keep list and saves its layout now.
func (k *Keeper) Keep(ctx context.Context, name, instanceID string) error {
	if err := k.c.Store.KeepSession(ctx, name, instanceID); err != nil {
		return err
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	delete(k.lastSig, name)
	delete(k.quiet, name)
	return k.saveLocked(ctx, name)
}

// Unkeep removes the session and its saved layout from the keep list.
func (k *Keeper) Unkeep(ctx context.Context, name string) error {
	_, err := k.c.Store.UnkeepSession(ctx, name)
	k.mu.Lock()
	delete(k.lastSig, name)
	delete(k.lastSave, name)
	delete(k.quiet, name)
	k.mu.Unlock()
	return err
}

// Kept returns the names on the keep list.
func (k *Keeper) Kept(ctx context.Context) (map[string]bool, error) {
	return k.c.Store.KeptNames(ctx)
}

// sizeRE matches the sizes and the checksum of a tmux window layout: a client
// that attaches with another size is no new layout.
var sizeRE = regexp.MustCompile(`^[0-9a-f]{4},|\d+x\d+,\d+,\d+`)

// signature is the comparable shape of a layout: windows, names, splits
// without sizes, folders and the processes of the panes.
func signature(l term.SessionLayout) string {
	type pane struct {
		Cwd, Cmd string
		PID      int
	}
	type win struct {
		Name, Layout string
		Panes        []pane
	}
	ws := make([]win, 0, len(l.Windows))
	for _, w := range l.Windows {
		x := win{Name: w.Name, Layout: sizeRE.ReplaceAllString(w.Layout, "")}
		for _, p := range w.Panes {
			x.Panes = append(x.Panes, pane{Cwd: p.Cwd, Cmd: p.Command, PID: p.PID})
		}
		ws = append(ws, x)
	}
	b, _ := json.Marshal(ws)
	return string(b)
}

// saveLocked saves the kept sessions (only the one named when only is not
// ""): a session that exists is saved when its layout changed or the save
// interval passed; a kept name that is gone while a live session has its
// instance id was renamed, and the row follows it.
func (k *Keeper) saveLocked(ctx context.Context, only string) error {
	kept, err := k.c.Store.KeptSessions(ctx)
	if err != nil {
		return fmt.Errorf("read the keep list: %w", err)
	}
	if len(kept) == 0 {
		return nil
	}
	live, err := k.c.Host.ListSessions(ctx)
	if err != nil {
		return nil // no server: nothing to save, the saved layouts stay
	}
	kept = k.followRenames(ctx, kept, live)
	byName := map[string]term.Session{}
	for _, s := range live {
		byName[s.Name] = s
	}
	now := k.c.Now()
	var det *detector
	for _, r := range kept {
		if only != "" && r.Name != only {
			continue
		}
		s, ok := byName[r.Name]
		if !ok {
			continue
		}
		if until, q := k.quiet[r.Name]; q && now.Before(until) && only == "" {
			continue
		}
		l, err := k.c.Layouts.SessionLayout(ctx, r.Name)
		if err != nil {
			continue
		}
		sig := signature(l)
		if only == "" && r.Layout != "" && sig == k.lastSig[r.Name] && now.Sub(k.lastSave[r.Name]) < k.c.SaveInterval {
			continue
		}
		if det == nil {
			det = newDetector(k.c.ProcRoot, k.c.Home)
		}
		for wi := range l.Windows {
			for pi := range l.Windows[wi].Panes {
				p := &l.Windows[wi].Panes[pi]
				p.Tool, p.Conversation = det.detect(p.PID, p.Command, p.StartCommand)
			}
		}
		b, err := json.Marshal(l)
		if err != nil {
			continue
		}
		if string(b) != r.Layout {
			if _, err := k.c.Store.SaveKeptLayout(ctx, r.Name, s.Instance, string(b)); err != nil {
				k.log.Warn("keep: cannot save the layout of a kept session", "session", r.Name, "error", err.Error())
				continue
			}
		}
		k.lastSig[r.Name], k.lastSave[r.Name] = sig, now
	}
	return nil
}

// followRenames moves the row of a kept name that is gone to the name its
// terminal session (the same instance id) has now, unless that name is kept
// already, and returns the keep list as it then is.
func (k *Keeper) followRenames(ctx context.Context, kept []store.KeptSession, live []term.Session) []store.KeptSession {
	names := map[string]bool{}
	byInstance := map[string]string{}
	for _, s := range live {
		names[s.Name] = true
		if s.Instance != "" {
			byInstance[s.Instance] = s.Name
		}
	}
	keptName := map[string]bool{}
	for _, r := range kept {
		keptName[r.Name] = true
	}
	out := make([]store.KeptSession, 0, len(kept))
	for _, r := range kept {
		if to, found := byInstance[r.InstanceID]; !names[r.Name] && r.InstanceID != "" && found && !keptName[to] {
			if moved, err := k.c.Store.RenameKeptSession(ctx, r.Name, to); err == nil && moved {
				k.log.Info("keep: a kept session was renamed; the keep flag follows it", "from", r.Name, "to", to)
				keptName[to] = true
				delete(k.lastSig, r.Name)
				r.Name = to
			}
		}
		out = append(out, r)
	}
	return out
}

// Restore runs a restore pass now (see [Keeper.Tick] for when it runs on its
// own) and returns the names it recreated.
func (k *Keeper) Restore(ctx context.Context) ([]string, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.restoreLocked(ctx)
}

// restoreLocked recreates every kept session that does not exist and has a
// saved layout. An existing session of that name is never touched. Each
// recreated session is audited; one that fails does not stop the others.
// ErrHostDown (the managed server is not up yet) stops the pass.
func (k *Keeper) restoreLocked(ctx context.Context) ([]string, error) {
	kept, err := k.c.Store.KeptSessions(ctx)
	if err != nil {
		return nil, fmt.Errorf("read the keep list: %w", err)
	}
	if len(kept) == 0 {
		return nil, nil
	}
	exists := map[string]bool{}
	live, err := k.c.Host.ListSessions(ctx)
	switch {
	case err == nil:
		kept = k.followRenames(ctx, kept, live)
		for _, s := range live {
			exists[s.Name] = true
		}
	case errors.Is(err, term.ErrHostDown):
		return nil, err
	}
	// Any other listing error (no server yet on a host that is not managed):
	// the rebuild starts the server, and a name that is taken still fails
	// there without touching the session.
	var done []string
	for _, r := range kept {
		if exists[r.Name] || r.Layout == "" {
			continue
		}
		var l term.SessionLayout
		if err := json.Unmarshal([]byte(r.Layout), &l); err != nil {
			k.audit(ctx, r.Name, "failed_not_sent", "invalid_layout")
			continue
		}
		err := k.c.Layouts.RebuildSession(ctx, r.Name, l, k.c.Home, func(p term.PaneLayout, shell string) []string {
			return LaunchArgv(p.Tool, p.Conversation, shell)
		})
		switch {
		case errors.Is(err, term.ErrHostDown):
			return done, err
		case errors.Is(err, term.ErrSessionExists):
			continue // created meanwhile by someone else: never touched
		case err != nil:
			k.log.Warn("keep: cannot restore a kept session", "session", r.Name, "error", err.Error())
			k.audit(ctx, r.Name, "failed_not_sent", "restore_failed")
			continue
		}
		k.log.Info("keep: restored a kept session", "session", r.Name)
		k.audit(ctx, r.Name, string(ops.OutcomeDelivered), "")
		k.quiet[r.Name] = k.c.Now().Add(restoreQuiet)
		delete(k.lastSig, r.Name)
		done = append(done, r.Name)
	}
	return done, nil
}

func (k *Keeper) audit(ctx context.Context, name, outcome, reason string) {
	worker := ""
	if k.c.WorkerID != nil {
		worker = k.c.WorkerID()
	}
	rec := store.AuditRecord{
		Op: AuditOpRestore, Target: ops.Target{WorkerID: worker, SessionName: name}.String(),
		Outcome: outcome, Reason: reason, Origin: "local",
	}
	if err := k.c.Store.AppendAudit(ctx, rec); err != nil {
		k.log.Warn("keep: cannot write the audit row of a restore", "error", err.Error())
	}
}
