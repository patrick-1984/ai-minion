//go:build a77_native && linux

package fixtures

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/transport"
	"github.com/patrick-1984/ai-minion/internal/worker/agent"
	wstore "github.com/patrick-1984/ai-minion/internal/worker/store"
)

type fixedMasterSource struct{}

func (fixedMasterSource) Observe(context.Context) (agent.View, error) {
	return agent.View{"prepared": {Name: "prepared", Kind: "agent:claude", State: "stable"}}, nil
}

func (s fixedMasterSource) ObserveShare(ctx context.Context) (agent.View, ops.ShareDecl, error) {
	view, err := s.Observe(ctx)
	return view, ops.ShareDecl{Rule: ops.ShareRule{Mode: "all"}, Revision: 1}, err
}

func (fixedMasterSource) ShareDecl(context.Context) (ops.ShareDecl, error) {
	return ops.ShareDecl{Rule: ops.ShareRule{Mode: "all"}, Revision: 1}, nil
}

// This client deliberately retains old authenticated state instead of using
// the native helper's normal revocation path. It talks to the default Hub
// with a real fixture enrollment and real pinned encrypted links.
func TestProductionHubRejectsStaleMasterLease(t *testing.T) {
	root, err := os.MkdirTemp("", "a77-lease-")
	must(t, err)
	t.Cleanup(func() { must(t, os.RemoveAll(root)) })
	hub := startProductionHub(t, root)
	source := seedProductionWorker(t, hub, root, "retaining-source")
	st, err := wstore.Open(source.state)
	must(t, err)
	t.Cleanup(func() { _ = st.Close() })
	enrollment := agent.NewEnrollments(st, source.state)
	must(t, st.PrepareUsageIntent(t.Context(), hub.url, ""))
	pub, err := agent.NewEventPublisher(agent.EventConfig{Observer: fixedMasterSource{}, Interval: time.Hour})
	must(t, err)
	connector, err := agent.NewConnector(agent.ConnectorConfig{
		HubURL: hub.url, Name: source.name, Enrollment: enrollment, Key: enrollment.LoadKey, Dialer: agent.NewHubDialer(),
		Features:           func() []string { return []string{proto.FeatureMasterSessionV1} },
		Backoff:            &transport.Backoff{Initial: 10 * time.Millisecond, Max: 80 * time.Millisecond},
		ConsumeUsageIntent: st.ConsumeUsageIntent, UsageConsent: st.UsageConsent, UsageConnectionAllowed: st.UsageConnectionAllowed,
	})
	must(t, err)
	links := make(chan *agent.Link, 4)
	connector.OnLink("events", pub.Serve)
	connector.OnLink("receive-test-link", func(l *agent.Link) {
		select {
		case links <- l:
		case <-l.Context().Done():
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		connector.Run(ctx)
	}()
	t.Cleanup(func() { cancel(); <-done })

	const incarnation = "wire-fixture-incarnation"
	nextLane := func() (*agent.Link, *transport.Stream) {
		t.Helper()
		select {
		case link := <-links:
			if !link.Welcome.MasterSessionV1 {
				t.Fatal("the actual Hub failed to negotiate the master capability")
			}
			events, err := link.Session.OpenEvents(t.Context())
			must(t, err)
			must(t, events.WriteFrame(proto.MasterReport{Type: proto.TypeMasterOpen, Seq: 1, Candidates: []proto.MasterCandidate{
				{SessionName: "prepared", IncarnationID: incarnation, Adapter: proto.MasterAdapterClaude},
			}}))
			return link, events
		case <-time.After(10 * time.Second):
			t.Fatalf("fixture encrypted link did not arrive: %+v", connector.Status())
			return nil, nil
		}
	}
	link, events := nextLane()
	var sourceState ops.ControlWorker
	eventually(t, "report and ordinary sharing agree", func() bool {
		sourceState = hub.control(t, source.id)
		return len(sourceState.MasterCandidates) == 1
	}, func() string { return "missing real Hub candidate" })
	candidate := productionCandidate(t, sourceState, "prepared")
	hub.setMaster(t, source.id, candidate, true)
	old := readFixtureLease(t, events)
	expectMasterWire(t, link, old, true)

	active := productionCandidate(t, hub.control(t, source.id), "prepared")
	hub.setMaster(t, source.id, active, false)
	// The fixture deliberately has not read any revocation frame.
	expectMasterWire(t, link, old, false)
	revoked := productionCandidate(t, hub.control(t, source.id), "prepared")
	hub.setMaster(t, source.id, revoked, true)
	expectMasterWire(t, link, old, false)
	fresh := readFixtureLease(t, events)
	if fresh.Generation == old.Generation || fresh.LeaseID == old.LeaseID {
		t.Fatal("re-nomination reused the original capability")
	}
	expectMasterWire(t, link, fresh, true)

	if !hub.service.Registry().Disconnect(source.id) {
		t.Fatal("failed to disconnect the known fixture link")
	}
	link, events = nextLane()
	eventually(t, "replacement link re-proves sharing", func() bool {
		return len(hub.control(t, source.id).MasterCandidates) == 1
	}, func() string { return "replacement link did not have an ordinary share view" })
	must(t, events.WriteFrame(proto.MasterReport{Type: proto.TypeMasterReport, Seq: 2, Candidates: []proto.MasterCandidate{
		{SessionName: "prepared", IncarnationID: incarnation, Adapter: proto.MasterAdapterClaude},
	}}))
	reconnected := readFixtureLease(t, events)
	if reconnected.Generation != fresh.Generation || reconnected.LeaseID == fresh.LeaseID {
		t.Fatal("replacement link did not rebind the durable nomination")
	}
	expectMasterWire(t, link, fresh, false)
	expectMasterWire(t, link, reconnected, true)
	t.Log("production Hub rejects cached leases after revoke, identical re-nomination and real link replacement")

	// Deliberately omit the recurring report. The real authority must expire
	// even though the Worker link remains up and this client kept the handle.
	expiry := time.NewTimer(proto.MasterLeaseTTL + 50*time.Millisecond)
	defer expiry.Stop()
	select {
	case <-expiry.C:
	case <-t.Context().Done():
		t.Fatal("the fixture stopped before lease expiry")
	}
	expectMasterWire(t, link, reconnected, false)
	if link.Context().Err() != nil {
		t.Fatal("the production Hub refused a disconnected link, not an expired lease")
	}
	t.Log("production Hub rejects the expired lease on an otherwise live link")
}

func readFixtureLease(t *testing.T, events *transport.Stream) proto.MasterBinding {
	t.Helper()
	must(t, events.SetReadDeadline(time.Now().Add(8*time.Second)))
	for {
		body, err := events.ReadFrame()
		must(t, err)
		var state proto.MasterState
		must(t, proto.DecodeMasterJSON(body, &state))
		must(t, state.Validate())
		if len(state.Active) == 1 {
			return state.Active[0]
		}
	}
}

func expectMasterWire(t *testing.T, link *agent.Link, binding proto.MasterBinding, allowed bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	stream, err := link.Session.OpenCall(ctx)
	must(t, err)
	defer stream.Reset()
	must(t, stream.SetReadDeadline(time.Now().Add(5*time.Second)))
	must(t, stream.WriteFrame(proto.MasterCallRequest{
		Type: proto.TypeOpRequest, Op: proto.OpMasterCallV1, RequestID: ops.NewUUIDv7(),
		Args: proto.MasterCallArgs{IncarnationID: binding.IncarnationID, Generation: binding.Generation,
			LeaseID: binding.LeaseID, Operation: ops.OpWorkersList, Arguments: json.RawMessage(`{}`)},
	}))
	body, err := stream.ReadFrame()
	must(t, err)
	kind, err := proto.FrameType(body)
	must(t, err)
	if allowed && kind == proto.TypeOpResult {
		return
	}
	if !allowed && kind == proto.TypeError {
		var response proto.ErrorFrame
		must(t, json.Unmarshal(body, &response))
		if response.Code == proto.CodeForbidden || response.Code == proto.CodeUnauthenticated {
			return
		}
		t.Fatalf("stale authenticated attachment failed outside authorization: %s", response.Code)
	}
	t.Fatalf("authenticated attachment received %q, want allowed=%t", kind, allowed)
}
