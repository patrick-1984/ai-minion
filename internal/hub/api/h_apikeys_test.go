package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// fakeKeyManager records the calls of the three handlers.
type fakeKeyManager struct {
	created []ops.APIKeyCreateArgs
	listed  []ops.APIKeyListArgs
	revoked []string
	last    authz.Principal
	err     error
}

func (f *fakeKeyManager) Create(_ context.Context, p authz.Principal, a ops.APIKeyCreateArgs) (ops.APIKeyCreateResult, error) {
	f.last = p
	f.created = append(f.created, a)
	if f.err != nil {
		return ops.APIKeyCreateResult{}, f.err
	}
	return ops.APIKeyCreateResult{Key: "mn_testkey", Info: ops.APIKeyInfo{ID: "id-1", Name: a.Name, Hint: "testkey0"}}, nil
}

func (f *fakeKeyManager) List(_ context.Context, p authz.Principal, a ops.APIKeyListArgs) (ops.APIKeyListResult, error) {
	f.last = p
	f.listed = append(f.listed, a)
	return ops.APIKeyListResult{}, f.err
}

func (f *fakeKeyManager) Revoke(_ context.Context, p authz.Principal, id string) error {
	f.last = p
	f.revoked = append(f.revoked, id)
	return f.err
}

type keysRig struct {
	*rig
	km *fakeKeyManager
}

func newKeysRig(t *testing.T) *keysRig {
	t.Helper()
	cat := ops.NewRegistry()
	for _, n := range []string{ops.OpAPIKeyCreate, ops.OpAPIKeyList, ops.OpAPIKeyRevoke} {
		op, ok := ops.Lookup(n)
		if !ok {
			t.Fatalf("ops.Default lacks %s", n)
		}
		cat.Register(op)
	}
	r := &rig{
		authn: &fakeAuthn{id: testIdentity}, decide: &fakeDecider{}, audit: &fakeAudit{},
		reg: NewRegistry(cat), mux: http.NewServeMux(), logs: &strings.Builder{},
	}
	r.d = &Dispatcher{Catalog: cat, Handlers: r.reg, Auth: r.authn, Authz: r.decide, Audit: r.audit, Log: testLogger(r.logs)}
	var err error
	if r.router, err = NewRouter(r.d, r.mux); err != nil {
		t.Fatal(err)
	}
	km := &fakeKeyManager{}
	if err := RegisterAPIKeyHandlers(r.reg, km); err != nil {
		t.Fatal(err)
	}
	return &keysRig{rig: r, km: km}
}

func (r *keysRig) post(path, body string) *httptest.ResponseRecorder {
	return r.do("POST", "/api/v1/ops/"+path, body)
}

func TestRegisterAPIKeyHandlersFailsClosed(t *testing.T) {
	if err := RegisterAPIKeyHandlers(NewRegistry(ops.NewRegistry()), &fakeKeyManager{}); err == nil {
		t.Error("registering on a catalog without the key ops succeeded")
	}
	if err := RegisterAPIKeyHandlers(NewRegistry(ops.Default), nil); err == nil {
		t.Error("registering a nil manager succeeded")
	}
	r := NewRegistry(ops.Default)
	if err := RegisterAPIKeyHandlers(r, &fakeKeyManager{}); err != nil {
		t.Fatal(err)
	}
	if err := RegisterAPIKeyHandlers(r, &fakeKeyManager{}); err == nil {
		t.Error("registering twice succeeded")
	}
}

func TestCreateHandlerPassesTheCallerAndReturnsTheKey(t *testing.T) {
	r := newKeysRig(t)
	wid := ops.NewUUIDv7()
	w := r.post("apikey_create", `{"name":"bot","resources":[{"kind":"worker","worker_id":"`+wid+`"}],"actions":["read"]}`)
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	var res ops.APIKeyCreateResult
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil || res.Key != "mn_testkey" || res.Info.Hint != "testkey0" {
		t.Fatalf("result = %s (%v)", w.Body, err)
	}
	if len(r.km.created) != 1 || r.km.created[0].Resources[0].WorkerID != wid || r.km.last.UserID != "user-1" {
		t.Errorf("manager saw %+v for %+v", r.km.created, r.km.last)
	}
	// Decided on the caller's own user resource with operate.
	c := r.decide.calls[0]
	if c.res != (authz.UserResource{ID: "user-1"}) || !c.accepted.Permits(ops.ActionOperate) || c.accepted.Permits(ops.ActionManage) {
		t.Errorf("Decide(%+v)", c)
	}
	// The response is not cached or sniffed, and no audit row is written by
	// the dispatcher for a non-write op (the manager audits create).
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("Cache-Control = %q", w.Header().Get("Cache-Control"))
	}
	if len(r.audit.all()) != 0 {
		t.Errorf("dispatcher audit rows = %+v", r.audit.all())
	}
}

func TestCreateHandlerValidatesBeforeTheManager(t *testing.T) {
	r := newKeysRig(t)
	for name, body := range map[string]string{
		"no name":      `{"resources":[{"kind":"everything"}],"actions":["read"]}`,
		"no resources": `{"name":"x","actions":["read"]}`,
		"bad action":   `{"name":"x","resources":[{"kind":"everything"}],"actions":["root"]}`,
		"bad expiry":   `{"name":"x","resources":[{"kind":"everything"}],"actions":["read"],"expires_at":"tomorrow"}`,
	} {
		if w := r.post("apikey_create", body); errCode(t, w) != proto.CodeInvalidArgument {
			t.Errorf("%s: %d %s", name, w.Code, w.Body)
		}
	}
	if len(r.km.created) != 0 {
		t.Error("the manager ran for invalid arguments")
	}
}

func TestCreateCannotBeCalledWithGET(t *testing.T) {
	// A link must not mint a key: the resources cannot be given in a query
	// string, so a GET never reaches the manager.
	r := newKeysRig(t)
	w := r.do("GET", "/api/v1/ops/apikey_create?name=x&actions=read&resources=everything", "")
	if w.Code == 200 {
		t.Fatalf("GET apikey_create succeeded: %s", w.Body)
	}
	if len(r.km.created) != 0 {
		t.Error("the manager ran for a GET")
	}
}

func TestListHandlerDecidesOnTheUserWhoseKeysAreAsked(t *testing.T) {
	r := newKeysRig(t)
	if w := r.do("GET", "/api/v1/ops/apikey_list", ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"keys":[]`) {
		t.Fatalf("own list: %d %s", w.Code, w.Body)
	}
	if c := r.decide.calls[0]; c.res != (authz.UserResource{ID: "user-1"}) || !c.accepted.Permits(ops.ActionRead) {
		t.Errorf("own list decided on %+v", c)
	}
	other := ops.NewUUIDv7()
	if w := r.do("GET", "/api/v1/ops/apikey_list?owner_id="+other, ""); w.Code != 200 {
		t.Fatalf("owner list: %d %s", w.Code, w.Body)
	}
	if c := r.decide.calls[1]; c.res != (authz.UserResource{ID: other}) {
		t.Errorf("owner list decided on %+v, want the asked user", c)
	}
	if r.km.listed[1].OwnerID != other {
		t.Errorf("manager saw %+v", r.km.listed[1])
	}
	// A denial by Decide stops the handler (a user asking for another user).
	r.decide.fn = func(c decideCall) error {
		if u, ok := c.res.(authz.UserResource); ok && u.ID == other {
			return proto.NewError(proto.CodeNotFound, "not found")
		}
		return nil
	}
	before := len(r.km.listed)
	if w := r.do("GET", "/api/v1/ops/apikey_list?owner_id="+other, ""); errCode(t, w) != proto.CodeNotFound {
		t.Errorf("denied list: %d %s", w.Code, w.Body)
	}
	if len(r.km.listed) != before {
		t.Error("the manager ran after Decide denied")
	}
	if w := r.do("GET", "/api/v1/ops/apikey_list?owner_id=bob", ""); errCode(t, w) != proto.CodeInvalidArgument {
		t.Errorf("bad owner id: %d %s", w.Code, w.Body)
	}
}

func TestRevokeHandlerIsAWriteWithAnAuditRow(t *testing.T) {
	r := newKeysRig(t)
	kid := ops.NewUUIDv7()
	if w := r.do("GET", "/api/v1/ops/apikey_revoke?id="+kid, ""); w.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET revoke = %d", w.Code)
	}
	body := `{"request_id":"` + ops.NewUUIDv7() + `","id":"` + kid + `"}`
	w := r.post("apikey_revoke", body)
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	var res ops.WriteResult
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil || res.Outcome != ops.OutcomeDelivered {
		t.Fatalf("result = %s (%v)", w.Body, err)
	}
	if len(r.km.revoked) != 1 || r.km.revoked[0] != kid {
		t.Errorf("revoked = %v", r.km.revoked)
	}
	if ev := r.audit.all(); len(ev) != 1 || ev[0].Action != ops.OpAPIKeyRevoke || ev[0].Outcome != AuditOutcomeSuccess {
		t.Errorf("audit = %+v", ev)
	}
	// Without a request id or with a bad key id nothing runs.
	for name, b := range map[string]string{
		"no request id": `{"id":"` + kid + `"}`,
		"bad id":        `{"request_id":"` + ops.NewUUIDv7() + `","id":"key"}`,
	} {
		if w := r.post("apikey_revoke", b); errCode(t, w) != proto.CodeInvalidArgument {
			t.Errorf("%s: %d %s", name, w.Code, w.Body)
		}
	}
	if len(r.km.revoked) != 1 {
		t.Error("the manager ran for invalid arguments")
	}
}

func TestManagerErrorsMapToCodes(t *testing.T) {
	r := newKeysRig(t)
	kid := ops.NewUUIDv7()
	body := `{"request_id":"` + ops.NewUUIDv7() + `","id":"` + kid + `"}`
	for name, tc := range map[string]struct {
		err  error
		want proto.Code
	}{
		"forbidden": {proto.NewError(proto.CodeForbidden, "no"), proto.CodeForbidden},
		"not found": {proto.NewError(proto.CodeNotFound, "no"), proto.CodeNotFound},
		"internal":  {errBoom, proto.CodeInternal},
	} {
		r.km.err = tc.err
		w := r.post("apikey_revoke", body)
		if errCode(t, w) != tc.want {
			t.Errorf("%s: %d %s", name, w.Code, w.Body)
		}
		if strings.Contains(w.Body.String(), "minion.db") {
			t.Errorf("%s: an internal detail leaked: %s", name, w.Body)
		}
	}
	// A failed revoke is audited as failed.
	last := r.audit.all()
	if len(last) == 0 || last[len(last)-1].Outcome != "failed" {
		t.Errorf("audit = %+v", last)
	}
}
