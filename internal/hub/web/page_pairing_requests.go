package web

// Pairing requests (v0.12.0, A41). A Worker that waits with a pairing code is
// also a request an administrator can admit with one click, after comparing
// fingerprint words (NetBird's peer approval, SSH and Signal fingerprint
// comparison). The panel is the partial "pairing_requests" that the Workers
// page and the Add worker page include: it loads its list from
// /workers/pairing-requests and refreshes it while the page is open. Admit and
// Reject are plain posts of the catalog operations pairing_request_admit and
// pairing_request_reject as the signed-in user: the Hub decides (admin only).

import (
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/patrick-1984/ai-minion/internal/ops"
)

// pathPairingRequests serves the list fragment (GET).
const pathPairingRequests = "/workers/pairing-requests"

// PairingRequestAdmitPath and PairingRequestRejectPath are the posts of one
// request, named by its public pairing id.
func PairingRequestAdmitPath(id string) string {
	return pathPairingRequests + "/" + url.PathEscape(id) + "/admit"
}

// PairingRequestRejectPath is the path that rejects a request (POST).
func PairingRequestRejectPath(id string) string {
	return pathPairingRequests + "/" + url.PathEscape(id) + "/reject"
}

// pairingRequestsView is what the fragment "pairing_requests_list" renders.
type pairingRequestsView struct {
	HubWords, HubFull string
	Requests          []pairingRequestView
	MaxName           int
}

type pairingRequestView struct {
	ops.PairingRequestInfo
	Since               string
	AdmitURL, RejectURL string
}

var pairingRequestFragments = fragments("_pairing_requests.html")

// stopPolling is the status that tells htmx to stop polling the panel (a user
// who may not admit, or a Hub without the operations).
const stopPolling = 286

func handlePairingRequests(w http.ResponseWriter, r *http.Request, c OpCaller, u pageUser) {
	if !u.Admin {
		w.WriteHeader(stopPolling)
		return
	}
	var res ops.PairingRequestsListResult
	if err := c.CallOp(r, ops.OpPairingRequestsList, ops.PairingRequestsListArgs{}, &res); err != nil {
		w.WriteHeader(stopPolling)
		return
	}
	v := pairingRequestsView{HubWords: res.HubFingerprintWords, HubFull: res.HubFingerprint, MaxName: ops.MaxWorkerNameLen}
	for _, q := range res.Requests {
		since := q.WaitingSince
		if t, err := time.Parse(time.RFC3339, q.WaitingSince); err == nil {
			since = t.UTC().Format("2006-01-02 15:04:05") + " UTC"
		}
		v.Requests = append(v.Requests, pairingRequestView{
			PairingRequestInfo: q, Since: since,
			AdmitURL: PairingRequestAdmitPath(q.PairingID), RejectURL: PairingRequestRejectPath(q.PairingID),
		})
	}
	writeFragmentTemplate(w, pairingRequestFragments(), "pairing_requests_list", v)
}

func handlePairingRequestAdmit(w http.ResponseWriter, r *http.Request, c OpCaller, _ pageUser) {
	args := ops.PairingRequestAdmitArgs{
		WriteArgs: newWriteArgs(), PairingID: r.PathValue("pairing"),
		WorkerFingerprint: r.PostFormValue("worker_fingerprint"), Name: strings.TrimSpace(r.PostFormValue("name")),
	}
	writeOp(w, r, c, ops.OpPairingRequestAdmit, args,
		"The Worker is admitted and approved. It is in your space and connects within moments.")
}

func handlePairingRequestReject(w http.ResponseWriter, r *http.Request, c OpCaller, _ pageUser) {
	args := ops.PairingRequestRejectArgs{
		WriteArgs: newWriteArgs(), PairingID: r.PathValue("pairing"), WorkerFingerprint: r.PostFormValue("worker_fingerprint"),
	}
	writeOp(w, r, c, ops.OpPairingRequestReject, args,
		"The pairing request is rejected. The Worker shows that and a new code.")
}

func init() {
	RegisterRoute(Route{Pattern: "GET " + pathPairingRequests, Handler: userRoute(handlePairingRequests)})
	RegisterRoute(Route{Pattern: "POST " + pathPairingRequests + "/{pairing}/admit", Handler: userRoute(handlePairingRequestAdmit)})
	RegisterRoute(Route{Pattern: "POST " + pathPairingRequests + "/{pairing}/reject", Handler: userRoute(handlePairingRequestReject)})
}
