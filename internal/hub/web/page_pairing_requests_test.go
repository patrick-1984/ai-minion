package web

// A41: the Pairing requests panel of the Workers page and the Add worker page.

import (
	"io/fs"
	"net/url"
	"strings"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/ops"
)

const (
	prWorkerFP = "to7lcal2ljtsreilmsriq6xqkqhemi67na4jpbthuy5k7slnwe7a"
	prHubFP    = "mzuhvlpymk6xo3epygfy5h4oeaejofefn3rdhm4qfjmr2dk7fesq"
)

func pairingRequestsOps() *fakeOps {
	return newFakeOps().reply(ops.OpPairingRequestsList, ops.PairingRequestsListResult{
		HubFingerprint: prHubFP, HubFingerprintWords: identity.Fingerprint(prHubFP).Words(),
		Requests: []ops.PairingRequestInfo{{
			PairingID: "K7Q4", Name: "pi-<b>", SourceAddr: "192.0.2.9", WaitingSince: "2026-11-18T09:00:00Z",
			WorkerFingerprint: prWorkerFP, WorkerFingerprintWords: identity.Fingerprint(prWorkerFP).Words(),
		}},
	}).reply(ops.OpPairingRequestAdmit, ops.WriteResult{Outcome: ops.OutcomeDelivered}).
		reply(ops.OpPairingRequestReject, ops.WriteResult{Outcome: ops.OutcomeDelivered})
}

func TestBothPagesIncludeThePairingRequestsPanel(t *testing.T) {
	connect(t, pairingRequestsOps())
	rec := onbGet(t, onbAdmin(t), "/workers/add")
	if rec.Code != 200 {
		t.Fatalf("add worker = %d", rec.Code)
	}
	mustHave(t, rec.Body.String(), `hx-get="/workers/pairing-requests"`, "data-pairing-requests")
	for _, f := range []string{"workers.html", "addworker.html"} {
		b, err := fs.ReadFile(EmbeddedTemplates(), f)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(b), `{{template "pairing_requests" .}}`) {
			t.Errorf("%s does not include the pairing requests panel", f)
		}
	}
}

func TestThePairingRequestsListShowsTheWordsAndAsksToCompareThem(t *testing.T) {
	f := pairingRequestsOps()
	connect(t, f)
	rec := onbGet(t, onbAdmin(t), "/workers/pairing-requests")
	if rec.Code != 200 {
		t.Fatalf("list = %d", rec.Code)
	}
	body := rec.Body.String()
	mustHave(t, body, "Pairing requests", "K7Q4", "192.0.2.9", "2026-11-18 09:00:00 UTC", prWorkerFP,
		identity.Fingerprint(prWorkerFP).Words(), identity.Fingerprint(prHubFP).Words(),
		"Admit only if the Worker shows these words", `hx-post="/workers/pairing-requests/K7Q4/admit"`,
		`hx-post="/workers/pairing-requests/K7Q4/reject"`, `name="worker_fingerprint" value="`+prWorkerFP+`"`)
	mustNotHave(t, body, "pi-<b>")
}

func TestAPlainUserGetsNoListAndStopsPolling(t *testing.T) {
	f := pairingRequestsOps()
	connect(t, f)
	rec := onbGet(t, onbUser(t), "/workers/pairing-requests")
	if rec.Code != stopPolling || strings.TrimSpace(rec.Body.String()) != "" {
		t.Fatalf("user list = %d %q", rec.Code, rec.Body.String())
	}
	if len(f.called(ops.OpPairingRequestsList)) != 0 {
		t.Error("the list was called for a plain user (a denied call is audited)")
	}
}

func TestAnEmptyListRendersNothing(t *testing.T) {
	connect(t, newFakeOps().reply(ops.OpPairingRequestsList, ops.PairingRequestsListResult{}))
	rec := onbGet(t, onbAdmin(t), "/workers/pairing-requests")
	if rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != "" {
		t.Fatalf("empty list = %d %q", rec.Code, rec.Body.String())
	}
}

func TestAdmitAndRejectPostTheCatalogOperationsWithTheFingerprint(t *testing.T) {
	f := pairingRequestsOps()
	connect(t, f)
	s := onbAdmin(t)
	if rec := onbPost(t, s, "/workers/pairing-requests/K7Q4/admit", url.Values{"worker_fingerprint": {prWorkerFP}}); rec.Code != 200 {
		t.Fatalf("admit = %d %s", rec.Code, rec.Body.String())
	}
	c := f.called(ops.OpPairingRequestAdmit)
	if len(c) != 1 || c[0].Args["pairing_id"] != "K7Q4" || c[0].Args["worker_fingerprint"] != prWorkerFP {
		t.Fatalf("admit calls = %+v", c)
	}
	if rec := onbPost(t, s, "/workers/pairing-requests/K7Q4/reject", url.Values{"worker_fingerprint": {prWorkerFP}}); rec.Code != 200 {
		t.Fatalf("reject = %d %s", rec.Code, rec.Body.String())
	}
	if c := f.called(ops.OpPairingRequestReject); len(c) != 1 || c[0].Args["worker_fingerprint"] != prWorkerFP {
		t.Fatalf("reject calls = %+v", c)
	}
}
