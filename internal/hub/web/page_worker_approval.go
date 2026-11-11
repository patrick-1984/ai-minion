package web

// Approval of a new Worker (F-02, enrollment is not authorization). A Worker that
// enrolled waits as "pending": the Workers list and the Worker page show it with
// its fingerprint, host name and source address, a banner counts the pending ones,
// and the owner or an admin approves or removes it. Both buttons are plain posts of
// the catalog operations worker_approve and worker_remove as the signed-in user:
// the Hub decides, the page only offers what the API would allow.

import (
	"net/http"
	"net/url"

	"github.com/patrick-1984/ai-minion/internal/ops"
)

// WorkerApprovePath is the path that approves a pending Worker (POST).
func WorkerApprovePath(id string) string { return "/workers/" + url.PathEscape(id) + "/approve" }

// WorkerRemovePath is the path that removes a Worker from its page or row (POST).
func WorkerRemovePath(id string) string { return "/workers/" + url.PathEscape(id) + "/remove" }

func handleWorkerApprove(w http.ResponseWriter, r *http.Request, c OpCaller, _ pageUser) {
	args := ops.WorkerApproveArgs{
		WriteArgs: newWriteArgs(), WorkerID: r.PathValue("worker"), Fingerprint: r.PostFormValue("fingerprint"),
	}
	writeOp(w, r, c, ops.OpWorkerApprove, args,
		"The Worker is approved. It reconnects within moments and then receives operations.")
}

func handleWorkerRemove(w http.ResponseWriter, r *http.Request, c OpCaller, _ pageUser) {
	args := ops.WorkerRemoveArgs{WriteArgs: newWriteArgs(), WorkerID: r.PathValue("worker")}
	writeOp(w, r, c, ops.OpWorkerRemove, args, "The Worker is removed.")
}

func init() {
	RegisterRoute(Route{Pattern: "POST /workers/{worker}/approve", Handler: userRoute(handleWorkerApprove)})
	RegisterRoute(Route{Pattern: "POST /workers/{worker}/remove", Handler: userRoute(handleWorkerRemove)})
}
