package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

type claimRecorder struct {
	got PairingClaim
	n   int
	res PairingClaimed
	err error
}

func (c *claimRecorder) ClaimPairing(_ context.Context, in PairingClaim) (PairingClaimed, error) {
	c.n++
	c.got = in
	return c.res, c.err
}

type retryErr struct {
	err   *proto.Error
	after time.Duration
}

func (e retryErr) Error() string             { return e.err.Error() }
func (e retryErr) Unwrap() error             { return e.err }
func (e retryErr) RetryAfter() time.Duration { return e.after }

func pairingRig(t *testing.T, claimer PairingClaimer) *rig {
	t.Helper()
	op, ok := ops.Lookup(ops.OpPairingClaim)
	if !ok {
		t.Fatal("pairing_claim is not in the catalog")
	}
	cat := ops.NewRegistry()
	cat.Register(op)
	r := &rig{
		authn: &fakeAuthn{id: testIdentity}, decide: &fakeDecider{}, audit: &fakeAudit{},
		reg: NewRegistry(cat), mux: http.NewServeMux(), logs: &strings.Builder{},
	}
	r.d = &Dispatcher{Catalog: cat, Handlers: r.reg, Auth: r.authn, Authz: r.decide, Audit: r.audit, Log: testLogger(r.logs)}
	var err error
	if r.router, err = NewRouter(r.d, r.mux); err != nil {
		t.Fatal(err)
	}
	if err := RegisterPairing(r.reg, claimer); err != nil {
		t.Fatal(err)
	}
	return r
}

func claimBody(code string) string {
	return `{"request_id":"` + ops.NewUUIDv7() + `","code":"` + code + `"}`
}

func TestPairingClaimPassesTheCallerAndTheCodeToTheService(t *testing.T) {
	rec := &claimRecorder{res: PairingClaimed{WorkerID: "w-1", Name: "pi"}}
	r := pairingRig(t, rec)
	body := `{"request_id":"` + ops.NewUUIDv7() + `","code":"MN-K7Q4-9XTB","name":"pi"}`
	w := r.do("POST", "/api/v1/ops/pairing_claim", body)
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if rec.got != (PairingClaim{UserID: "user-1", Code: "MN-K7Q4-9XTB", Name: "pi", IP: netip.MustParseAddr("192.0.2.7"), Actor: "user-1", Principal: testIdentity.Principal}) {
		t.Errorf("service got %+v", rec.got)
	}
	var res ops.WriteResult
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil || res.Outcome != ops.OutcomeDelivered || len(res.Warnings) != 0 {
		t.Errorf("result = %s (%v)", w.Body, err)
	}
	// Decided on the caller's own user resource with operate: a user can only
	// pair into their own space.
	c := r.decide.calls[0]
	if c.res != (authz.UserResource{ID: "user-1"}) || !c.accepted.Permits(ops.ActionOperate) || c.accepted.Permits(ops.ActionManage) {
		t.Errorf("Decide(%+v)", c)
	}
}

func TestPairingClaimPassesTheAuditIdentityUnderActAsAndWithAnAPIKey(t *testing.T) {
	for _, tc := range []struct {
		name     string
		id       Identity
		wantUser string
		actor    string
		actingAs string
	}{
		{"act-as", Identity{Principal: authz.NewActAsPrincipal("admin-1", "user-1", time.Now().Add(time.Hour)), Actor: "admin-1", ActingAs: "user-1", Via: ViaSession, IP: testIdentity.IP}, "user-1", "admin-1", "user-1"},
		{"api key", Identity{Principal: authz.NewAPIKeyPrincipal("user-1", nil, ""), Actor: "key:k1", Via: ViaAPIKey, IP: testIdentity.IP}, "user-1", "key:k1", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := &claimRecorder{res: PairingClaimed{WorkerID: "w-1", Name: "pi"}}
			r := pairingRig(t, rec)
			r.authn.id = tc.id
			if w := r.do("POST", "/api/v1/ops/pairing_claim", claimBody("MN-K7Q4-9XTB")); w.Code != 200 {
				t.Fatalf("%d %s", w.Code, w.Body)
			}
			if rec.got.UserID != tc.wantUser || rec.got.Actor != tc.actor || rec.got.ActingAs != tc.actingAs {
				t.Errorf("service got user=%q actor=%q acting_as=%q", rec.got.UserID, rec.got.Actor, rec.got.ActingAs)
			}
		})
	}
}

func TestPairingClaimAuditsTheCallWithoutTheCode(t *testing.T) {
	r := pairingRig(t, &claimRecorder{res: PairingClaimed{WorkerID: "w-1", Name: "pi"}})
	if w := r.do("POST", "/api/v1/ops/pairing_claim", claimBody("MN-K7Q4-9XTB")); w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	evs := r.audit.all()
	if len(evs) != 1 || evs[0].Action != ops.OpPairingClaim || evs[0].Outcome != AuditOutcomeSuccess || evs[0].Actor != "user-1" {
		t.Fatalf("audit = %+v", evs)
	}
	for _, e := range evs {
		if strings.Contains(e.Target+e.Action+e.Actor+e.Outcome, "9XTB") {
			t.Errorf("the audit event holds the code: %+v", e)
		}
	}
}

func TestPairingClaimWarnsWhenTheNameWasSuffixed(t *testing.T) {
	r := pairingRig(t, &claimRecorder{res: PairingClaimed{WorkerID: "w-1", Name: "pi-2", Renamed: true}})
	w := r.do("POST", "/api/v1/ops/pairing_claim", claimBody("MN-K7Q4-9XTB"))
	var res ops.WriteResult
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil || len(res.Warnings) != 1 ||
		res.Warnings[0].Code != ops.WarnWorkerRenamed || !strings.Contains(res.Warnings[0].Message, "pi-2") {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
}

func TestPairingClaimMapsServiceErrorsToTheEnvelope(t *testing.T) {
	wrong := proto.NewError(proto.CodePairingCodeInvalid, "the pairing code is wrong")
	wrong.Details = json.RawMessage(`{"attempts_left":3}`)
	cases := []struct {
		name string
		err  error
		code proto.Code
	}{
		{"wrong code", wrong, proto.CodePairingCodeInvalid},
		{"attempts exceeded", proto.NewError(proto.CodePairingAttemptsExceeded, "x"), proto.CodePairingAttemptsExceeded},
		{"worker gone", proto.NewError(proto.CodeWorkerOffline, "x"), proto.CodeWorkerOffline},
		{"already paired", proto.NewError(proto.CodeAlreadyPaired, "x"), proto.CodeAlreadyPaired},
		{"unknown error is internal", errBoom, proto.CodeInternal},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := pairingRig(t, &claimRecorder{err: tc.err})
			w := r.do("POST", "/api/v1/ops/pairing_claim", claimBody("MN-K7Q4-9XTB"))
			if got := errCode(t, w); got != tc.code {
				t.Fatalf("code = %s, want %s (%s)", got, tc.code, w.Body)
			}
			if strings.Contains(w.Body.String(), "secret detail") {
				t.Errorf("an internal cause leaked: %s", w.Body)
			}
			if evs := r.audit.all(); len(evs) != 1 || evs[0].Outcome != auditOutcomeError {
				t.Errorf("a failed claim is audited as failed: %+v", evs)
			}
		})
	}
	// The attempts left reach the client.
	r := pairingRig(t, &claimRecorder{err: wrong})
	w := r.do("POST", "/api/v1/ops/pairing_claim", claimBody("MN-K7Q4-9XTB"))
	if !strings.Contains(w.Body.String(), `"attempts_left":3`) {
		t.Errorf("details lost: %s", w.Body)
	}
}

func TestPairingClaimSetsRetryAfterWhenRateLimited(t *testing.T) {
	err := retryErr{err: proto.NewError(proto.CodeRateLimited, "slow down"), after: 7 * time.Second}
	r := pairingRig(t, &claimRecorder{err: err})
	w := r.do("POST", "/api/v1/ops/pairing_claim", claimBody("MN-K7Q4-9XTB"))
	if errCode(t, w) != proto.CodeRateLimited || w.Header().Get("Retry-After") != "7" {
		t.Fatalf("%d retry-after=%q %s", w.Code, w.Header().Get("Retry-After"), w.Body)
	}
}

func TestPairingClaimRefusesBadArgumentsBeforeTheService(t *testing.T) {
	rec := &claimRecorder{}
	r := pairingRig(t, rec)
	for name, body := range map[string]string{
		"no code":           `{"request_id":"` + ops.NewUUIDv7() + `"}`,
		"no request id":     `{"code":"MN-K7Q4-9XTB"}`,
		"name with a space": `{"request_id":"` + ops.NewUUIDv7() + `","code":"MN-K7Q4-9XTB","name":"a b"}`,
	} {
		w := r.do("POST", "/api/v1/ops/pairing_claim", body)
		if errCode(t, w) != proto.CodeInvalidArgument {
			t.Errorf("%s: %d %s", name, w.Code, w.Body)
		}
	}
	if rec.n != 0 {
		t.Errorf("the service was called %d times with invalid arguments", rec.n)
	}
}

func TestPairingClaimIsPostOnlySoTheCodeNeverTravelsInAURL(t *testing.T) {
	rec := &claimRecorder{}
	r := pairingRig(t, rec)
	w := r.do("GET", "/api/v1/ops/pairing_claim?code=MN-K7Q4-9XTB&request_id="+ops.NewUUIDv7(), "")
	if errCode(t, w) != proto.CodeMethodNotAllowed || rec.n != 0 {
		t.Fatalf("%d %s (calls %d)", w.Code, w.Body, rec.n)
	}
}

func TestPairingClaimIsDeniedWhenAuthzDenies(t *testing.T) {
	rec := &claimRecorder{}
	r := pairingRig(t, rec)
	r.decide.fn = func(decideCall) error { return proto.NewError(proto.CodeForbidden, "not allowed") }
	w := r.do("POST", "/api/v1/ops/pairing_claim", claimBody("MN-K7Q4-9XTB"))
	if errCode(t, w) != proto.CodeForbidden || rec.n != 0 {
		t.Fatalf("%d %s (calls %d)", w.Code, w.Body, rec.n)
	}
}

func TestRegisterPairingRejectsANilServiceAndADuplicate(t *testing.T) {
	r := pairingRig(t, &claimRecorder{})
	if err := RegisterPairing(r.reg, &claimRecorder{}); err == nil {
		t.Error("a second handler for pairing_claim was accepted")
	}
	if err := RegisterPairing(NewRegistry(ops.Default), nil); err == nil {
		t.Error("a nil service was accepted")
	}
	f := PairingClaimFunc(func(context.Context, PairingClaim) (PairingClaimed, error) { return PairingClaimed{Name: "x"}, nil })
	if got, err := f.ClaimPairing(context.Background(), PairingClaim{}); err != nil || got.Name != "x" {
		t.Errorf("PairingClaimFunc = %+v, %v", got, err)
	}
}
