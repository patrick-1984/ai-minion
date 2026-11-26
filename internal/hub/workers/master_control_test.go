package workers

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

type masterGateBackend struct {
	on     Online
	stream OpStream
	opened chan []string
	goOn   chan struct{}
}

func (b *masterGateBackend) Get(id string) (Online, bool) { return b.on, id == b.on.WorkerID }

func (b *masterGateBackend) OpenOp(ctx context.Context, id, _ string, check func(Online) error, features ...string) (OpStream, error) {
	if id != b.on.WorkerID {
		return nil, proto.NewError(proto.CodeNotFound, "no target")
	}
	if check != nil {
		if err := check(b.on); err != nil {
			return nil, err
		}
	}
	b.opened <- features
	select {
	case <-b.goOn:
	case <-ctx.Done():
	}
	// Even a backend that succeeded concurrently with cancellation cannot
	// bypass the final admission fence.
	return b.stream, nil
}

type masterGateStream struct {
	*fakeStream
	sent    chan struct{}
	release chan struct{}
}

func (s *masterGateStream) WriteFrame(v any) error {
	if err := s.fakeStream.WriteFrame(v); err != nil {
		return err
	}
	close(s.sent)
	return nil
}

func (s *masterGateStream) ReadFrame() (json.RawMessage, error) {
	<-s.release
	return s.fakeStream.ReadFrame()
}

type masterRig struct {
	master  *MasterControl
	source  store.WorkerRow
	target  store.WorkerRow
	lane    *masterLane
	backend *masterGateBackend
	stream  *masterGateStream
	now     time.Time
}

func newMasterRig(t *testing.T) *masterRig {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := t.Context()
	if _, err := st.DB().ExecContext(ctx, `INSERT INTO users(id,username,created_at,updated_at) VALUES('owner','owner',1,1);
	 INSERT INTO user_roles(user_id,role,source) VALUES('owner','user','local');`); err != nil {
		t.Fatal(err)
	}
	source := store.WorkerRow{ID: ops.NewUUIDv7(), OwnerID: "owner", Name: "source", Kind: store.WorkerKindPrivate, Pub: bytes.Repeat([]byte{8}, 32)}
	target := store.WorkerRow{ID: ops.NewUUIDv7(), OwnerID: "owner", Name: "target", Kind: store.WorkerKindPrivate, Pub: bytes.Repeat([]byte{9}, 32)}
	registry := NewRegistry()
	events := NewEventHub(EventHubOptions{Share: st})
	var online []Online
	for i, w := range []store.WorkerRow{source, target} {
		if err := st.InsertWorker(ctx, nil, w); err != nil {
			t.Fatal(err)
		}
		on := Online{WorkerID: w.ID, OwnerID: w.OwnerID, Name: w.Name, MasterSessionV1: i == 0,
			TranscriptHash: bytes.Repeat([]byte{byte(i + 1)}, 32), EnrollmentKey: [32]byte(w.Pub)}
		registry.Add(on, newFakeSession())
		online = append(online, on)
		link := events.Connect(w.ID, w.Name)
		f, err := link.NewFeed()
		if err != nil {
			t.Fatal(err)
		}
		name := "prepared"
		if i == 1 {
			name = "build"
		}
		if err := f.SnapshotShare(0, []SessionState{{Name: name, Kind: "agent:claude"}},
			&ops.ShareDecl{Rule: ops.ShareRule{Mode: "all"}, Revision: 1}); err != nil {
			t.Fatal(err)
		}
	}

	dir := &fakeDir{recs: []WorkerRecord{{ID: source.ID, Name: source.Name, OwnerID: source.OwnerID, Fingerprint: "source-fp"},
		{ID: target.ID, Name: target.Name, OwnerID: target.OwnerID, Fingerprint: "target-fp"}}}
	stream := &masterGateStream{fakeStream: newFakeStream(resultFrame(t, ops.WriteResult{Outcome: ops.OutcomeDelivered})),
		sent: make(chan struct{}), release: make(chan struct{})}
	backend := &masterGateBackend{on: online[1], stream: stream, opened: make(chan []string, 1), goOn: make(chan struct{})}
	az := authz.New(fakeUsers{"owner": {Active: true, Role: authz.RoleUser}}, dir)
	rt, err := NewRouter(RouterConfig{Backend: backend, Directory: dir, Authz: az})
	if err != nil {
		t.Fatal(err)
	}
	m := NewMasterControl(st, registry, events, rt, nil)
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	m.now = func() time.Time { return now }
	lctx, end := context.WithCancel(ctx)
	t.Cleanup(end)
	lane := &masterLane{info: online[0], ctx: lctx, end: end, leases: map[string]masterLease{},
		updates: make(chan proto.MasterState, 1), pending: map[*masterPending]struct{}{}}
	report, _ := json.Marshal(proto.MasterReport{Type: proto.TypeMasterOpen, Seq: 1,
		Candidates: []proto.MasterCandidate{{SessionName: "prepared", IncarnationID: "first", Adapter: proto.MasterAdapterClaude}}})
	if !m.applyReport(lane, report, true) {
		t.Fatal("verified report was refused")
	}
	r := &masterRig{master: m, source: source, target: target, lane: lane, backend: backend, stream: stream, now: now}
	if _, err := m.SetBoundMaster(ctx, source, r.change(0, true), "owner"); err != nil {
		t.Fatal(err)
	}
	return r
}

func (r *masterRig) change(revision uint64, active bool) ops.ControlSetBoundMasterArgs {
	return ops.ControlSetBoundMasterArgs{WriteArgs: ops.WriteArgs{RequestID: ops.NewUUIDv7()},
		WorkerID: r.source.ID, SessionName: "prepared", IncarnationID: "first", IfGeneration: &revision, Master: active}
}

func (r *masterRig) call(op string) Call {
	b := r.lane.leases["first"].binding
	return Call{Worker: r.source, TranscriptHash: r.lane.info.TranscriptHash, RequestID: ops.NewUUIDv7(),
		Master: &proto.MasterCallArgs{Generation: b.Generation, IncarnationID: b.IncarnationID, LeaseID: b.LeaseID,
			Operation: op, Arguments: json.RawMessage(`{}`)}}
}

func TestMasterRevocationOrdersFinalDispatchAndPreservesAdmittedOutcome(t *testing.T) {
	for _, revokeFirst := range []bool{true, false} {
		t.Run(map[bool]string{true: "revoke before admission", false: "admit before revoke"}[revokeFirst], func(t *testing.T) {
			r := newMasterRig(t)
			m := r.master
			ctx, cancel := context.WithTimeout(t.Context(), 4*time.Second)
			defer cancel()
			if out, err := m.runMasterCall(ctx, r.call(ops.OpWorkersList)); err != nil || len(out.(ops.WorkersListResult).Workers) != 0 {
				t.Fatalf("unconsenting targets or implicit own-Worker discovery: %+v, %v", out, err)
			}
			if err := m.db.PutAccept(ctx, store.AcceptRow{TargetWorkerID: r.target.ID, SourceWorkerID: r.source.ID, Sessions: []string{"build"}}); err != nil {
				t.Fatal(err)
			}
			c := r.call(ops.OpSendText)
			c.Master.Arguments, _ = json.Marshal(ops.SendTextArgs{WriteArgs: ops.WriteArgs{RequestID: c.RequestID},
				Target: ops.Target{WorkerID: r.target.ID, SessionName: "build"}, Text: "Check the test result", Submit: true})
			type answer struct {
				value any
				err   error
			}
			done := make(chan answer, 1)
			go func() {
				value, err := m.runMasterCall(ctx, c)
				done <- answer{value, err}
			}()
			select {
			case features := <-r.backend.opened:
				if !slices.Contains(features, proto.FeatureSendTextAgentOnly) {
					t.Fatal("destination was not required to support the agent-only guard")
				}
			case got := <-done:
				t.Fatalf("call ended before the dispatch barrier: %+v", got)
			case <-ctx.Done():
				t.Fatal("did not reach destination stream open")
			}
			if !revokeFirst {
				close(r.backend.goOn)
				select {
				case <-r.stream.sent:
				case <-ctx.Done():
					t.Fatal("the request did not reach the destination")
				}
			}
			if _, err := m.SetBoundMaster(ctx, r.source, r.change(1, false), "owner"); err != nil {
				t.Fatal(err)
			}
			if _, err := m.SetBoundMaster(ctx, r.source, r.change(2, true), "owner"); err != nil {
				t.Fatal(err)
			}
			if revokeFirst {
				close(r.backend.goOn)
			}
			close(r.stream.release)
			got := <-done
			if revokeFirst {
				if !errors.Is(got.err, errMasterDenied) || !r.stream.wasReset() || len(r.stream.written) != 0 {
					t.Fatalf("a call reached the target after committed revoke: %+v, frames=%d", got, len(r.stream.written))
				}
			} else {
				w, ok := got.value.(ops.WriteResult)
				if got.err != nil || !ok || w.Outcome != ops.OutcomeDelivered {
					t.Fatalf("an already admitted outcome was lost to revocation: %+v", got)
				}
				request := r.stream.request(t)
				var args ops.SendTextArgs
				if err := json.Unmarshal(request.Args, &args); err != nil || !args.AgentOnly || args.Force || !args.Submit ||
					request.RequestID != c.RequestID || !request.Origin.Grant {
					t.Fatalf("destination frame omitted the scoped delivery guard: %+v, %+v", request, args)
				}
				// A nomination-only master: the bound master principal keeps no
				// session, the admission names the lease's one, verified.
				if o := request.Origin; o.SourceSession != "prepared" || o.SourceProof != proto.OriginProofVerified || o.GrantID != "" {
					t.Fatalf("destination frame lost the verified source: %+v", o)
				}
			}
			if _, err := m.runMasterCall(ctx, c); !errors.Is(err, errMasterDenied) {
				t.Fatalf("old handle worked after identical re-nomination: %v", err)
			}
		})
	}
}

func TestMasterFinalAdmissionDoesNotOutliveLeaseWhileStoreWaits(t *testing.T) {
	r := newMasterRig(t)
	m := r.master
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Second)
	defer cancel()
	if err := m.db.PutAccept(ctx, store.AcceptRow{TargetWorkerID: r.target.ID, SourceWorkerID: r.source.ID}); err != nil {
		t.Fatal(err)
	}

	busy, release := make(chan struct{}), make(chan struct{})
	writer := make(chan error, 1)
	go func() {
		writer <- m.db.Tx(ctx, func(*sql.Tx) error {
			close(busy)
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
	}()
	select {
	case <-busy:
	case <-ctx.Done():
		t.Fatal("fixture writer did not acquire the SQLite lock")
	}

	var elapsed atomic.Int64
	checked := make(chan struct{}, 1)
	m.now = func() time.Time {
		current := r.now.Add(time.Duration(elapsed.Load()))
		select {
		case checked <- struct{}{}:
		default:
		}
		return current
	}
	call := r.call(ops.OpSendText)
	admitted := make(chan error, 1)
	go func() { _, err := m.admitMaster(ctx, call, nil, r.backend.on, "build", masterAuth{}); admitted <- err }()
	select {
	case <-checked:
	case <-ctx.Done():
		t.Fatal("the admission precheck was not reached")
	}
	elapsed.Store(int64(proto.MasterLeaseTTL + time.Second))
	close(release)
	if err := <-writer; err != nil {
		t.Fatal(err)
	}
	if err := <-admitted; !errors.Is(err, errMasterDenied) {
		t.Fatalf("a request passed final admission after its source lease expired: %v", err)
	}
}
