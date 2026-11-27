package agent

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"github.com/patrick-1984/ai-minion/internal/version"
	"sort"
	"time"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/ops"
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
	files := e.filesAnnounced()
	for name := range e.handlers {
		if ops.IsFileOp(name) && !files {
			// The file ops are announced only with files.v1 (NFR-8).
			continue
		}
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// Features returns the proto.Feature* names this Worker really implements,
// sorted. The Worker announces them in its signed hello (spine AM-13), so that
// the Hub does not hand a request that relies on a safeguard to a Worker
// without it. The answer binding, the atomic submit and the message id of a
// pipe delivery are code of this executor; the verbatim text and the listing
// of session instance ids are properties of the terminal host and are
// announced only when the host guarantees them. proto.FeatureSessionInstanceV1
// is never announced: the Worker does not refuse a target of another instance
// yet.
func (e *Executor) Features() []string {
	out := []string{proto.FeatureAnswerReviewed, proto.FeaturePipeCausal, proto.FeatureSendTextSubmit, proto.FeatureSendTextAgentOnly}
	if term.IsVerbatim(e.host) {
		out = append(out, proto.FeatureTextVerbatim)
	}
	if term.ReportsInstances(e.host) {
		out = append(out, proto.FeatureSessionInstanceIDListV1)
	}
	if e.filesAnnounced() {
		out = append(out, proto.FeatureFilesV1)
	}
	return proto.NormaliseFeatures(out)
}

// connectorDeps are the seams of the connector module that tests replace.
type connectorDeps struct {
	dialer     Dialer
	pairDialer PairDialer
	clock      transport.Clock
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
	if rec, ok, err := enr.RecoverHub(ctx, s.Resolver); err != nil {
		return nil, fmt.Errorf("read the enrollment: %w", err)
	} else if ok {
		hub.Server = rec.URL
		if hub.Fingerprint == "" {
			hub.Fingerprint = rec.Fingerprint
		}
		log.Info("recovered the Hub address from the enrollment (the join did not finish saving its settings)", "hub", hubLabel(rec.URL))
	}
	// A configured hub_fingerprint that is malformed refuses the start: it is
	// never read as "absent" (the operator asked for a check).
	expect, err := ParseHubExpectation(hub.Fingerprint)
	if err != nil {
		return nil, err
	}
	host, _ := Require[term.SessionHost](env)
	hostInfo, hostErr := Require[*SessionHostInfo](env)
	hostKind := ""
	if hostErr == nil {
		hostKind = hostInfo.Kind
	}
	su, facts, features := setupSelfUpdate(log, s, ex, host, hostKind)
	master := masterFeature(env)
	if master != nil {
		baseFeatures := features
		features = func() []string {
			return proto.NormaliseFeatures(append(baseFeatures(), proto.FeatureMasterSessionV1))
		}
	}
	// The read-back receiver exists before the connector dials, so that the
	// very first signed hello already carries usage_readback_v1: a Hub decides
	// once per connection whether to deliver, and a hello without the feature
	// would withhold every nomination and withdrawal from a healthy first link.
	// The announcement stands while the Worker can publish; the presence module
	// attaches the publisher later and withdraws the feature (SetUnavailable)
	// only if it cannot publish, and a frame in that gap is answered "busy".
	rcv := NewReadbackReceiver(log, enr.Moved)
	rcv.SetGuard(enr.WithEnrollmentIfLive)
	rcv.SetTombstoneDir(s.StateDir)
	{
		baseFeatures := features
		features = func() []string {
			out := baseFeatures()
			if rcv.Ready() {
				out = append(append([]string(nil), out...), proto.FeatureUsageReadbackV1)
			}
			return proto.NormaliseFeatures(out)
		}
	}
	health, _ := Require[*SessionListHealth](env)
	// The session host, whether its sessions are hidden (the Hub's
	// hidden_sessions policy reads it) and the latest listing error (v0.10.0).
	// A missing service (a test core) leaves them unknown.
	base := facts
	facts = func() transport.WorkerFacts {
		f := base()
		hostInfo.applyTo(&f)
		applyListHealth(&f, health)
		f.LoginStartupOutdated = s.LoginStartupOutdated
		return f
	}
	dbg := setupDebugRemote(log, s, ex)
	facts = withDebugFact(facts, dbg)
	// The saved pause applies from the first cycle.
	pause := NewPauseService(s.StateDir, log)
	c, err := NewConnector(ConnectorConfig{
		Pause:                  pause.Gate(),
		HubURL:                 hub.Server,
		Name:                   s.Name,
		Ops:                    ex.Ops,
		Features:               features,
		Facts:                  facts,
		Key:                    enr.LoadKey,
		Out:                    s.Out,
		Enrollment:             enr,
		ConsumeUsageIntent:     st.ConsumeUsageIntent,
		UsageConnectionAllowed: st.UsageConnectionAllowed, UsageConsent: st.UsageConsent,
		OnWelcomeStored: func(w HubWelcome, hubPub ed25519.PublicKey) {
			rcv.WelcomeStored(w.WorkerID, w.OwnerID, hubPub, w.Pending)
		},
		OnHubPinChanged: rcv.Clear,
		Policy:          policy,
		Dialer:          d.dialer,
		Clock:           d.clock,
		Session:         transport.SessionOptions{Keepalive: transport.KeepaliveConfig{Clock: d.clock}, FrameTap: env.Settings().TrafficTap},
		Logger:          log,
	})
	if err != nil {
		return nil, err
	}
	if master != nil {
		c.OnLink("master", master.serve)
	}
	if err := Provide(env, enr); err != nil {
		return nil, err
	}
	if err := Provide(env, rcv); err != nil {
		return nil, err
	}
	// The managed Android mode shows the pairing code through the private
	// status of the control socket, never on the console or in the log.
	var private *PairingStatus
	if s.Managed.Android {
		private = NewPairingStatus(d.clock)
		if err := Provide(env, private); err != nil {
			return nil, err
		}
	}
	if err := Provide(env, c); err != nil {
		return nil, err
	}
	pause.settled = func() bool {
		st := c.Status()
		return st.State == ConnIdle && st.Reason == ReasonPaused
	}
	if err := Provide(env, pause); err != nil {
		return nil, err
	}
	su.hubRange = func() *version.ProtoRange { return connectorHubRange(c) }
	if err := c.applyStoredPolicy(ctx); err != nil {
		return nil, fmt.Errorf("apply the stored Hub policy: %w", err)
	}
	// The setting on a paired Worker is compared with the pin before the
	// connector starts (the three cases of CheckEnrolledHub).
	if err := CheckEnrolledHub(ctx, enr, expect, s.Out, log); err != nil {
		return nil, fmt.Errorf("check the paired Hub against hub_fingerprint: %w", err)
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
		Store: enr.RetiredStore(), Dialer: d.dialer, Name: s.Name, Clock: d.clock, Pause: pause.Gate(),
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
	commitDone := make(chan struct{})
	go func() {
		defer close(commitDone)
		tick := time.NewTicker(30 * time.Second)
		defer tick.Stop()
		// Only a Worker that started every module may commit an update.
		commitAfter(loopCtx, s.StateDir, version.Version, func() updateHealth {
			if !env.Started() {
				return healthNone
			}
			return connectorHealth(c.Status(), s.StateDir)
		}, tick.C)
	}()
	pairDone := make(chan struct{})
	go func() {
		defer close(pairDone)
		// Pairing talks to the Hub: it waits for the resume, and a pause during
		// it ends the attempt, which starts over after the resume.
		for loopCtx.Err() == nil {
			if waitUnpaused(loopCtx, pause.Gate()) != nil {
				return
			}
			pctx, end, ok := pause.Gate().beginWork(loopCtx)
			if !ok {
				continue
			}
			d.autoPair(pctx, s, hub.Server, expect, enr, ex, private)
			if !end() {
				return
			}
		}
	}()
	return func(stopCtx context.Context) error {
		cancel()
		for _, ch := range []chan struct{}{done, retDone, pairDone, commitDone} {
			select {
			case <-ch:
			case <-stopCtx.Done():
				return errors.New("the hub connector did not stop in time")
			}
		}
		return nil
	}, nil
}

// autoPair pairs a Worker that has a Hub address but no enrollment and no
// invitation key: it shows a pairing code (console and log), exactly like
// "minion worker pair", and the connector connects as soon as the pairing is
// stored. Without it such a Worker only idled as "not enrolled" and showed
// nothing to type into the Hub.
func (d connectorDeps) autoPair(ctx context.Context, s Settings, hubURL string, expect identity.FingerprintMatcher, enr *Enrollments, ex *Executor, private *PairingStatus) {
	if !needsAutoPair(ctx, s, hubURL, enr) {
		return
	}
	log := s.logger()
	dialer := d.pairDialer
	if dialer == nil {
		dialer = NewPairDialer()
	}
	p, err := NewPairer(PairerConfig{
		HubURL: hubURL, Name: s.Name, Ops: ex.Ops, Features: ex.Features, ExpectHub: expect,
		Enrollment: enr, Dialer: dialer, Clock: d.clock, Out: s.Out, Logger: log, Private: private, NoAdmit: private != nil,
	})
	if err != nil {
		log.Warn("cannot start pairing", "error", err.Error())
		return
	}
	if private != nil {
		log.Info("this Worker is not paired: the pairing code is in the private status of the app (never in this log)")
	} else {
		log.Info("this Worker is not paired: showing a pairing code; enter it in the Hub (Workers > Add worker)")
	}
	if expect.IsZero() {
		// First use without an expectation stays allowed (as before) and is
		// still reported as hub_check=none ("not verified") in the status and
		// on the Hub. The code itself authenticates the Hub it is entered in
		// (the CPace transcript binds the Hub's key), which the box shows.
		log.Info("no hub_fingerprint is set: the pairing code confirms the Hub's key; the box shows the Hub's words, "+
			"which the Hub shows on Workers > Add worker", "hub", hubLabel(hubURL), "hub_check", "none")
	}
	if _, err := p.Run(ctx); err != nil && ctx.Err() == nil {
		log.Warn("pairing stopped", "error", err.Error())
	}
}

// needsAutoPair reports whether the Worker should pair by itself: a Hub
// address, no enrollment and no invitation key (that one enrolls in the
// pre-start hook instead).
func needsAutoPair(ctx context.Context, s Settings, hubURL string, enr *Enrollments) bool {
	if hubURL == "" || enr == nil {
		return false
	}
	if s.Resolver != nil {
		if given, _, err := config.InviteKeyGiven(s.Resolver); err != nil || given {
			return false
		}
	}
	_, enrolled, err := enr.Load(ctx)
	return err == nil && !enrolled
}

// connectorHealth is the crash guard's view of the connector: connected, or
// idle on purpose because the operator quarantined the Hub, or because the
// Worker is not enrolled (the pairing was removed here with --forget or by the
// Hub, or it waits to be paired). Not enrolled is a deliberate state of a
// working binary that read its enrollment: the update is committed, since a
// rollback would restore the pre-migration database and lose a pairing made
// meanwhile. An unreachable Hub (backoff) stays healthNone.
func connectorHealth(st Status, _ string) updateHealth {
	switch {
	case st.State == ConnConnected:
		return healthConnected
	case st.State == ConnIdle && (st.Reason == ReasonQuarantined || st.Reason == ReasonNotEnrolled):
		return healthTrustHold
	}
	return healthNone
}
