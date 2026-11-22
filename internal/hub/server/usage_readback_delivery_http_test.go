package server

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/client"
	"github.com/patrick-1984/ai-minion/internal/hub/api"
	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/hub/workers"
	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/transport"
)

// rbWorker is the Worker end of an in-process connection: it accepts the op
// streams the Hub opens and records the frames, acknowledging read-back frames.
type rbWorker struct {
	mu      sync.Mutex
	frames  []proto.ReadbackApply
	streams int
	// ackOnly, when set, answers revisions it likes; nil acknowledges all.
	refuse func(proto.ReadbackApply) *proto.Error
}

func (d *dashHub) connectReadbackWorker(wid, ownerID string, features []string, pending bool, refuse func(proto.ReadbackApply) *proto.Error) *rbWorker {
	d.t.Helper()
	hubSide, workerSide := net.Pipe()
	hs, err := transport.NewSession(hubSide, transport.RoleHub, transport.SessionOptions{})
	if err != nil {
		d.t.Fatal(err)
	}
	ws, err := transport.NewSession(workerSide, transport.RoleWorker, transport.SessionOptions{})
	if err != nil {
		d.t.Fatal(err)
	}
	d.t.Cleanup(func() { _ = ws.Close(); _ = hs.Close() })
	w := &rbWorker{refuse: refuse}
	go func() {
		for {
			st, err := ws.Accept(context.Background())
			if err != nil {
				return
			}
			go func() {
				w.mu.Lock()
				w.streams++
				w.mu.Unlock()
				body, err := st.ReadFrame()
				if err != nil {
					return
				}
				var f proto.ReadbackApply
				if typ, _ := proto.FrameType(body); typ != proto.TypeReadbackApply || json.Unmarshal(body, &f) != nil {
					_ = st.WriteFrame(proto.NewError(proto.CodeUnsupportedOp, "no").Frame())
					return
				}
				if w.refuse != nil {
					if e := w.refuse(f); e != nil {
						_ = st.WriteFrame(e.Frame())
						return
					}
				}
				w.mu.Lock()
				w.frames = append(w.frames, f)
				w.mu.Unlock()
				res, _ := json.Marshal(proto.ReadbackAck{Revision: f.Revision})
				_ = st.WriteFrame(workers.OpResult{Type: proto.TypeOpResult, Result: res})
				_ = st.Close()
			}()
		}
	}()
	root, _, _ := ed25519.GenerateKey(rand.Reader)
	tr := make([]byte, 32)
	_, _ = rand.Read(tr)
	d.wsv.Registry().Add(workers.Online{
		WorkerID: wid, Name: "w", OwnerID: ownerID, Kind: store.WorkerKindPrivate, Ops: []string{ops.OpSessionsList},
		Features: features, Pending: pending, TranscriptHash: tr, HubRoot: root,
	}, hs)
	return w
}

func (w *rbWorker) got() []proto.ReadbackApply {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]proto.ReadbackApply(nil), w.frames...)
}

func (w *rbWorker) opened() int { w.mu.Lock(); defer w.mu.Unlock(); return w.streams }

func rbEventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

var rbFeatures = []string{proto.FeatureUsageReadbackV1}

// Nominate on the Hub and the Worker is handed a schema-valid document bound
// to its owner, Worker and the Hub key; withdraw and an empty one follows at
// once. A Worker that does not announce the feature, one that waits for
// approval and one whose owner has nothing nominated hear nothing wrong.
func TestUsageReadbackDeliveryFromNominationToWithdrawal(t *testing.T) {
	d, _ := startUsageHTTPHub(t)
	alice := d.user("alice")
	bob := d.user("bob")
	hdr, _ := d.login("alice")
	call := func(op string, args any) string {
		t.Helper()
		code, body, _ := d.hc.do(http.MethodPost, ops.RESTPrefix+op, args, hdr)
		if code != http.StatusOK {
			t.Fatalf("%s: %d %s", op, code, body)
		}
		return body
	}
	var bucket ops.UsageBucketSetResult
	if err := json.Unmarshal([]byte(call(ops.OpUsageBucketSet, ops.UsageBucketSetArgs{Action: "save", Kind: "session", Label: "Team"})), &bucket); err != nil || bucket.ID == "" {
		t.Fatalf("bucket: %+v %v", bucket, err)
	}
	wid := d.enrol("alice-laptop", alice, store.WorkerKindPrivate)
	oldW := d.connectReadbackWorker(d.enrol("alice-old", alice, store.WorkerKindPrivate), alice, nil, false, nil)
	pendW := d.connectReadbackWorker(d.enrol("alice-pending", alice, store.WorkerKindPrivate), alice, rbFeatures, true, nil)
	bobW := d.connectReadbackWorker(d.enrol("bob-laptop", bob, store.WorkerKindPrivate), bob, rbFeatures, false, nil)
	w := d.connectReadbackWorker(wid, alice, rbFeatures, false, nil)

	// Admission: an empty document first (nothing nominated yet).
	rbEventually(t, "the first document", func() bool { return len(w.got()) >= 1 })
	first := w.got()[0]
	if first.Revision != 1 || first.OwnerID != alice || first.WorkerID != wid || first.HubKey == "" {
		t.Fatalf("first frame = %+v", first)
	}
	if doc := decodeMeters(t, string(first.Doc)); len(doc.Slots) != 0 {
		t.Fatalf("nothing is nominated yet: %+v", doc.Slots)
	}

	// Nominate: the next document carries the slot, immediately (no reconciliation wait).
	call(ops.OpUsageNominationSet, ops.UsageNominationSetArgs{Action: "nominate", Scope: "user", Kind: "session", BucketID: bucket.ID})
	rbEventually(t, "the nominated slot", func() bool { return len(w.got()) >= 2 })
	second := w.got()[1]
	doc := decodeMeters(t, string(second.Doc))
	if second.Revision != 2 || len(doc.Slots) != 1 || slotOf(t, doc, "user", "session").Slot.Label != "Team" {
		t.Fatalf("second frame = rev %d %s", second.Revision, second.Doc)
	}
	if len(second.Doc) > proto.MaxReadbackDocBytes {
		t.Fatal("document over the limit")
	}

	// Withdraw: an empty document replaces it.
	call(ops.OpUsageNominationSet, ops.UsageNominationSetArgs{Action: "clear", Scope: "user", Kind: "session"})
	rbEventually(t, "the withdrawal", func() bool { return len(w.got()) >= 3 })
	third := w.got()[2]
	if third.Revision != 3 || len(decodeMeters(t, string(third.Doc)).Slots) != 0 {
		t.Fatalf("third frame = rev %d %s", third.Revision, third.Doc)
	}

	// Bob's Worker gets Bob's document only; Alice's nomination never reached it.
	rbEventually(t, "Bob's first document", func() bool { return len(bobW.got()) >= 1 })
	for _, f := range bobW.got() {
		if f.OwnerID != bob || strings.Contains(string(f.Doc), "Team") {
			t.Fatalf("Bob's Worker received %+v", f)
		}
	}
	// An old Worker (no feature) and one that waits for approval are not touched.
	time.Sleep(100 * time.Millisecond)
	if oldW.opened() != 0 || pendW.opened() != 0 {
		t.Fatalf("streams opened to an old Worker (%d) or a pending one (%d)", oldW.opened(), pendW.opened())
	}
}

// The Worker refuses a binding: nothing is retried in a loop. A Worker whose
// first answer is lost sees the same revision again.
func TestUsageReadbackDeliveryDoesNotLoopOnABindingRefusal(t *testing.T) {
	d, _ := startUsageHTTPHub(t)
	alice := d.user("alice")
	var mu sync.Mutex
	n := 0
	d.connectReadbackWorker(d.enrol("w", alice, store.WorkerKindPrivate), alice, rbFeatures, false, func(proto.ReadbackApply) *proto.Error {
		mu.Lock()
		defer mu.Unlock()
		n++
		return proto.ReadbackRefusal(proto.CodeConflict, proto.ReadbackFenced, "another owner")
	})
	time.Sleep(300 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if n > 1 {
		t.Fatalf("%d attempts after a binding refusal", n)
	}
}

// Nothing but the Hub's own delivery reaches the receiver: not REST, not MCP
// (read-only or write-enabled), and it is in no catalog.
func TestUsageReadbackFrameIsNotReachableFromAnyCaller(t *testing.T) {
	dataDir := t.TempDir()
	d, keys := startUsageHTTPHubAt(t, dataDir)
	rootID := d.userID("root")
	root, _ := d.login("root")
	token := identity.NewAPIKey().String()
	keys.mu.Lock()
	keys.known[token] = api.KeyIdentity{KeyID: token, Principal: authz.NewAPIKeyPrincipal(rootID, nominationScope{actions: []ops.Action{ops.ActionRead, ops.ActionOperate, ops.ActionManage}, all: true}, "")}
	keys.mu.Unlock()
	names := []string{proto.TypeReadbackApply, "readback_apply", "worker_readback", "usage_readback_apply", "readback"}

	for _, n := range names {
		if op, ok := ops.Lookup(n); ok && op.Scope == ops.ScopeWorker {
			t.Fatalf("%q is a Worker op", n)
		}
		for _, h := range []map[string]string{root, asKey(token)} {
			code, body, _ := d.hc.do(http.MethodPost, ops.RESTPrefix+n, map[string]any{"revision": 1}, h)
			if code != http.StatusNotFound && code != http.StatusBadRequest && code != http.StatusNotImplemented {
				t.Fatalf("REST %s answered %d: %s", n, code, body)
			}
		}
	}
	b, err := client.NewHub(client.HubConfig{URL: "https://" + d.h.srv.Addr(), APIKeyFile: keyFile(t, token), CAFile: filepath.Join(dataDir, TLSDir, TLSCertFile)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(b.Close)
	for _, writes := range []bool{false, true} {
		cs := newMCPSession(t, b, writes)
		raw, err := cs.rpc("tools/list", map[string]any{})
		if err != nil {
			t.Fatal(err)
		}
		var list struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		}
		if json.Unmarshal(raw, &list) != nil || len(list.Tools) == 0 {
			t.Fatalf("tools/list: %s", raw)
		}
		for _, tool := range list.Tools {
			if strings.Contains(tool.Name, "readback") && tool.Name != ops.OpUsageReadback {
				t.Fatalf("MCP (writes=%v) offers %q", writes, tool.Name)
			}
		}
		for _, n := range names {
			if text, isErr, err := cs.callTool(n, map[string]any{"revision": 1}); err == nil && !isErr {
				t.Fatalf("MCP (writes=%v) served %q: %s", writes, n, text)
			}
		}
	}
}
