package presence

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
	"time"
)

// DefaultRetention is how long a stopped or stale record of another instance
// stays before it is cleaned.
const DefaultRetention = time.Hour

// ErrNotRunning is returned when the publisher is not publishing.
var ErrNotRunning = errors.New("presence: not publishing")

// Options configure a [Publisher].
type Options struct {
	Layout   Layout
	Instance string
	// Version is the daemon's build version.
	Version string
	// StartedAt is the daemon start time; zero means the time of Start.
	StartedAt time.Time
	// PID is published when positive.
	PID int
	// HubOrigin returns the configured Hub URL (sanitized before publication);
	// nil or "" publishes none (Solo).
	HubOrigin func() string
	// Now, Interval, StaleAfterS and Retention default to the clock, 30 s, 90
	// and one hour. Tests set them.
	Now         func() time.Time
	Interval    time.Duration
	StaleAfterS int
	Retention   time.Duration
	Logger      *slog.Logger
}

// Publisher keeps the presence record of one running daemon and the latest
// usage-meters document beside it.
type Publisher struct {
	o  Options
	b  backend
	mu sync.Mutex // guards everything below and every disk write

	started, stopped bool
	startedAt        time.Time
	usage            []byte
	cancel           context.CancelFunc
	done             chan struct{}
	failing          bool
}

// New checks the options and returns a publisher that does nothing yet.
func New(o Options) (*Publisher, error) {
	if !ValidInstance(o.Instance) {
		return nil, errors.New("presence: invalid instance id")
	}
	if o.Version == "" || len(o.Version) > 64 {
		return nil, errors.New("presence: invalid daemon version")
	}
	if err := o.Layout.validate(); err != nil {
		return nil, err
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Interval <= 0 {
		o.Interval = DefaultInterval
	}
	if o.StaleAfterS <= 0 {
		o.StaleAfterS = DefaultStaleAfterS
	}
	if o.Retention <= 0 {
		o.Retention = DefaultRetention
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Publisher{o: o, b: backend{o.Layout}}, nil
}

// Instance returns the instance id.
func (p *Publisher) Instance() string { return p.o.Instance }

func (p *Publisher) record(state string, now time.Time) []byte {
	r := Record{
		Schema: Schema, Instance: p.o.Instance, DaemonVersion: p.o.Version,
		StartedAt: ms(p.startedAt), UpdatedAt: ms(now), StaleAfterS: p.o.StaleAfterS,
		State: state, OSScope: p.o.Layout.Scope, PID: p.o.PID,
	}
	if p.o.HubOrigin != nil {
		r.HubOrigin = SanitizeHubOrigin(p.o.HubOrigin())
	}
	b, _ := json.Marshal(r)
	return b
}

// Start prepares the directories, cleans the files of the predecessor and of
// expired instances, and publishes the running record. On an error nothing is
// left behind and the publisher stays stopped (the daemon still runs).
func (p *Publisher) Start(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.started {
		return errors.New("presence: already started")
	}
	now := p.o.Now()
	p.startedAt = p.o.StartedAt
	if p.startedAt.IsZero() {
		p.startedAt = now
	}
	if err := p.b.prepare(now); err != nil {
		return fmt.Errorf("presence: prepare: %w", err)
	}
	for _, id := range p.b.predecessors(p.o.Instance) {
		_ = p.b.removeInstance(id)
	}
	p.cleanExpired(now)
	if err := p.b.setClaim(p.o.Instance); err != nil {
		return fmt.Errorf("presence: claim: %w", err)
	}
	if err := p.b.write(p.o.Instance+recordSuffix, p.record(StateRunning, now)); err != nil {
		p.b.dropClaim()
		_ = p.b.removeInstance(p.o.Instance)
		return fmt.Errorf("presence: write the record: %w", err)
	}
	if err := writeDiscovery(p.o.Layout); err != nil {
		p.o.Logger.Warn("presence: the discovery entry of this profile was not written", "error", err)
	}
	p.started = true
	hb, cancel := context.WithCancel(context.WithoutCancel(ctx))
	p.cancel = cancel
	p.done = make(chan struct{})
	go p.loop(hb)
	return nil
}

func (p *Publisher) loop(ctx context.Context) {
	defer close(p.done)
	t := time.NewTicker(p.o.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.heartbeat()
		}
	}
}

func (p *Publisher) heartbeat() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.started || p.stopped || !p.b.current(p.o.Instance) {
		return
	}
	now := p.o.Now()
	err := p.b.write(p.o.Instance+recordSuffix, p.record(StateRunning, now))
	switch {
	case err != nil && !p.failing:
		p.failing = true
		p.o.Logger.Warn("presence: the heartbeat could not be written", "error", err)
	case err == nil:
		p.failing = false
	}
}

// Stop ends the heartbeat and publishes the stopped marker, unless a reset
// cleared the record or a successor replaced it meanwhile (a late marker must
// not resurrect either). It is idempotent.
func (p *Publisher) Stop() error {
	p.mu.Lock()
	if !p.started || p.stopped {
		p.mu.Unlock()
		return nil
	}
	p.stopped = true
	cancel, done := p.cancel, p.done
	p.mu.Unlock()
	cancel()
	<-done
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.b.current(p.o.Instance) {
		return nil
	}
	if err := p.b.write(p.o.Instance+recordSuffix, p.record(StateStopped, p.o.Now())); err != nil {
		return fmt.Errorf("presence: write the stopped marker: %w", err)
	}
	return nil
}

// SetUsage stores the latest verified usage-meters document, in memory and
// next to the record (0600 private, 0644 public), atomically. The document is
// kept verbatim.
func (p *Publisher) SetUsage(raw []byte) error {
	if err := checkUsage(raw); err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.started || p.stopped || !p.b.current(p.o.Instance) {
		return ErrNotRunning
	}
	if err := p.b.write(p.o.Instance+usageSuffix, raw); err != nil {
		return fmt.Errorf("presence: write the usage document: %w", err)
	}
	p.usage = append([]byte(nil), raw...)
	return nil
}

// Usage returns a copy of the stored usage document, nil when none.
func (p *Publisher) Usage() []byte {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]byte(nil), p.usage...)
}

// ClearUsage removes the usage document from memory and disk. It is called on
// every change of authority (reset, replacement, revoke, new owner). The
// memory is cleared even when the file cannot be removed; the error is
// returned then.
func (p *Publisher) ClearUsage() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.usage = nil
	return p.b.remove(p.o.Instance + usageSuffix)
}

func (p *Publisher) cleanExpired(now time.Time) {
	dir, err := p.b.physDir()
	if err != nil {
		return
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	cut := now.Add(-p.o.Retention)
	for _, e := range ents {
		id, usage, ok := splitName(e.Name())
		if !ok || id == p.o.Instance {
			continue
		}
		fi, err := e.Info()
		if err != nil || !fi.Mode().IsRegular() {
			continue
		}
		expired := fi.ModTime().Before(cut)
		if !usage {
			if raw, err := p.b.read(e.Name(), MaxRecordSize); err == nil {
				if rec, err := DecodeRecord(raw); err == nil {
					expired = time.UnixMilli(rec.UpdatedAt).Before(cut)
				}
			}
		}
		if expired {
			_ = p.b.remove(e.Name())
		}
	}
}

// ClearProfile removes every presence and usage file of the profile of l, with
// no daemon running (reset, replacement, offline paths). Missing files are not
// an error. A user layout loses its whole presence directory content and its
// discovery entry; a root layout loses the files of the instance that last
// published for the state directory.
func ClearProfile(l Layout) error {
	if err := l.validate(); err != nil {
		if errors.Is(err, ErrUnsupported) {
			return nil
		}
		return err
	}
	b := backend{l}
	var errs []error
	if _, err := b.physDir(); err != nil {
		// A missing directory has nothing to clear; one that fails the
		// ownership and mode checks is refused by every reader as well, and
		// the daemon never published into it.
		_ = err
	} else {
		ids := b.instances()
		if b.public() {
			ids = nil
			if c := b.claimed(); c != "" {
				ids = []string{c}
			}
		}
		for _, id := range ids {
			if err := b.removeInstance(id); err != nil {
				errs = append(errs, err)
			}
		}
		if b.public() {
			b.dropClaim()
		}
	}
	if err := removeDiscovery(l); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// ClearUsageFiles removes only the usage-meters documents of the profile of l
// (the presence records stay): the offline wipe of an enrollment, where a
// daemon may still be running. Best effort per file; the first error is
// returned.
func ClearUsageFiles(l Layout) error {
	if l.validate() != nil {
		return nil
	}
	b := backend{l}
	if _, err := b.physDir(); err != nil {
		return nil
	}
	ids := b.instances()
	if b.public() {
		ids = nil
		if c := b.claimed(); c != "" {
			ids = []string{c}
		}
	}
	var errs []error
	for _, id := range ids {
		if err := b.remove(id + usageSuffix); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
