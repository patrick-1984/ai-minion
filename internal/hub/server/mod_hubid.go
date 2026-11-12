package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/auth/local"
	"github.com/patrick-1984/ai-minion/internal/hub/hubid"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
)

// hubidModuleOrder places the Hub identity module after the store (10), authz
// (15) and audit (20) modules and before the workers module (30), which needs
// the service.
const hubidModuleOrder = 25

// identityTickEvery is how often the expiry policy runs (design 8.4); the
// events it writes are unique per UTC day, so a shorter period is harmless.
// A variable so tests can shorten it.
var identityTickEvery = 24 * time.Hour

// identityRetryEvery is the wait after a tick that found an identity operation
// running.
const identityRetryEvery = 30 * time.Second

func init() { registerHubIDModule(DefaultRegistry) }

// registerHubIDModule registers the Hub identity module in r. It requires the
// store and provides the one *hubid.Service.
func registerHubIDModule(r *Registry) {
	if err := r.Register("hubid", hubidModuleOrder, initHubID, DeclaresService[*hubid.Service]()); err != nil {
		panic(err)
	}
}

// initHubID opens the two-tier Hub identity (design hub-identity-v2): it runs
// the recovery of section 9.5 without a passphrase and generates nothing. A
// pristine data directory (B0), a migrated 0.6 identity (B1) and a sealed root
// without a certificate (B2) start the Hub without a Worker service; the Hub
// refuses to start, naming the files, when a recorded key file is missing.
// Expiry warnings run at start and every 24 hours; the expiry itself runs at
// the not_after of the serving certificate.
func initHubID(ctx context.Context, env *Env) error {
	cfg, err := Require[*Config](env)
	if err != nil {
		return err
	}
	st, err := Require[*store.Store](env)
	if err != nil {
		return err
	}
	svc, err := hubid.Open(ctx, hubIDOptions(cfg, st))
	if err != nil {
		return fmt.Errorf("start the Hub identity: %w", err)
	}
	if err := Provide(env, svc); err != nil {
		return fmt.Errorf("provide the Hub identity: %w", err)
	}
	timers := newExpiryTimers(svc, cfg.logger(), time.Now)
	timers.start(ctx)
	svc.SetActivationHook(timers.rearm)
	env.OnStop(func(context.Context) error {
		svc.SetActivationHook(nil)
		timers.stop()
		return nil
	})
	return nil
}

// identityOptions lets a test change the options of the identity service (a
// fast sealer, crash hooks). It is nil in production.
var identityOptions func(*hubid.Options)

// hubIDOptions are the options the one Hub identity service is opened with.
// A passphrase that equals the signed-in administrator's login password is
// refused (section 10.3); the veto applies to the web channel only.
func hubIDOptions(cfg *Config, st *store.Store) hubid.Options {
	hasher := local.NewHasher()
	opts := hubid.Options{
		DataDir: cfg.DataDir, Store: st, Logger: cfg.logger(),
		RejectPassphrase: func(actor hubid.Actor, pass string) bool {
			if actor.Channel != hubid.ChannelUI {
				return false
			}
			u, err := st.UserByID(context.Background(), actor.ID)
			if err != nil || u.PasswordHash == "" {
				return false
			}
			ok, _, err := hasher.Verify(context.Background(), pass, u.PasswordHash)
			return err == nil && ok
		},
	}
	if identityOptions != nil {
		identityOptions(&opts)
	}
	return opts
}

// HubIdentity returns the one Hub identity service. The "hubid" module owns
// it and its timers (the daily warning tick and the expiry tick at the serving
// certificate's not_after); other modules consume it through this accessor and
// must not open the identity themselves. It fails when the module did not run.
func HubIdentity(env *Env) (*hubid.Service, error) { return Require[*hubid.Service](env) }

// expiryTimers runs hubid.Tick at start, every 24 hours, and at the not_after
// of the serving certificate. The activation hook of the service re-arms the
// expiry timer after every activation (first certificate, re-issue, rotation).
type expiryTimers struct {
	svc    *hubid.Service
	log    *slog.Logger
	now    func() time.Time
	kick   chan struct{}
	cancel context.CancelFunc
	done   chan struct{}
}

func newExpiryTimers(svc *hubid.Service, log *slog.Logger, now func() time.Time) *expiryTimers {
	return &expiryTimers{svc: svc, log: log, now: now, kick: make(chan struct{}, 1), done: make(chan struct{})}
}

// rearm tells the loop to recompute the expiry timer. It never blocks.
func (t *expiryTimers) rearm() {
	select {
	case t.kick <- struct{}{}:
	default:
	}
}

// tick runs the expiry policy and reports whether to retry soon: an identity
// operation that holds the rotation mutex is "retry later", not a failure.
func (t *expiryTimers) tick(ctx context.Context) (retry bool) {
	_, err := t.svc.Tick(ctx)
	switch {
	case err == nil:
		return false
	case errors.Is(err, hubid.ErrConflict):
		t.log.Debug("hub identity: the expiry check waits for a running identity operation")
		return true
	}
	t.log.Warn("hub identity: the expiry check failed", "error", err.Error())
	return false
}

func (t *expiryTimers) start(ctx context.Context) {
	tctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	t.cancel = cancel
	retry := t.tick(tctx)
	go t.loop(tctx, retry)
}

func (t *expiryTimers) stop() {
	t.cancel()
	<-t.done
}

func (t *expiryTimers) loop(ctx context.Context, retry bool) {
	defer close(t.done)
	daily := time.NewTimer(identityTickEvery)
	defer daily.Stop()
	if retry {
		daily.Reset(identityRetryEvery)
	}
	expiry := time.NewTimer(time.Hour)
	defer expiry.Stop()
	// arm points the expiry timer at the serving certificate's not_after; with
	// no serving certificate (or one that already expired and was handled) it
	// stays stopped.
	armed := false
	arm := func() {
		if armed && !expiry.Stop() {
			select {
			case <-expiry.C:
			default:
			}
		}
		armed = false
		sn, err := t.svc.Current()
		if err != nil {
			return
		}
		d := time.UnixMilli(sn.Cert.NotAfterMs).Sub(t.now())
		if d < 0 {
			d = 0
		}
		expiry.Reset(d)
		armed = true
	}
	arm()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.kick:
			arm()
		case <-expiry.C:
			armed = false
			if t.tick(ctx) {
				expiry.Reset(identityRetryEvery)
				armed = true
			} else {
				arm()
			}
		case <-daily.C:
			next := identityTickEvery
			if t.tick(ctx) {
				next = identityRetryEvery
			}
			daily.Reset(next)
		}
	}
}
