package server

import (
	"context"
	"fmt"
	"net/http"

	"github.com/patrick-1984/ai-minion/internal/hub/auth/session"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/hub/web"
)

// webModuleOrder places the web module after the modules whose services it
// needs (core, store, auth) and after the modules that serve more specific
// routes, so the catch-all "/" it claims never shadows them.
const webModuleOrder = 80

func init() { registerWeb(DefaultRegistry, web.DefaultRegistry) }

// inspectorContextPurpose names the key the web module derives from the data key for the
// identity context of the session inspector (see store.SecretBox.Derive).
const inspectorContextPurpose = "web/inspector-context/v1"

// registerWeb registers the web UI module in r (Story 3.7). Pages, navigation
// entries and login providers come from wr, which other stories fill from
// their own init functions through web.RegisterPage and the like.
//
// The module also connects the administration pages to the REST operations of
// this Hub (web.SetAdminOps) and gives the onboarding pages the configured
// public URL (web.SetPublicURL). It provides the *web.Server and mounts it on the
// root mux as the catch-all "/": the login page, the change-password page, the
// static assets and every registered page. Routes with a longer pattern, such
// as /auth/login and /api/v1/, win over it.
func registerWeb(r *Registry, wr *web.Registry) {
	err := r.Register("web", webModuleOrder, func(ctx context.Context, env *Env) error {
		return initWeb(ctx, env, wr)
	}, DeclaresService[*web.Server]())
	if err != nil {
		panic(err)
	}
}

func initWeb(_ context.Context, env *Env, wr *web.Registry) error {
	cfg, err := Require[*Config](env)
	if err != nil {
		return err
	}
	nw, err := Require[*Network](env)
	if err != nil {
		return err
	}
	mux, err := Require[*http.ServeMux](env)
	if err != nil {
		return err
	}
	mw, err := Require[*session.Middleware](env)
	if err != nil {
		return err
	}
	st, err := Require[*store.Store](env)
	if err != nil {
		return err
	}
	srv, err := web.New(web.Config{
		Registry: wr,
		Sessions: mw,
		Secure:   func(*http.Request) bool { return session.CookieSecure(nw.PublicURL()) },
		Log:      cfg.logger(),
		// The inspector identity context is stable for an unchanged login across restarts
		// of the Hub: its key comes from the persisted data key, with a purpose of its own.
		ContextKey: st.Box().Derive(inspectorContextPurpose),
	})
	if err != nil {
		return fmt.Errorf("web UI: %w", err)
	}
	if err := Provide(env, srv); err != nil {
		return err
	}
	mux.Handle("/", srv)
	// The administration pages call the operation catalog over the REST
	// pipeline of this Hub, in process, as the signed-in admin. Without this
	// the four admin pages answer 503.
	caller := web.NewHandlerCaller(mux)
	web.SetAdminOps(caller)
	// The onboarding pages put the Hub address into the Worker commands. It
	// must be the configured MINION_PUBLIC_URL, which is what a remote Worker
	// can reach, not the Host header of the browser, which behind a proxy or
	// on a private name is not.
	releaseURL := web.SetPublicURL(func(*http.Request) string { return nw.PublicURL() })
	env.OnStop(func(context.Context) error {
		web.DisconnectAdminOps(caller)
		releaseURL()
		return nil
	})
	return nil
}
