package web

// Central Worker update (v0.9.0, A10): an administrator updates a Worker, or
// every outdated one, to the Hub's own release. The Hub sends worker_update
// with the exact version; the Worker answers at once and updates and restarts
// in the background, verifying the signed release itself. The new version
// shows in the Version column when it reconnects.

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/patrick-1984/ai-minion/internal/hub/auth/session"
	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/version"
)

// WorkerUpdatePath updates one Worker (POST); WorkersUpdateAllPath every outdated one.
func WorkerUpdatePath(id string) string { return "/workers/" + url.PathEscape(id) + "/update" }

const WorkersUpdateAllPath = "/workers/update-all"

// maxUpdatesPerClick bounds the Workers one "Update all outdated" starts.
const maxUpdatesPerClick = 8

// updateTarget is the release Workers are updated to: the Hub's own. A
// development build of the Hub has none.
func updateTarget() (string, bool) {
	v, err := version.ParseVersion(version.Version)
	if err != nil {
		return "", false
	}
	return v.String(), true
}

// workerOutdated reports whether a Worker that reported build b runs an older
// release than target.
func workerOutdated(b, target string) bool {
	bv, err1 := version.ParseVersion(b)
	tv, err2 := version.ParseVersion(target)
	return err1 == nil && err2 == nil && bv.Compare(tv) < 0
}

// updatable reports whether the Hub offers the Update button for w.
func updatable(w ops.WorkerInfo, target string) bool {
	return w.Online && !w.Pending && proto.HasFeature(w.Features, proto.FeatureSelfUpdate) && workerOutdated(w.Build, target)
}

func updateOne(r *http.Request, c OpCaller, id string, force bool) (ops.WorkerUpdateResult, error) {
	target, ok := updateTarget()
	if !ok {
		return ops.WorkerUpdateResult{}, fmt.Errorf("this Hub is a development build: there is no release to update Workers to")
	}
	var res ops.WorkerUpdateResult
	err := c.CallOp(r, ops.OpWorkerUpdate, ops.WorkerUpdateArgs{WorkerID: id, Version: target, Force: force}, &res)
	return res, err
}

func handleWorkerUpdate(w http.ResponseWriter, r *http.Request, c OpCaller, _ pageUser) {
	res, err := updateOne(r, c, r.PathValue("worker"), r.PostFormValue("force") == "1")
	switch {
	case err != nil:
		writeResultError(w, failureText(err))
	case !res.Accepted:
		writeResultError(w, "The Worker did not start the update: "+res.Reason+".")
	default:
		done(w, r, fmt.Sprintf("Updating the Worker from %s to %s. It restarts by itself and shows the new version when it reconnects.", res.From, res.To), ops.WriteResult{})
	}
}

// handleWorkersUpdateAll asks every online, outdated, self-updatable Worker
// the caller may see. Each call returns at once, so the loop is short.
func handleWorkersUpdateAll(w http.ResponseWriter, r *http.Request, c OpCaller, _ pageUser) {
	target, ok := updateTarget()
	if !ok {
		writeResultError(w, "This Hub is a development build: there is no release to update Workers to.")
		return
	}
	var list ops.WorkersListResult
	// The same scope as the page the button was on (Mine or All users).
	all := r.PostFormValue("scope") == scopeAll
	if err := c.CallOp(r, ops.OpWorkersList, ops.WorkersListArgs{All: all}, &list); err != nil {
		writeResultError(w, failureText(err))
		return
	}
	var started, skipped []string
	queued := 0
	for _, wk := range list.Workers {
		if !updatable(wk, target) {
			continue
		}
		// At most maxUpdatesPerClick Workers download and restart at once;
		// the next click updates the next ones.
		if len(started) >= maxUpdatesPerClick {
			queued++
			continue
		}
		res, err := updateOne(r, c, wk.ID, false)
		switch {
		case err != nil:
			skipped = append(skipped, wk.Name+" ("+failureText(err)+")")
		case !res.Accepted:
			skipped = append(skipped, wk.Name+" ("+res.Reason+")")
		default:
			started = append(started, wk.Name)
		}
	}
	if len(started) == 0 && len(skipped) == 0 {
		writeResultError(w, "No Worker needs an update to "+target+".")
		return
	}
	msg := fmt.Sprintf("Updating %d Workers to %s: %s.", len(started), target, strings.Join(started, ", "))
	if len(started) == 0 {
		msg = "No Worker started the update."
	}
	if len(skipped) > 0 {
		msg += " Not updated: " + strings.Join(skipped, "; ") + "."
	}
	if queued > 0 {
		msg += fmt.Sprintf(" %d more wait: click Update all outdated again when these show the new version.", queued)
	}
	done(w, r, msg, ops.WriteResult{})
}

func init() {
	RegisterRoute(Route{Pattern: "POST /workers/{worker}/refresh", Handler: userRoute(handleWorkerRefresh)})
	RegisterRoute(Route{Pattern: "POST /workers/{worker}/update", Handler: userRoute(handleWorkerUpdate)})
	RegisterRoute(Route{Pattern: "POST " + WorkersUpdateAllPath, Handler: userRoute(handleWorkersUpdateAll)})
}

// isAdminRequest reports whether the request comes from an administrator
// acting as themselves.
func isAdminRequest(r *http.Request) bool {
	sess, ok := session.FromContext(r.Context())
	return ok && sess.Role == authz.RoleAdmin && sess.ActAs == nil
}
