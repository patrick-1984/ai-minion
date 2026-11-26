package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/hub/audit"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// newStrictStack is newGrantStack with the request de-duplication wired to
// the real store, as the Hub wires it.
func newStrictStack(t *testing.T) *kindStack { return newStrictStackWith(t, nil) }

// newStrictStackWith is newStrictStack; requests, when set, wraps the store as
// the dispatcher's request store.
func newStrictStackWith(t *testing.T, requests func(*store.Store) RequestStore) *kindStack {
	t.Helper()
	k := newKindStack(t)
	cat := ops.NewRegistry()
	for _, n := range []string{ops.OpGrantList, ops.OpGrantSet, ops.OpGrantRevoke, ops.OpGrantApprove} {
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
	// The real audit log over the store, as on the Hub: the audit row of a
	// grant change commits in the store with the change, a refusal is
	// recorded through the log, and both are read from audit_events.
	lg := audit.New(k.st)
	if err := RegisterGrants(handlers, k.st, list, k.az, lg); err != nil {
		t.Fatal(err)
	}
	k.mux = http.NewServeMux()
	var rs RequestStore = k.st
	if requests != nil {
		rs = requests(k.st)
	}
	d := &Dispatcher{Catalog: cat, Handlers: handlers, Auth: k.authn, Authz: k.az, Audit: lg, Requests: rs,
		Log: testLogger(&strings.Builder{})}
	if _, err := NewRouter(d, k.mux); err != nil {
		t.Fatal(err)
	}
	return k
}

type errorEnvelope struct {
	Error struct {
		Code    proto.Code     `json:"code"`
		Message string         `json:"message"`
		Details map[string]any `json:"details"`
	} `json:"error"`
}

func strictErr(t *testing.T, body string) errorEnvelope {
	t.Helper()
	var e errorEnvelope
	if err := json.Unmarshal([]byte(body), &e); err != nil || e.Error.Code == "" {
		t.Fatalf("not an error body: %s", body)
	}
	return e
}

func (k *kindStack) get(op, query string) (int, string) {
	k.t.Helper()
	rec := newRecorder()
	k.mux.ServeHTTP(rec, httptest.NewRequest("GET", "/api/v1/ops/"+op+"?"+query, nil))
	return rec.Code, rec.Body.String()
}

// auditCount is the number of stored audit rows of one action.
func (k *kindStack) auditCount(action string) int {
	k.t.Helper()
	return len(k.grantEvents(action))
}

// auditTotal is the number of stored audit rows.
func (k *kindStack) auditTotal() int {
	k.t.Helper()
	var n int
	if err := k.st.DB().QueryRowContext(context.Background(), `SELECT COUNT(*) FROM audit_events`).Scan(&n); err != nil {
		k.t.Fatal(err)
	}
	return n
}

func TestRepeatedGrantSetRunsOnceAndAnswersTheSameBody(t *testing.T) {
	k := newStrictStack(t)
	a1, a2 := k.worker(k.u1, "a1", store.WorkerKindPrivate), k.worker(k.u1, "a2", store.WorkerKindPrivate)
	k.as(k.u1)
	args := grantArgs(a1, a2, ops.GrantLevelView)
	args["request_id"] = ops.NewUUIDv7()

	code1, body1 := k.call(ops.OpGrantSet, args)
	if code1 != http.StatusOK {
		t.Fatalf("grant_set = %d %s", code1, body1)
	}
	// The repeat is the same request, whatever the order of its members.
	code2, body2 := k.call(ops.OpGrantSet, args)
	if code2 != code1 || body2 != body1 {
		t.Fatalf("the repeat answers %d %s, want %d %s", code2, body2, code1, body1)
	}
	if l := k.listGrants(); len(l) != 1 {
		t.Fatalf("grants after the repeat = %+v, want one", l)
	}
	if n := k.auditCount(AuditActionGrantCreate); n != 1 {
		t.Fatalf("grant create audit rows = %d, want 1: %+v", n, k.grantEvents(AuditActionGrantCreate))
	}

	// A new request_id is a new request: the pair exists, so it conflicts,
	// and that refusal is the answer of its id too.
	args["request_id"] = ops.NewUUIDv7()
	code3, body3 := k.call(ops.OpGrantSet, args)
	if code3 != http.StatusConflict || strictErr(t, body3).Error.Details["reason"] == "request_id_reused" {
		t.Fatalf("second grant of the pair = %d %s", code3, body3)
	}
	rows := k.auditCount(AuditActionGrantCreate)
	if code4, body4 := k.call(ops.OpGrantSet, args); code4 != code3 || body4 != body3 {
		t.Fatalf("the repeated refusal answers %d %s, want %d %s", code4, body4, code3, body3)
	}
	if n := k.auditCount(AuditActionGrantCreate); n != rows {
		t.Fatalf("the repeated refusal added audit rows: %d, want %d", n, rows)
	}
}

func TestRequestIDOfAnotherRequestConflicts(t *testing.T) {
	k := newStrictStack(t)
	a1, a2 := k.worker(k.u1, "a1", store.WorkerKindPrivate), k.worker(k.u1, "a2", store.WorkerKindPrivate)
	k.as(k.u1)
	id := ops.NewUUIDv7()
	args := grantArgs(a1, a2, ops.GrantLevelView)
	args["request_id"] = id
	code, body := k.call(ops.OpGrantSet, args)
	if code != http.StatusOK {
		t.Fatalf("grant_set = %d %s", code, body)
	}
	var first ops.GrantSetResult
	if err := json.Unmarshal([]byte(body), &first); err != nil {
		t.Fatal(err)
	}
	reused := func(what string, code int, body string) {
		t.Helper()
		e := strictErr(t, body)
		if code != http.StatusConflict || e.Error.Code != proto.CodeConflict ||
			e.Error.Message != "this request_id was already used for a different request" || e.Error.Details["field"] != "request_id" {
			t.Fatalf("%s = %d %s", what, code, body)
		}
	}

	// Other arguments with the same id.
	other := grantArgs(a1, a2, ops.GrantLevelControl)
	other["request_id"] = id
	code, body = k.call(ops.OpGrantSet, other)
	reused("other arguments", code, body)
	// Another operation with the same id.
	code, body = k.call(ops.OpGrantRevoke, map[string]any{"request_id": id, "id": first.Grant.ID, "if_revision": first.Grant.Revision})
	reused("another operation", code, body)
	// Another caller with the same id and the same arguments.
	k.as(k.admin)
	code, body = k.call(ops.OpGrantSet, args)
	reused("another caller", code, body)

	// Nothing of it ran: the grant is as the first request made it.
	if l := k.listGrants(); len(l) != 1 || l[0].Level != ops.GrantLevelView || l[0].Revision != first.Grant.Revision {
		t.Fatalf("grants = %+v", l)
	}
}

func TestRepeatedGrantRevokeIsAuditedOnce(t *testing.T) {
	k := newStrictStack(t)
	a1, a2 := k.worker(k.u1, "a1", store.WorkerKindPrivate), k.worker(k.u1, "a2", store.WorkerKindPrivate)
	k.as(k.u1)
	g := k.setGrant(grantArgs(a1, a2, ops.GrantLevelView))
	before := k.auditTotal()
	args := map[string]any{"request_id": ops.NewUUIDv7(), "id": g.ID, "if_revision": g.Revision}
	code1, body1 := k.call(ops.OpGrantRevoke, args)
	if code1 != http.StatusOK {
		t.Fatalf("grant_revoke = %d %s", code1, body1)
	}
	rows := k.auditTotal()
	if rows == before {
		t.Fatal("grant_revoke wrote no audit row")
	}
	if code2, body2 := k.call(ops.OpGrantRevoke, args); code2 != code1 || body2 != body1 {
		t.Fatalf("the repeat answers %d %s, want %d %s", code2, body2, code1, body1)
	}
	if n := k.auditTotal(); n != rows {
		t.Fatalf("the repeat added audit rows: %d, want %d", n, rows)
	}
}

// Requests that arrive together with one id run one after the other: one
// runs, the others are answered from its result.
func TestConcurrentRepeatsOfOneRequestRunOnce(t *testing.T) {
	k := newStrictStack(t)
	a1, a2 := k.worker(k.u1, "a1", store.WorkerKindPrivate), k.worker(k.u1, "a2", store.WorkerKindPrivate)
	k.as(k.u1)
	args := grantArgs(a1, a2, ops.GrantLevelView)
	args["request_id"] = ops.NewUUIDv7()
	b, _ := json.Marshal(args)

	const n = 8
	bodies := make([]string, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rec := newRecorder()
			k.mux.ServeHTTP(rec, newJSONRequest(ops.OpGrantSet, string(b)))
			bodies[i] = rec.Body.String()
		}(i)
	}
	wg.Wait()
	for i := 1; i < n; i++ {
		if bodies[i] != bodies[0] {
			t.Fatalf("answer %d = %s, want %s", i, bodies[i], bodies[0])
		}
	}
	if l := k.listGrants(); len(l) != 1 {
		t.Fatalf("grants = %+v, want one", l)
	}
	if c := k.auditCount(AuditActionGrantCreate); c != 1 {
		t.Fatalf("grant create audit rows = %d, want 1", c)
	}
}

func TestStrictOperationRefusesAnUnknownMember(t *testing.T) {
	k := newStrictStack(t)
	a1, a2 := k.worker(k.u1, "a1", store.WorkerKindPrivate), k.worker(k.u1, "a2", store.WorkerKindPrivate)
	k.as(k.u1)

	unknown := func(what string, code int, body, field string) {
		t.Helper()
		e := strictErr(t, body)
		if code != http.StatusBadRequest || e.Error.Code != proto.CodeInvalidArgument || e.Error.Details["reason"] != "unknown_member" {
			t.Fatalf("%s = %d %s", what, code, body)
		}
		if got, _ := e.Error.Details["field"].(string); got != field {
			t.Fatalf("%s names %q, want %q: %s", what, got, field, body)
		}
	}

	// POST body: a misspelt member, and a member that differs only in case.
	args := grantArgs(a1, a2, ops.GrantLevelView)
	args["target_sesion"] = "build"
	code, body := k.call(ops.OpGrantSet, args)
	unknown("misspelt member", code, body, "target_sesion")
	args = grantArgs(a1, a2, ops.GrantLevelView)
	args["Level"] = ops.GrantLevelControl
	code, body = k.call(ops.OpGrantSet, args)
	unknown("member in another case", code, body, "Level")
	// A name that is no plain member name is refused without being echoed.
	args = grantArgs(a1, a2, ops.GrantLevelView)
	args["<b>x</b>"] = 1
	code, body = k.call(ops.OpGrantSet, args)
	unknown("odd member name", code, body, "")
	if strings.Contains(body, "<b>") || strings.Contains(body, `<b`) {
		t.Fatalf("the odd name is echoed: %s", body)
	}
	// Nothing was created and nothing was audited as a grant change.
	if l := k.listGrants(); len(l) != 0 {
		t.Fatalf("grants after the refusals = %+v", l)
	}
	if n := k.auditCount(AuditActionGrantCreate); n != 0 {
		t.Fatalf("grant create audit rows = %d, want 0", n)
	}

	// GET query: an unknown name is refused, a declared one is accepted.
	code, body = k.get(ops.OpGrantList, "worker="+a1.ID)
	unknown("unknown query member", code, body, "worker")
	if code, body := k.get(ops.OpGrantList, "worker_id="+a1.ID); code != http.StatusOK {
		t.Fatalf("grant_list with its own member = %d %s", code, body)
	}
	// A POST read is strict too.
	code, body = k.call(ops.OpGrantList, map[string]any{"owner": k.u1})
	unknown("unknown member of a read", code, body, "owner")

	// The request still works without the unknown member.
	if g := k.setGrant(grantArgs(a1, a2, ops.GrantLevelView)); g.ID == "" {
		t.Fatal("no grant")
	}
}

// The operations that existed before the strict set keep the additive rule:
// an unknown member is ignored, in the body and in the query.
func TestOldOperationStillIgnoresAnUnknownMember(t *testing.T) {
	k := newKindStack(t)
	w := k.worker(k.u1, "w", store.WorkerKindPrivate)
	k.as(k.u1)
	if ops.StrictArgs(ops.OpTrustSet) || ops.StrictArgs(ops.OpTrustList) {
		t.Fatal("the trust operations are not strict")
	}
	code, body := k.call(ops.OpTrustSet, map[string]any{"worker_id": w.ID, "sesion_name": "x", "Worker_ID": "y"})
	if code != http.StatusOK {
		t.Fatalf("trust_set with an unknown member = %d %s", code, body)
	}
	if code, body := k.get(ops.OpTrustList, "bogus=1&nested.member=2"); code != http.StatusOK {
		t.Fatalf("trust_list with an unknown query member = %d %s", code, body)
	}
	if code, body := k.call(ops.OpTrustList, map[string]any{"bogus": true}); code != http.StatusOK {
		t.Fatalf("trust_list with an unknown member = %d %s", code, body)
	}
}

func TestUnknownMemberWalksNestedArguments(t *testing.T) {
	type inner struct {
		Name string `json:"name"`
	}
	type args struct {
		ops.WriteArgs
		One  inner   `json:"one"`
		Many []inner `json:"many"`
		Ptr  *inner  `json:"ptr,omitempty"`
		Free map[string]any
	}
	for body, want := range map[string]string{
		`{"request_id":"r","force":true,"one":{"name":"a"},"many":[{"name":"b"}],"ptr":{"name":"c"},"Free":{"x":1}}`: "",
		`{"zzz":1,"aaa":2}`:                "aaa",
		`{"one":{"name":"a","nam":"b"}}`:   "one.nam",
		`{"many":[{"name":"a"},{"x":1}]}`:  "many.x",
		`{"ptr":{"Name":"a"}}`:             "ptr.Name",
		`{"one":"not an object","many":7}`: "",
		`{"Request_ID":"r"}`:               "Request_ID",
	} {
		var o rawObject
		if err := json.Unmarshal([]byte(body), &o); err != nil {
			t.Fatal(err)
		}
		got, found := unknownMember(reflect.TypeFor[args](), o)
		if got != want || found != (want != "") {
			t.Errorf("unknownMember(%s) = %q %v, want %q", body, got, found, want)
		}
	}
}

// stoppedRequests is the request store of a Hub that stops right after a
// grant change commits: the dispatcher's own write of the answer never
// happens.
type stoppedRequests struct{ *store.Store }

func (stoppedRequests) PutOpRequest(context.Context, store.OpRequestRow) (bool, error) {
	return false, nil
}

// The answer of a grant write commits with the change: a repeat of the
// request after a stop at the commit gets the same answer, not a conflict or
// not_found, and nothing runs again.
func TestGrantWriteAnswerCommitsWithTheChange(t *testing.T) {
	k := newStrictStackWith(t, func(st *store.Store) RequestStore { return stoppedRequests{st} })
	a1, a2 := k.worker(k.u1, "a1", store.WorkerKindPrivate), k.worker(k.u1, "a2", store.WorkerKindPrivate)
	b := k.worker(k.u2, "b", store.WorkerKindPrivate)
	twice := func(op string, args map[string]any) string {
		t.Helper()
		args["request_id"] = ops.NewUUIDv7()
		code1, body1 := k.call(op, args)
		if code1 != http.StatusOK {
			t.Fatalf("%s = %d %s", op, code1, body1)
		}
		before := k.auditTotal()
		if code2, body2 := k.call(op, args); code2 != code1 || body2 != body1 {
			t.Fatalf("%s repeated = %d %s, want %d %s", op, code2, body2, code1, body1)
		}
		if n := k.auditTotal(); n != before {
			t.Fatalf("%s repeated added audit rows: %d, want %d", op, n, before)
		}
		return body1
	}
	k.as(k.u1)
	var set ops.GrantSetResult
	if err := json.Unmarshal([]byte(twice(ops.OpGrantSet, grantArgs(a1, a2, ops.GrantLevelView))), &set); err != nil || set.Grant.ID == "" {
		t.Fatalf("grant_set = %+v (%v)", set, err)
	}
	twice(ops.OpGrantRevoke, map[string]any{"id": set.Grant.ID, "if_revision": set.Grant.Revision})
	if l := k.listGrants(); len(l) != 0 {
		t.Fatalf("grants = %+v", l)
	}
	k.as(k.admin)
	g := k.setGrant(grantArgs(a1, b, ops.GrantLevelView))
	if _, err := k.st.DB().ExecContext(t.Context(), `UPDATE session_grant SET suspended_at = 1, suspend_cause = 'test' WHERE id = ?`, g.ID); err != nil {
		t.Fatal(err)
	}
	var appr ops.GrantSetResult
	if err := json.Unmarshal([]byte(twice(ops.OpGrantApprove, map[string]any{"id": g.ID, "if_revision": g.Revision})), &appr); err != nil ||
		appr.Grant.Suspended || appr.Grant.Revision != g.Revision+1 {
		t.Fatalf("grant_approve = %+v (%v)", appr, err)
	}
}
