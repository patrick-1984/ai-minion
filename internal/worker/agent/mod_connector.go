package agent

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/transport"
	"github.com/patrick-1984/ai-minion/internal/worker/store"
	"github.com/patrick-1984/ai-minion/internal/worker/term"
)

// Module name and start order of the Hub connector. It starts after the core
// module (order 100) whose store, policy and executor it needs, and leaves
// room before it for the modules that register link handlers.
const (
	// ModuleConnector is the name of the Hub connector module.
	ModuleConnector = "connector"
	// OrderConnector is its start order.
	OrderConnector = 200
)

func init() { RegisterModule(ModuleConnector, OrderConnector, startConnector) }

// Ops returns the names of the ops this executor implements, sorted. The
// Worker announces them in its hello (AD-4), so the Hub never dispatches an op
// that it does not serve.
func (e *Executor) Ops() []string {
	e.mu.RLock()
	defer e.mu.RUnlock()
	out := make([]string, 0, len(e.handlers))
	for name := range e.handlers {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// Features returns the proto.Feature* names this Worker really implements,
// sorted. The Worker announces them in its signed hello (spine AM-13), so that
// the Hub does not hand a request that relies on a safeguard to a Worker
// without it. The answer binding, the atomic submit and the message id of a
// pipe delivery are code of this executor; the verbatim text is a property of
// the terminal host and is announced only when the host guarantees it.
func (e *Executor) Features() []string {
	out := []string{proto.FeatureAnswerReviewed, proto.FeaturePipeCausal, proto.FeatureSendTextSubmit}
	if term.IsVerbatim(e.host) {
		out = append(out, proto.FeatureTextVerbatim)
	}
	return proto.NormaliseFeatures(out)
}

// connectorDeps are the seams of the connector module that tests replace.
type connectorDeps struct {
	dialer Dialer
	clock  transport.Clock
}

// startConnector is the module start function. It provides *Enrollments and
// *Connector to later modules and runs the connector until the Worker stops.
// A Worker without a Hub URL stays idle (Solo): the module still starts, so
// that a later `minion worker config set server` or enrollment needs no
// different code path.
func startConnector(ctx context.Context, env *Env) (StopFunc, error) {
	return connectorDeps{dialer: NewHubDialer()}.start(ctx, env)
}

func (d connectorDeps) start(ctx context.Context, env *Env) (StopFunc, error) {
	s := env.Settings()
	log := s.logger()
	st, err := Require[*store.Store](env)
	if err != nil {
		return nil, err
	}
	policy, err := Require[*Policy](env)
	if err != nil {
		return nil, err
	}
	ex, err := Require[*Executor](env)
	if err != nil {
		return nil, err
	}

	var hub config.WorkerHub
	if s.Resolver != nil {
		if hub, err = config.ResolveWorkerHub(s.Resolver); err != nil {
			return nil, err
		}
	}
	enr := NewEnrollments(st, s.StateDir)
	c, err := NewConnector(ConnectorConfig{
		HubURL:     hub.Server,
		Name:       s.Name,
		Ops:        ex.Ops,
		Features:   ex.Features,
		Key:        enr.LoadKey,
		Enrollment: enr,
		Policy:     policy,
		Dialer:     d.dialer,
		Clock:      d.clock,
		Session:    transport.SessionOptions{Keepalive: transport.KeepaliveConfig{Clock: d.clock}},
		Logger:     log,
	})
	if err != nil {
		return nil, err
	}
	if err := Provide(env, enr); err != nil {
		return nil, err
	}
	if err := Provide(env, c); err != nil {
		return nil, err
	}
	if err := c.applyStoredPolicy(ctx); err != nil {
		return nil, fmt.Errorf("apply the stored Hub policy: %w", err)
	}
	if hub.Server == "" {
		log.Info("no hub configured; the Worker runs alone (Solo). Set --server to connect to a Hub")
	} else {
		log.Info("hub connector started", "hub", hubLabel(hub.Server))
	}
	if hub.Insecure || hub.CAFile != "" {
		log.Debug("hub_insecure and hub_ca_file apply to plain HTTPS calls only; the Worker's hub connection is authenticated by its pin")
	}

	// The loop ends through its own context, not the start context: that one
	// ends when the shutdown begins, before the modules are stopped in order.
	loopCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	done := make(chan struct{})
	go func() {
		defer close(done)
		c.Run(loopCtx)
	}()
	// Old identities that `minion worker pair --new-code` replaced are revoked
	// at their Hub from here, with the same dialer (Story 4.7).
	ret, err := NewRetirer(RetirerConfig{
		Store: enr.RetiredStore(), Dialer: d.dialer, Name: s.Name, Clock: d.clock,
		Session: transport.SessionOptions{Keepalive: transport.KeepaliveConfig{Clock: d.clock}},
		Logger:  log,
	})
	if err != nil {
		cancel()
		<-done
		return nil, err
	}
	retDone := make(chan struct{})
	go func() {
		defer close(retDone)
		ret.Run(loopCtx)
	}()
	return func(stopCtx context.Context) error {
		cancel()
		for _, ch := range []chan struct{}{done, retDone} {
			select {
			case <-ch:
			case <-stopCtx.Done():
				return errors.New("the hub connector did not stop in time")
			}
		}
		return nil
	}, nil
}
