package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/netip"
	"strings"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/hub/audit"
	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/ops"
)

func newGrantHistoryStack(t *testing.T) *kindStack {
	t.Helper()
	k := newKindStack(t)
	cat := ops.NewRegistry()
	for _, n := range []string{ops.OpGrantList, ops.OpGrantSet, ops.OpGrantRevoke, ops.OpGrantApprove, ops.OpGrantHistory} {
		op, ok := ops.Lookup(n)
		if !ok {
			t.Fatalf("ops.Default lacks %s", n)
		}
		cat.Register(op)
	}
	handlers := NewRegistry(cat)
	list := func(ctx context.Context) ([]GrantWorker, error) {
		rows, err := k.st.ListWorkers(ctx)
		if err != nil {
			return nil, err
		}
		out := make([]GrantWorker, 0, len(rows))
		for _, w := range rows {
			out = append(out, GrantWorker{ID: w.ID, Name: w.Name, OwnerID: w.OwnerID, Approved: true, CanBeSource: true})
		}
		return out, nil
	}
	// The grant handlers write to the real audit log, which the history reads,
	// with the namer the Hub gives it.
	alog := audit.New(k.st, audit.WithNamer(hubAuditNamer(k.st)))
	if err := RegisterGrants(handlers, k.st, list, k.az, alog); err != nil {
		t.Fatal(err)
	}
	if err := RegisterGrantHistory(handlers, k.st, alog, list, k.az); err != nil {
		t.Fatal(err)
	}
	k.mux = http.NewServeMux()
	d := &Dispatcher{Catalog: cat, Handlers: handlers, Auth: k.authn, Authz: k.az, Audit: k.audit, Log: testLogger(&strings.Builder{})}
	if _, err := NewRouter(d, k.mux); err != nil {
		t.Fatal(err)
	}
	return k
}

// hubAuditNamer is the namer the Hub gives the audit log (storeNamer of
// server/mod_audit.go, which this package cannot import): a user id gives the
// user name; "<worker id>:<session>", "<worker id>/..." and a Worker id give
// the Worker name followed by the rest, the session name included.
func hubAuditNamer(st *store.Store) func(context.Context, string) string {
	return func(ctx context.Context, id string) string {
		id = strings.TrimPrefix(id, "worker:")
		rest := ""
		if i := strings.IndexAny(id, "/:"); i >= 0 {
			id, rest = id[:i], id[i:]
		}
		if !ops.ValidUUIDv7(id) {
			return ""
		}
		if u, err := st.UserByID(ctx, id); err == nil && rest == "" {
			return u.Username
		}
		if w, err := st.WorkerByID(ctx, id); err == nil {
			return w.Name + rest
		}
		return ""
	}
}

func (k *kindStack) grantHistory(id string) (int, []ops.AuditEvent) {
	k.t.Helper()
	code, body := k.call(ops.OpGrantHistory, map[string]any{"id": id})
	var res ops.GrantHistoryResult
	if code == http.StatusOK {
		if err := json.Unmarshal([]byte(body), &res); err != nil {
			k.t.Fatalf("grant_history = %s (%v)", body, err)
		}
	}
	return code, res.Events
}

// The owner of a Worker of the grant and an administrator read its history,
// also after the grant has ended; another user does not.
func TestGrantHistoryForOwnerAndAdministrator(t *testing.T) {
	k := newGrantHistoryStack(t)
	ctx := t.Context()
	a1, a2 := k.worker(k.u1, "a1", store.WorkerKindPrivate), k.worker(k.u1, "a2", store.WorkerKindPrivate)
	b1 := k.worker(k.u2, "b1", store.WorkerKindPrivate)
	if _, err := k.st.DB().ExecContext(ctx, `UPDATE workers SET approval='approved'`); err != nil {
		t.Fatal(err)
	}
	k.as(k.u1)
	g := k.setGrant(grantArgs(a1, a2, ops.GrantLevelView))
	other := k.setGrant(map[string]any{"source_worker_id": a2.ID, "source_session": "agent", "target_worker_id": a1.ID, "level": ops.GrantLevelView})

	code, evs := k.grantHistory(g.ID)
	if code != http.StatusOK || len(evs) != 1 || evs[0].Action != AuditActionGrantCreate || evs[0].GrantID != g.ID {
		t.Fatalf("owner history = %d %+v", code, evs)
	}
	k.as(k.u2)
	if code, evs := k.grantHistory(g.ID); code == http.StatusOK {
		t.Fatalf("another user reads the history: %+v", evs)
	}
	k.as(k.admin)
	if code, evs := k.grantHistory(g.ID); code != http.StatusOK || len(evs) != 1 {
		t.Fatalf("administrator history = %d %+v", code, evs)
	}

	// The session is gone: the grant ends, its history stays readable.
	if n, err := k.st.EndGoneSessionGrants(ctx, a1.ID, []string{"agent"}, 1<<60); err != nil || n != 1 {
		t.Fatalf("end = %d, %v", n, err)
	}
	k.as(k.u1)
	code, evs = k.grantHistory(g.ID)
	if code != http.StatusOK || len(evs) != 2 || evs[0].Action != store.AuditActionGrantEnd ||
		evs[0].Actor != store.AuditActorSystem || evs[0].Detail != "cause=session_gone" || evs[1].Action != AuditActionGrantCreate {
		t.Fatalf("history of an ended grant = %d %+v", code, evs)
	}
	k.as(k.u2)
	if code, evs := k.grantHistory(g.ID); code == http.StatusOK {
		t.Fatalf("another user reads the history of an ended grant: %+v", evs)
	}
	k.as(k.admin)
	if code, evs := k.grantHistory(g.ID); code != http.StatusOK || len(evs) != 2 {
		t.Fatalf("administrator, ended grant = %d %+v", code, evs)
	}
	// The other grant's rows are not part of this history.
	if code, evs := k.grantHistory(other.ID); code != http.StatusOK || len(evs) != 1 || evs[0].GrantID != other.ID {
		t.Fatalf("other grant = %d %+v", code, evs)
	}
	// A grant nobody recorded, and an id that is none.
	k.as(k.u1)
	if code, _ := k.grantHistory(ops.NewUUIDv7()); code != http.StatusNotFound {
		t.Fatalf("unknown grant = %d", code)
	}
	if code, _ := k.grantHistory("nope"); code != http.StatusBadRequest {
		t.Fatalf("bad id = %d", code)
	}
	_ = b1
}

// operateOnlyOn is an API key scope with read and operate on one Worker.
type operateOnlyOn string

func (s operateOnlyOn) Actions(res authz.ResolvedResource) []ops.Action {
	if res.WorkerID == string(s) {
		return []ops.Action{ops.ActionRead, ops.ActionOperate}
	}
	return nil
}

// sessionsOf returns the Source and Target of every event of the history.
func sessionsOf(evs []ops.AuditEvent) string {
	var b strings.Builder
	for _, e := range evs {
		b.WriteString(e.Source + " -> " + e.Target + "\n")
	}
	return b.String()
}

// grant_history masks, as grant_list does, the session name on the side of a
// grant whose Worker the caller may not operate: the other owner's Worker,
// and a Worker outside an API key's scope. Also for an ended grant.
func TestGrantHistoryMasksSessionsOutsideTheCallersVisibility(t *testing.T) {
	k := newGrantHistoryStack(t)
	ctx := t.Context()
	a, a2 := k.worker(k.u1, "a", store.WorkerKindPrivate), k.worker(k.u1, "a2", store.WorkerKindPrivate)
	b := k.worker(k.u2, "b", store.WorkerKindPrivate)
	if _, err := k.st.DB().ExecContext(ctx, `UPDATE workers SET approval='approved'`); err != nil {
		t.Fatal(err)
	}
	k.as(k.admin)
	cross := k.setGrant(grantArgs(a, b, ops.GrantLevelView)) // a:agent -> b:build
	k.as(k.u1)
	own := k.setGrant(grantArgs(a, a2, ops.GrantLevelView)) // a:agent -> a2:build

	var maskedName func(target string) string
	check := func(who, id, wantSrc, wantTgt string) {
		t.Helper()
		code, evs := k.grantHistory(id)
		if code != http.StatusOK || len(evs) == 0 {
			t.Fatalf("%s: history = %d %+v", who, code, evs)
		}
		// The whole answer, names given by the Hub's namer included, holds
		// no masked session name.
		_, body := k.call(ops.OpGrantHistory, map[string]any{"id": id})
		for _, w := range []struct{ want, name string }{{wantSrc, "agent"}, {wantTgt, "build"}} {
			if !strings.HasSuffix(w.want, ":"+w.name) && strings.Contains(body, w.name) {
				t.Fatalf("%s: the answer names the masked session %q: %s", who, w.name, body)
			}
		}
		for _, e := range evs {
			if wantName := maskedName(e.Target); e.Target == wantTgt && e.TargetName != wantName {
				t.Fatalf("%s: target_name = %q, want %q", who, e.TargetName, wantName)
			}
		}
		for _, e := range evs {
			if e.Source != wantSrc || e.Target != wantTgt {
				t.Fatalf("%s: events =\n%s want %s -> %s", who, sessionsOf(evs), wantSrc, wantTgt)
			}
		}
	}
	masked := func(w store.WorkerRow) string { return w.ID + ":a session of another user" }
	names := map[string]string{a.ID: "a", a2.ID: "a2", b.ID: "b"}
	maskedName = func(target string) string {
		id, rest, _ := strings.Cut(target, ":")
		return names[id] + ":" + rest
	}
	for round := 0; round < 2; round++ {
		k.as(k.admin)
		check("administrator", cross.ID, a.ID+":agent", b.ID+":build")
		k.as(k.u1) // owns the source
		check("source owner", cross.ID, a.ID+":agent", masked(b))
		k.as(k.u2) // owns the target
		check("target owner", cross.ID, masked(a), b.ID+":build")
		// A key of u1 scoped to Worker a: a2's session is hidden.
		k.authn.mu.Lock()
		k.authn.id = Identity{Principal: authz.NewAPIKeyPrincipal(k.u1, operateOnlyOn(a.ID), ""), Actor: "key:k1", Via: ViaAPIKey, IP: netip.MustParseAddr("192.0.2.7")}
		k.authn.mu.Unlock()
		check("key scoped to a", own.ID, a.ID+":agent", masked(a2))
		check("key scoped to a, cross", cross.ID, a.ID+":agent", masked(b))
		if round == 0 {
			// The source session is gone: both grants end; the history of an
			// ended grant is masked the same way.
			if n, err := k.st.EndGoneSessionGrants(ctx, a.ID, []string{"agent"}, 1<<60); err != nil || n != 2 {
				t.Fatalf("end = %d, %v", n, err)
			}
		}
	}
	k.as(k.u2)
	if code, evs := k.grantHistory(own.ID); code == http.StatusOK {
		t.Fatalf("another user reads the history: %+v", evs)
	}
}
