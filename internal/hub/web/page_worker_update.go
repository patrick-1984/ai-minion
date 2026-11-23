package web

// Central Worker update (v0.9.0, A10): an administrator updates a Worker, or
// every outdated one, to the Hub's own release. The Hub sends worker_update
// with the exact version; the Worker answers at once and updates and restarts
// in the background, verifying the signed release itself. The new version
// shows in the Version column when it reconnects.

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

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

// PublishedSource says whether the release tag is published on GitHub as far
// as the Hub's release check knows: known is false when it has no recent list
// (the check is off or has not answered), and the Update button then stays on.
type PublishedSource func(tag string) (published, known bool)

var publishedSource atomic.Pointer[PublishedSource]

// SetPublishedSource installs the release list of the Hub's update check (nil
// removes it).
func SetPublishedSource(f PublishedSource) {
	if f == nil {
		publishedSource.Store(nil)
		return
	}
	publishedSource.Store(&f)
}

// notPublishedText is the reason shown instead of an Update button for a
// release the Hub cannot see publicly; empty when it is published or unknown.
func notPublishedText(target string) string {
	if p := publishedSource.Load(); p != nil {
		if published, known := (*p)(target); known && !published {
			return target + " is not published yet"
		}
	}
	return ""
}

// updateStatusTTL is how long a Worker's update status is reused, so that a
// live page does not ask every outdated Worker on each refresh.
const updateStatusTTL = 10 * time.Second

// updateStatusTimeout bounds the one question to a Worker about its update.
const updateStatusTimeout = 3 * time.Second

type cachedUpdateFailure struct {
	at       time.Time
	text     string
	inflight bool
}

var (
	updateFailMu    sync.Mutex
	updateFailCache = map[string]cachedUpdateFailure{}
)

// forgetUpdateFailure drops the cached status of a Worker (after a new attempt).
func forgetUpdateFailure(id string) {
	updateFailMu.Lock()
	delete(updateFailCache, id)
	updateFailMu.Unlock()
}

// updateFailureText is "Update to vX failed: <reason> (<time>)" (or "was
// rolled back") when the Worker's last update attempt to target did not
// succeed, else "". It never waits for a Worker: it answers from the cache and,
// when the entry is missing or older than updateStatusTTL, starts one bounded
// worker_update_status question per Worker in the background. A page that
// shows the status therefore sees it from its next refresh on, and a Worker
// that cannot answer shows nothing and delays no page.
func updateFailureText(r *http.Request, c OpCaller, id, target string) string {
	updateFailMu.Lock()
	e := updateFailCache[id]
	ask := !e.inflight && time.Since(e.at) >= updateStatusTTL
	if ask {
		e.inflight = true
		updateFailCache[id] = e
	}
	updateFailMu.Unlock()
	if ask {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), updateStatusTimeout)
		rr := r.WithContext(ctx)
		go func() {
			defer cancel()
			text := ""
			var st ops.WorkerUpdateStatus
			if err := c.CallOp(rr, ops.OpWorkerUpdateStatus, ops.WorkerUpdateStatusArgs{WorkerID: id}, &st); err == nil {
				text = updateStatusText(st, target)
			}
			updateFailMu.Lock()
			updateFailCache[id] = cachedUpdateFailure{at: time.Now(), text: text}
			updateFailMu.Unlock()
		}()
	}
	return e.text
}

// updateStatusText words a failed update to target, or one that the Worker
// rolled back after the new binary did not start (its From is then the target).
func updateStatusText(st ops.WorkerUpdateStatus, target string) string {
	var text string
	switch {
	case st.State == ops.UpdateStateFailed && st.To == target:
		text = "Update to " + target + " failed: " + friendlyUpdateError(st.Error, target)
	case st.State == ops.UpdateStateRolledBack && st.From == target:
		reason := st.Error
		if reason == "" {
			reason = target + " did not start on this Worker"
		}
		text = "Update to " + target + " was rolled back to the previous version: " + friendlyUpdateError(reason, target)
	default:
		return ""
	}
	if st.At != nil {
		text += " (" + time.Time(*st.At).UTC().Format("2006-01-02 15:04 UTC") + ")"
	}
	return text
}

// friendlyUpdateError words the error of a Worker that still says "no
// matching release" (before v0.20.1) as what it means.
func friendlyUpdateError(msg, target string) string {
	if strings.HasPrefix(msg, "no matching release") {
		return target + " is not published on GitHub yet (the release is still a draft or missing); try again after it is published"
	}
	return msg
}

func updateOne(r *http.Request, c OpCaller, id string, force bool) (ops.WorkerUpdateResult, error) {
	target, ok := updateTarget()
	if !ok {
		return ops.WorkerUpdateResult{}, fmt.Errorf("this Hub is a development build: there is no release to update Workers to")
	}
	if reason := notPublishedText(target); reason != "" {
		// A release the Hub cannot see publicly cannot be downloaded: do not send it.
		return ops.WorkerUpdateResult{Reason: reason}, nil
	}
	var res ops.WorkerUpdateResult
	forgetUpdateFailure(id)
	err := c.CallOp(r, ops.OpWorkerUpdate, ops.WorkerUpdateArgs{WorkerID: id, Version: target, Force: force}, &res)
	if !res.Accepted {
		res.Reason = friendlyUpdateError(res.Reason, target)
	}
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
	if reason := notPublishedText(target); reason != "" {
		writeResultError(w, "No Worker was sent the update: "+reason+". Try again after it is published.")
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
