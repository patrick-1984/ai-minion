package server

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/adminsock"
	"github.com/patrick-1984/ai-minion/internal/hub/api"
	"github.com/patrick-1984/ai-minion/internal/hub/hubid"
	"github.com/patrick-1984/ai-minion/internal/hub/web"
)

// hubIdentityModuleOrder places the module after the REST framework (40),
// whose handler registry it fills, after the workers module (30), and after
// the "hubid" module (25), which owns the one *hubid.Service and its timers.
const hubIdentityModuleOrder = 47

// identityStatusTTL is how long the page banner trusts its last look at the
// identity status; the banner runs on every page render.
const identityStatusTTL = 15 * time.Second

func init() { registerHubIdentityModule(DefaultRegistry) }

// registerHubIdentityModule registers the Hub identity operations module
// (contract section 8): it consumes the *hubid.Service of the "hubid" module
// (it neither opens the identity nor runs a timer) and
//
//   - registers the handlers of the five hub_identity_* operations, behind the
//     gate of fresh sign-in, same origin and effective HTTPS;
//   - serves the local admin socket for `minion hub identity` (section 8.3);
//   - gives the web UI the identity status for its banner and countdown.
func registerHubIdentityModule(r *Registry) {
	if err := r.Register("hub_identity", hubIdentityModuleOrder, initHubIdentity); err != nil {
		panic(err)
	}
}

func initHubIdentity(_ context.Context, env *Env) error {
	cfg, err := Require[*Config](env)
	if err != nil {
		return err
	}
	nw, err := Require[*Network](env)
	if err != nil {
		return err
	}
	handlers, err := Require[*api.Registry](env)
	if err != nil {
		return err
	}
	svc, err := HubIdentity(env)
	if err != nil {
		return err
	}
	log := cfg.logger()

	var trusted []string
	if o := originOf(nw.PublicURL()); o != "" {
		trusted = append(trusted, o)
	}
	gate := api.IdentityGate{
		EffectiveHTTPS: nw.EffectiveHTTPS, TrustedOrigins: trusted,
		Scheme: func(r *http.Request) string {
			if onManagementListener(r) {
				return "http" // the management listener serves cleartext
			}
			return nw.Scheme(r)
		},
	}
	if err := api.RegisterHubIdentityHandlers(handlers, svc, gate, log); err != nil {
		return fmt.Errorf("register the Hub identity handlers: %w", err)
	}

	// The web UI shows the banner and the countdown from the same status.
	statusSrc := newIdentityStatusSource(svc)
	release := web.SetIdentityStatus(statusSrc.get)
	env.OnStop(func(context.Context) error { release(); return nil })

	if err := startAdminSocket(env, cfg, svc); err != nil {
		return err
	}
	return nil
}

// identityStatusSource caches the status for the banner.
type identityStatusSource struct {
	svc     api.IdentityService
	mu      sync.Mutex
	checked time.Time
	last    hubid.Status
	have    bool
}

func newIdentityStatusSource(svc api.IdentityService) *identityStatusSource {
	return &identityStatusSource{svc: svc}
}

func (s *identityStatusSource) get(ctx context.Context) (web.IdentityStatus, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.have || time.Since(s.checked) >= identityStatusTTL {
		st, err := s.svc.Status(ctx)
		if err != nil {
			if !s.have {
				return web.IdentityStatus{}, false
			}
		} else {
			s.last, s.have, s.checked = st, true, time.Now()
		}
	}
	return web.IdentityStatus{
		Label:       s.last.State.String(),
		Serving:     s.last.Serving,
		NotAfter:    s.last.NotAfter,
		DaysLeft:    s.last.DaysLeft,
		Warning:     string(s.last.Warning),
		Passphrase:  s.last.PassphraseSet,
		Established: s.last.State == hubid.StateEstablished,
		Migrated:    s.last.MigratedFrom06,
	}, true
}

// startAdminSocket serves the local admin socket in the data directory. A
// socket path that is too long for a Unix socket, or a system without Unix
// sockets, is logged and skipped: the web UI still works. Another live Hub on
// the data directory is an error.
func startAdminSocket(env *Env, cfg *Config, svc *hubid.Service) error {
	path := adminsock.Path(cfg.DataDir)
	ln, err := adminsock.Listen(path)
	if err != nil {
		if adminsock.Skippable(err) {
			cfg.logger().Warn("the local admin socket is off; `minion hub identity` cannot reach this Hub", "error", err)
			return nil
		}
		return fmt.Errorf("local admin socket: %w", err)
	}
	srv := adminsock.NewServer(svc, cfg.logger())
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := srv.Serve(ln); err != nil {
			cfg.logger().Error("the local admin socket stopped", "error", err)
		}
	}()
	env.OnStop(func(context.Context) error {
		_ = srv.Close()
		_ = ln.Close()
		<-done
		return nil
	})
	return nil
}
