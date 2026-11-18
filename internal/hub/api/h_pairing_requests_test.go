package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/hub/workers"
	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

type requestsRecorder struct {
	reqs   []workers.PairingRequest
	admit  workers.AdmitRequest
	reject workers.RejectRequest
	nAdmit int
	err    error
}

func (r *requestsRecorder) Requests() []workers.PairingRequest { return r.reqs }
func (r *requestsRecorder) Admit(_ context.Context, a workers.AdmitRequest) (workers.ClaimResult, error) {
	r.nAdmit++
	r.admit = a
	return workers.ClaimResult{WorkerID: "w-1", Name: "pi"}, r.err
}
func (r *requestsRecorder) Reject(_ context.Context, a workers.RejectRequest) error {
	r.reject = a
	return r.err
}
func (r *requestsRecorder) HubFingerprint() identity.Fingerprint {
	return identity.Fingerprint("mzuhvlpymk6xo3epygfy5h4oeaejofefn3rdhm4qfjmr2dk7fesq")
}

func requestsRig(t *testing.T, svc PairingRequestsService) *rig {
	t.Helper()
	cat := ops.NewRegistry()
	for _, n := range []string{ops.OpPairingRequestsList, ops.OpPairingRequestAdmit, ops.OpPairingRequestReject} {
		op, ok := ops.Lookup(n)
		if !ok {
			t.Fatalf("%s is not in the catalog", n)
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
	if err := RegisterPairingRequests(r.reg, svc); err != nil {
		t.Fatal(err)
	}
	return r
}

const testWorkerFP = "to7lcal2ljtsreilmsriq6xqkqhemi67na4jpbthuy5k7slnwe7a"

func TestPairingRequestsListShowsTheFactsAndTheHubWords(t *testing.T) {
	since := time.Date(2026, 11, 18, 9, 0, 0, 0, time.UTC)
	rec := &requestsRecorder{reqs: []workers.PairingRequest{{
		PairingID: "K7Q4", Name: "pi", Addr: "192.0.2.9", Since: since, Expires: since.Add(10 * time.Minute),
		WorkerFingerprint: identity.Fingerprint(testWorkerFP),
	}}}
	r := requestsRig(t, rec)
	w := r.do("GET", "/api/v1/ops/pairing_requests_list", "")
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	var res ops.PairingRequestsListResult
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	q := res.Requests[0]
	if len(res.Requests) != 1 || q.PairingID != "K7Q4" || q.SourceAddr != "192.0.2.9" || q.WaitingSince != "2026-11-18T09:00:00Z" ||
		q.WorkerFingerprint != testWorkerFP || q.WorkerFingerprintWords != identity.Fingerprint(testWorkerFP).Words() ||
		res.HubFingerprintWords == "" {
		t.Errorf("result = %+v", res)
	}
	// Admin only: decided with manage.
	if c := r.decide.calls[0]; !c.accepted.Permits(ops.ActionManage) || c.accepted.Permits(ops.ActionOperate) {
		t.Errorf("Decide(%+v)", c)
	}
}

func TestAdmitPassesTheCallerTheKeyAndApproves(t *testing.T) {
	rec := &requestsRecorder{}
	r := requestsRig(t, rec)
	body := `{"request_id":"` + ops.NewUUIDv7() + `","pairing_id":"K7Q4","worker_fingerprint":"` + testWorkerFP + `","name":"pi"}`
	if w := r.do("POST", "/api/v1/ops/pairing_request_admit", body); w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if rec.admit != (workers.AdmitRequest{UserID: "user-1", PairingID: "K7Q4", Fingerprint: testWorkerFP, Name: "pi", Approve: true, Actor: "user-1"}) {
		t.Errorf("service got %+v", rec.admit)
	}
	evs := r.audit.all()
	if len(evs) != 1 || evs[0].Action != ops.OpPairingRequestAdmit || evs[0].Outcome != AuditOutcomeSuccess {
		t.Errorf("audit = %+v", evs)
	}
}

func TestAWorkerBoundKeyCannotAdmitOrReject(t *testing.T) {
	rec := &requestsRecorder{}
	r := requestsRig(t, rec)
	r.authn.id = Identity{Principal: authz.NewAPIKeyPrincipal("user-1", nil, "w-9"), Actor: "key:k1", Via: ViaAPIKey, IP: testIdentity.IP}
	body := `{"request_id":"` + ops.NewUUIDv7() + `","pairing_id":"K7Q4","worker_fingerprint":"` + testWorkerFP + `"}`
	for _, op := range []string{"pairing_request_admit", "pairing_request_reject"} {
		w := r.do("POST", "/api/v1/ops/"+op, body)
		if w.Code != http.StatusForbidden {
			t.Errorf("%s: %d %s", op, w.Code, w.Body)
		}
	}
	if rec.nAdmit != 0 {
		t.Error("the service was called")
	}
}

func TestRejectPassesTheRequestAndAnErrorKeepsItsCode(t *testing.T) {
	rec := &requestsRecorder{err: proto.NewError(proto.CodeConflict, "another key")}
	r := requestsRig(t, rec)
	body := `{"request_id":"` + ops.NewUUIDv7() + `","pairing_id":"K7Q4","worker_fingerprint":"` + testWorkerFP + `"}`
	w := r.do("POST", "/api/v1/ops/pairing_request_reject", body)
	if w.Code != http.StatusConflict {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if rec.reject != (workers.RejectRequest{PairingID: "K7Q4", Fingerprint: testWorkerFP, Actor: "user-1"}) {
		t.Errorf("service got %+v", rec.reject)
	}
}
