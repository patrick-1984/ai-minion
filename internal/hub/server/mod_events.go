package server

import (
	"context"
	"fmt"
	"net/http"

	"github.com/patrick-1984/ai-minion/internal/hub/api"
	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/hub/workers"
)

// eventsModuleOrder places the events module after the routing module (50),
// the workers module (30) and the api module (40) it builds on.
const eventsModuleOrder = 55

func init() { registerEventsModule(DefaultRegistry) }

// registerEventsModule registers the events module in r (Story 5.3). It
//
//   - creates the in-memory [workers.EventHub] and the [workers.Ingest] that
//     takes the "ev" streams of every Worker session as the registry adds it,
//     and provides both, so that a later module (the call streams of Epic 7)
//     can use the hub and set the Ingest's handler for the other stream kinds;
//   - mounts GET /api/v1/sessions (the aggregated session list) and
//     GET /api/v1/events (the SSE feed, re-authorised every 30 s) on the root
//     mux, and ends the open feeds (with a "reset" event) as soon as the Hub
//     begins to shut down, before the HTTP server waits for its handlers.
func registerEventsModule(r *Registry) {
	err := r.Register("events", eventsModuleOrder, initEvents,
		DeclaresService[*workers.EventHub](),
		DeclaresService[*workers.Ingest]())
	if err != nil {
		panic(err)
	}
}

func initEvents(ctx context.Context, env *Env) error {
	cfg, err := Require[*Config](env)
	if err != nil {
		return err
	}
	mux, err := Require[*http.ServeMux](env)
	if err != nil {
		return err
	}
	svc, err := Require[*workers.Service](env)
	if err != nil {
		return err
	}
	authn, err := Require[*api.Authenticator](env)
	if err != nil {
		return err
	}
	az, err := Require[*authz.Authorizer](env)
	if err != nil {
		return err
	}
	st, err := Require[*store.Store](env)
	if err != nil {
		return err
	}

	// The hub keeps every Worker's share rule in the Hub database: what it may
	// show of a session it stored (the pipe history) follows the rule the Worker
	// declared, across reconnects and restarts, and is hidden while it is unknown.
	hub := workers.NewEventHub(workers.EventHubOptions{Share: st, Logger: cfg.logger()})
	ing, err := workers.NewIngest(workers.IngestOptions{Hub: hub, Logger: cfg.logger()})
	if err != nil {
		return fmt.Errorf("events: %w", err)
	}
	svc.Registry().OnAdd(ing.Attach)

	lister := &api.SessionLister{
		Directory: workers.StoreDirectory{Store: st},
		State:     hub,
		Authz:     az,
		OwnerName: func(ctx context.Context, ownerID string) string {
			u, err := st.UserByID(ctx, ownerID)
			if err != nil {
				return ""
			}
			return u.Username
		},
	}
	sessions, err := api.NewSessionsHandler(authn, lister)
	if err != nil {
		return fmt.Errorf("events: %w", err)
	}
	events, err := api.NewEventsHandler(api.EventsConfig{
		Auth: authn, Authz: az, Bus: hub, Lister: lister, Owners: svc.OwnerLookup(), Log: cfg.logger(),
	})
	if err != nil {
		return fmt.Errorf("events: %w", err)
	}
	mux.Handle(api.SessionsPath, sessions)
	mux.Handle(api.EventsPath, events)
	// Server.Run shuts the HTTP server down before it stops the kernel, and a
	// graceful HTTP shutdown waits for the open SSE handlers. The feeds must
	// therefore end as soon as shutdown begins, which is when the start
	// context (Run's context) is cancelled; the stop hook is the backstop for
	// a kernel that stops for another reason.
	stopWatch := context.AfterFunc(ctx, events.Close)
	env.OnStop(func(context.Context) error {
		stopWatch()
		events.Close()
		return nil
	})
	if err := Provide(env, hub); err != nil {
		return err
	}
	return Provide(env, ing)
}
