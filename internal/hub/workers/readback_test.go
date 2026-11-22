package workers

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/transport"
)

// rbWorker is the Worker end of a real transport session: it accepts the op
// streams the Hub opens and answers each frame with answer.
type rbWorker struct {
	ws     *transport.Session
	frames chan json.RawMessage
}

func newRBPair(t *testing.T, answer func(json.RawMessage) any) (*transport.Session, *rbWorker) {
	t.Helper()
	hubSide, workerSide := net.Pipe()
	hs, err := transport.NewSession(hubSide, transport.RoleHub, transport.SessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ws, err := transport.NewSession(workerSide, transport.RoleWorker, transport.SessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ws.Close(); _ = hs.Close() })
	w := &rbWorker{ws: ws, frames: make(chan json.RawMessage, 16)}
	go func() {
		for {
			st, err := ws.Accept(context.Background())
			if err != nil {
				return
			}
			go func() {
				body, err := st.ReadFrame()
				if err != nil {
					return
				}
				w.frames <- body
				if a := answer(body); a != nil {
					_ = st.WriteFrame(a)
					_ = st.Close()
				} // nil: never answers
			}()
		}
	}()
	return hs, w
}

func ackFor(body json.RawMessage) any {
	var f proto.ReadbackApply
	_ = json.Unmarshal(body, &f)
	res, _ := json.Marshal(proto.ReadbackAck{Revision: f.Revision})
	return OpResult{Type: proto.TypeOpResult, Result: res}
}

func rbOnline(id, owner string, transcriptByte byte) Online {
	root, _, _ := ed25519.GenerateKey(rand.Reader)
	tr := make([]byte, 32)
	for i := range tr {
		tr[i] = transcriptByte
	}
	return Online{WorkerID: id, OwnerID: owner, Kind: "private", Features: []string{proto.FeatureUsageReadbackV1}, TranscriptHash: tr, HubRoot: root}
}

func rbFrame(w Online, owner string, rev uint64) proto.ReadbackApply {
	return proto.ReadbackApply{Type: proto.TypeReadbackApply, Revision: rev, OwnerID: owner, WorkerID: w.WorkerID, HubKey: "k", Doc: json.RawMessage(`{"schema":"minion.usage-meters/1"}`)}
}

func TestSendReadbackDeliversTheBoundFrameOnTheExactConnection(t *testing.T) {
	hs, w := newRBPair(t, ackFor)
	r := NewRegistry()
	on := rbOnline("w1", "alice", 1)
	r.Add(on, hs)
	ack, err := r.SendReadback(context.Background(), on, rbFrame(on, "alice", 3))
	if err != nil || ack.Revision != 3 {
		t.Fatalf("ack = %+v, %v", ack, err)
	}
	var got proto.ReadbackApply
	if err := json.Unmarshal(<-w.frames, &got); err != nil || got.Type != proto.TypeReadbackApply || got.OwnerID != "alice" || got.WorkerID != "w1" || got.Revision != 3 {
		t.Fatalf("the Worker received %+v (%v)", got, err)
	}
}

func TestSendReadbackIsFencedToTheCurrentApprovedConnection(t *testing.T) {
	cases := map[string]func(r *Registry, on *Online, hs *transport.Session) (frameOwner string){
		"unknown worker": func(r *Registry, on *Online, _ *transport.Session) string { on.WorkerID = "ghost"; return "alice" },
		"replaced connection (another handshake)": func(r *Registry, on *Online, _ *transport.Session) string {
			hs2, _ := newRBPair(t, ackFor)
			r.Add(rbOnline("w1", "alice", 2), hs2)
			return "alice"
		},
		"reassigned owner on the live connection": func(*Registry, *Online, *transport.Session) string { return "bob" },
		"worker without the feature": func(r *Registry, on *Online, hs *transport.Session) string {
			n := rbOnline("w1", "alice", 1)
			n.Features = nil
			r.Add(n, hs)
			return "alice"
		},
		"pending approval": func(r *Registry, on *Online, hs *transport.Session) string {
			n := rbOnline("w1", "alice", 1)
			n.Pending = true
			r.Add(n, hs)
			on.Pending = true
			return "alice"
		},
		"ended session": func(_ *Registry, _ *Online, hs *transport.Session) string { _ = hs.Close(); return "alice" },
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			hs, w := newRBPair(t, ackFor)
			r := NewRegistry()
			on := rbOnline("w1", "alice", 1)
			r.Add(on, hs)
			owner := setup(r, &on, hs)
			_, err := r.SendReadback(context.Background(), on, rbFrame(on, owner, 1))
			if !errors.Is(err, ErrReadbackFenced) {
				t.Fatalf("err = %v, want ErrReadbackFenced", err)
			}
			select {
			case f := <-w.frames:
				t.Fatalf("a frame reached the Worker: %s", f)
			case <-time.After(50 * time.Millisecond):
			}
		})
	}
}

func TestSendReadbackReportsRefusalsLostAnswersAndWrongAcks(t *testing.T) {
	on := rbOnline("w1", "alice", 1)
	t.Run("refusal carries its reason", func(t *testing.T) {
		hs, _ := newRBPair(t, func(json.RawMessage) any {
			return proto.ReadbackRefusal(proto.CodeConflict, proto.ReadbackStale, "stale").Frame()
		})
		r := NewRegistry()
		r.Add(on, hs)
		_, err := r.SendReadback(context.Background(), on, rbFrame(on, "alice", 1))
		if proto.ReadbackReason(err) != proto.ReadbackStale || proto.CodeOf(err) != proto.CodeConflict {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("lost answer ends at the deadline", func(t *testing.T) {
		hs, _ := newRBPair(t, func(json.RawMessage) any { return nil })
		r := NewRegistry()
		r.Add(on, hs)
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer cancel()
		start := time.Now()
		if _, err := r.SendReadback(ctx, on, rbFrame(on, "alice", 1)); err == nil || time.Since(start) > 3*time.Second {
			t.Fatalf("err = %v after %v", err, time.Since(start))
		}
	})
	t.Run("an ack of another revision is not an ack", func(t *testing.T) {
		hs, _ := newRBPair(t, func(json.RawMessage) any {
			res, _ := json.Marshal(proto.ReadbackAck{Revision: 99})
			return OpResult{Type: proto.TypeOpResult, Result: res}
		})
		r := NewRegistry()
		r.Add(on, hs)
		if _, err := r.SendReadback(context.Background(), on, rbFrame(on, "alice", 1)); err == nil {
			t.Fatal("accepted")
		}
	})
	t.Run("a frame with a bound missing is not sent", func(t *testing.T) {
		hs, w := newRBPair(t, ackFor)
		r := NewRegistry()
		r.Add(on, hs)
		f := rbFrame(on, "alice", 1)
		f.HubKey = ""
		if _, err := r.SendReadback(context.Background(), on, f); err == nil {
			t.Fatal("accepted")
		}
		select {
		case <-w.frames:
			t.Fatal("sent")
		case <-time.After(30 * time.Millisecond):
		}
	})
}
