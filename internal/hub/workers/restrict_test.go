package workers

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/transport"
)

// connectOnline dials the Hub as the Worker of key, reads welcome and starts
// the Worker side of the session. The session is closed at the end of the test.
func connectOnline(t *testing.T, h *testHub, key *identity.Key) (Welcome, *transport.Session) {
	t.Helper()
	res, err := h.dial(h.workerConfig(key, "send_text", "sessions_list"))
	if err != nil {
		t.Fatal(err)
	}
	w := decodeWelcome(t, res)
	sess, err := transport.NewSession(res.Channel, transport.RoleWorker, transport.SessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	return w, sess
}

func TestRestrictDeliversPreviewOnlyInWelcomeAndClosesTheOnlineConnection(t *testing.T) {
	h := newTestHub(t, nil)
	rs, err := NewRestrictions(RestrictionsConfig{Store: h.st, Connections: h.svc.Registry(), Auditor: h.audit})
	if err != nil {
		t.Fatal(err)
	}
	h.svc.UsePolicy(rs.Policy)
	// The post-registration admission recheck (connect.go) ends a connection
	// whose policy changed since its welcome. Each connection signals when its
	// recheck is over, and the test waits for it before it changes the policy,
	// so that Restrict's own Disconnect, not the recheck, ends the connection.
	rechecked := make(chan struct{}, 8)
	h.svc.Handler().afterRecheck = func() { rechecked <- struct{}{} }
	awaitRecheck := func() {
		t.Helper()
		select {
		case <-rechecked:
		case <-timeout(t):
			t.Fatal("the admission recheck did not finish")
		}
	}
	key := newKey(t)
	w := h.enrol(key, "owner-1", "pi")
	actor := Actor{ID: "owner-1"}

	welcome, sess := connectOnline(t, h, key)
	if welcome.Policy.PreviewOnly {
		t.Fatalf("a new Worker is restricted: %+v", welcome)
	}
	eventually(t, "the Worker to be online", func() bool { return h.svc.Registry().IsOnline(w.ID) })
	awaitRecheck()

	// Restricting closes the connection, so that the reconnect re-reads it.
	res, err := rs.Restrict(context.Background(), actor, w.ID, true)
	if err != nil || !res.Changed || !res.Disconnected {
		t.Fatalf("Restrict = %+v, %v", res, err)
	}
	select {
	case <-sess.Done():
	case <-timeout(t):
		t.Fatal("the connection of the restricted Worker was not closed")
	}
	eventually(t, "the Worker to go offline", func() bool { return !h.svc.Registry().IsOnline(w.ID) })

	welcome, sess = connectOnline(t, h, key)
	if !welcome.Policy.PreviewOnly || welcome.WorkerID != w.ID {
		t.Fatalf("welcome after the restriction = %+v", welcome)
	}
	eventually(t, "the Worker to be online again", func() bool { return h.svc.Registry().IsOnline(w.ID) })
	awaitRecheck()

	// The same policy again changes nothing and keeps the connection.
	res, err = rs.Restrict(context.Background(), actor, w.ID, true)
	if err != nil || res.Changed || res.Disconnected {
		t.Fatalf("repeat Restrict = %+v, %v", res, err)
	}
	select {
	case <-sess.Done():
		t.Fatal("a call that changed nothing closed the connection")
	default:
	}

	// Lifting the Hub restriction is a change too: the Worker reconnects free.
	res, err = rs.Restrict(context.Background(), actor, w.ID, false)
	if err != nil || !res.Changed || !res.Disconnected {
		t.Fatalf("lift = %+v, %v", res, err)
	}
	eventually(t, "the Worker to go offline", func() bool { return !h.svc.Registry().IsOnline(w.ID) })
	if welcome, _ = connectOnline(t, h, key); welcome.Policy.PreviewOnly {
		t.Fatalf("welcome after lifting = %+v", welcome)
	}
}

func TestRestrictOfAnOfflineWorkerIsStoredForTheNextConnect(t *testing.T) {
	h := newTestHub(t, nil)
	rs, err := NewRestrictions(RestrictionsConfig{Store: h.st, Connections: h.svc.Registry()})
	if err != nil {
		t.Fatal(err)
	}
	h.svc.UsePolicy(rs.Policy)
	key := newKey(t)
	w := h.enrol(key, "owner-1", "pi")
	res, err := rs.Restrict(context.Background(), Actor{ID: "admin"}, w.ID, true)
	if err != nil || !res.Changed || res.Disconnected {
		t.Fatalf("Restrict = %+v, %v", res, err)
	}
	if welcome, _ := connectOnline(t, h, key); !welcome.Policy.PreviewOnly {
		t.Fatalf("welcome = %+v", welcome)
	}
}

func TestRestrictAuditsTheChangeWithoutContent(t *testing.T) {
	r := newKindRig(t)
	w := r.worker(r.u1, "pi", store.WorkerKindPrivate)
	if _, err := r.rs.Restrict(context.Background(), Actor{ID: r.admin, ActingAs: r.u1}, w.ID, true); err != nil {
		t.Fatal(err)
	}
	rows := r.auditRows(AuditWorkerRestrict)
	if len(rows) != 1 || rows[0].Actor != r.admin || rows[0].ActingAs != r.u1 || rows[0].Outcome != "success" ||
		rows[0].Target != w.ID+" preview_only=true" {
		t.Errorf("audit = %+v", rows)
	}
	// A repeat changes nothing and is not audited again.
	_, _ = r.rs.Restrict(context.Background(), Actor{ID: r.admin}, w.ID, true)
	if rows := r.auditRows(AuditWorkerRestrict); len(rows) != 1 {
		t.Errorf("audit rows after a repeat = %d", len(rows))
	}
}

func TestRestrictRefusals(t *testing.T) {
	r := newKindRig(t)
	_, err := r.rs.Restrict(context.Background(), r.adminActor, store.NewID(), true)
	wantCode(t, err, proto.CodeNotFound)
	w := r.worker(r.u1, "pi", store.WorkerKindPrivate)
	_, err = r.rs.Restrict(context.Background(), Actor{}, w.ID, true)
	wantCode(t, err, proto.CodeForbidden)
	if on, _ := r.st.WorkerPreviewOnly(context.Background(), w.ID); on {
		t.Error("a refused call restricted the Worker")
	}
}

// The Hub only adds a restriction. Whatever it delivers, the Worker keeps its
// local policy (AD-12, enforced on the Worker, tested there); here the Hub side
// is checked to carry nothing but the one flag.
func TestPolicyFrameCarriesOnlyThePreviewOnlyFlag(t *testing.T) {
	r := newKindRig(t)
	w := r.worker(r.u1, "pi", store.WorkerKindPrivate)
	if _, err := r.rs.Restrict(context.Background(), r.adminActor, w.ID, true); err != nil {
		t.Fatal(err)
	}
	pol, err := r.rs.Policy(context.Background(), w)
	if err != nil || !pol.PreviewOnly {
		t.Fatalf("Policy = %+v, %v", pol, err)
	}
	if _, err := r.rs.Policy(context.Background(), store.WorkerRow{ID: store.NewID()}); err == nil {
		t.Error("a policy was served for an unknown Worker")
	}
	if !strings.Contains(string(mustJSON(t, Welcome{Policy: pol})), `"policy":{"preview_only":true}`) {
		t.Error("the welcome policy has another shape")
	}
}

func timeout(t *testing.T) <-chan time.Time {
	t.Helper()
	return time.After(10 * time.Second)
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// A restriction that commits between the policy read of a connecting Worker
// and its registration finds no connection to close, and the welcome already
// carries the old policy. The connection must end by itself, so that the
// reconnect reads the stored policy; repeating the restriction would change
// nothing and could not recover it.
func TestRestrictCommittingBetweenThePolicyReadAndTheRegistrationEndsTheConnection(t *testing.T) {
	h := newTestHub(t, nil)
	rs, err := NewRestrictions(RestrictionsConfig{Store: h.st, Connections: h.svc.Registry(), Auditor: h.audit})
	if err != nil {
		t.Fatal(err)
	}
	key := newKey(t)
	w := h.enrol(key, "owner-1", "pi")
	actor := Actor{ID: "owner-1"}

	var res RestrictResult
	var restrictErr error
	armed := true
	h.svc.UsePolicy(func(ctx context.Context, row store.WorkerRow) (Policy, error) {
		pol, err := rs.Policy(ctx, row) // reads preview_only=false
		if armed {
			armed = false
			// The restriction commits right after the read and before
			// Registry.Add: the Worker is not online yet.
			res, restrictErr = rs.Restrict(context.Background(), actor, w.ID, true)
		}
		return pol, err
	})

	welcome, sess := connectOnline(t, h, key)
	if restrictErr != nil || !res.Changed || res.Disconnected {
		t.Fatalf("Restrict between read and registration = %+v, %v; want changed and no connection to close", res, restrictErr)
	}
	if welcome.Policy.PreviewOnly {
		t.Fatalf("test setup: the first welcome was meant to carry the stale policy: %+v", welcome)
	}
	select {
	case <-sess.Done():
	case <-timeout(t):
		t.Fatal("the connection admitted with a stale policy stayed up")
	}
	eventually(t, "the stale connection to leave the registry", func() bool { return !h.svc.Registry().IsOnline(w.ID) })

	welcome, _ = connectOnline(t, h, key)
	if !welcome.Policy.PreviewOnly {
		t.Fatalf("the reconnect did not get the stored policy: %+v", welcome)
	}
	eventually(t, "the Worker to be online again", func() bool { return h.svc.Registry().IsOnline(w.ID) })
}
