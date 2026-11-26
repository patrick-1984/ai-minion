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
	"strconv"
	"strings"

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

// WorkerRepairPairingPath is the path that re-issues the pairing of a Worker
// that never connected (POST, design 12.3).
func WorkerRepairPairingPath(id string) string {
	return "/workers/" + url.PathEscape(id) + "/repair-pairing"
}

func handleWorkerRepairPairing(w http.ResponseWriter, r *http.Request, c OpCaller, _ pageUser) {
	args := ops.WorkerPairingReissueArgs{WriteArgs: newWriteArgs(), WorkerID: r.PathValue("worker")}
	writeOp(w, r, c, ops.OpWorkerPairingReissue, args,
		"Pairing re-issued for 10 minutes. Run minion worker pair on the Worker (or wait for its new code) and enter the new code under Add worker.")
}

// WorkerRenamePath is the path that renames a Worker from its page (POST).
func WorkerRenamePath(id string) string { return "/workers/" + url.PathEscape(id) + "/rename" }

// handleWorkerRename posts worker_rename as the signed-in user: the Hub
// decides whether this person may rename the Worker (its owner or an admin),
// keeps the owner prefix, refuses a name the owner already uses and audits
// the change. The Worker reconnects and learns the new name from the Hub.
func handleWorkerRename(w http.ResponseWriter, r *http.Request, c OpCaller, _ pageUser) {
	name := strings.TrimSpace(r.PostFormValue("name"))
	if !ops.ValidWorkerName(name) {
		writeResultError(w, "Enter a name of at most "+strconv.Itoa(ops.MaxWorkerNameLen)+" characters, without spaces or colons.")
		return
	}
	args := ops.WorkerRenameArgs{WriteArgs: newWriteArgs(), WorkerID: r.PathValue("worker"), Name: name}
	writeOp(w, r, c, ops.OpWorkerRename, args,
		"The Worker is renamed. It reconnects within moments and then shows the new name. Session ids, grants and keys are kept: they name the Worker by its id.")
}

func handleWorkerRemove(w http.ResponseWriter, r *http.Request, c OpCaller, _ pageUser) {
	args := ops.WorkerRemoveArgs{WriteArgs: newWriteArgs(), WorkerID: r.PathValue("worker")}
	writeOp(w, r, c, ops.OpWorkerRemove, args, "The Worker is removed.")
}

func init() {
	RegisterRoute(Route{Pattern: "POST /workers/{worker}/approve", Handler: userRoute(handleWorkerApprove)})
	RegisterRoute(Route{Pattern: "POST /workers/{worker}/remove", Handler: userRoute(handleWorkerRemove)})
	RegisterRoute(Route{Pattern: "POST /workers/{worker}/repair-pairing", Handler: userRoute(handleWorkerRepairPairing)})
	RegisterRoute(Route{Pattern: "POST /workers/{worker}/rename", Handler: userRoute(handleWorkerRename)})
}
