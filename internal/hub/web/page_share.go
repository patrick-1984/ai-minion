package web

// The share panel of the Worker detail page (Story 4.15, R75 "configure later from the
// Hub"): the owner of a Worker, or an administrator, picks the sessions the Worker shares.
//
//   - It is two guarded routes, GET and POST /w/<id>/share, on the Worker's canonical id. They
//     use the session routes' guard: the Worker must be in the signed-in user's own list
//     (a foreign or unknown Worker is the same plain 404), the CSRF check of the shell covers
//     the POST, and everything is done through the Hub's REST as that user: share_get and
//     share_set, which the API authorises (operate on that Worker) again. The page hides what
//     the API would refuse; the API decides.
//   - The panel is NOT part of the live fragment of the Worker page (/live/workers/<id>): that
//     fragment is replaced as sessions change, which would take a half-made choice with it.
//     The page loads the panel once (hx-trigger="load") and the form's own answer replaces it.
//   - What it shows is names only: the sessions the Worker shares (sessions_list) and the ones
//     it does not (share_get), with the rule. No screen, task or turn of any session.
//   - A Worker that is offline, that is preview-only (it refuses both operations), or that is
//     started with the local share lock (it refuses share_set) is said so in words; the form
//     is not offered when it cannot work.

import (
	"errors"
	"html/template"
	"net/http"
	"net/url"
	"slices"
	"sync"

	"github.com/patrick-1984/ai-minion/internal/ops"
)

// pathSharePattern is the route of the share panel (GET) and of the change (POST).
const pathSharePattern = "/w/{worker}/share"

// SharePath returns the path of the share panel of a Worker, by canonical id.
func SharePath(worker string) string { return "/w/" + url.PathEscape(worker) + "/share" }

// ShareEntry is one session name of the panel.
type ShareEntry struct {
	Name   string
	Shared bool
}

// ShareView is what the "share_panel" fragment sees.
type ShareView struct {
	WorkerID string
	Worker   string
	URL      string
	// Note is a sentence that replaces the form (offline, refused, not allowed).
	Note string
	// Mode is the rule in force: all, none, select or later.
	Mode string
	// Selected is the mode the form starts with.
	Selected string
	// Locked is true when the Worker's local share lock refuses a change.
	Locked  bool
	Entries []ShareEntry
	Results []ActionResult
}

var shareTemplates = sync.OnceValue(func() *template.Template {
	return template.Must(template.ParseFS(templateFiles, "templates/share.html"))
})

func shareNames(mode string, shared, unshared []string) []ShareEntry {
	out := make([]ShareEntry, 0, len(shared)+len(unshared))
	for _, n := range shared {
		out = append(out, ShareEntry{Name: n, Shared: true})
	}
	for _, n := range unshared {
		out = append(out, ShareEntry{Name: n})
	}
	slices.SortFunc(out, func(a, b ShareEntry) int {
		switch {
		case a.Name < b.Name:
			return -1
		case a.Name > b.Name:
			return 1
		}
		return 0
	})
	return out
}

// shareView reads the rule and the names of the Worker of t.
func shareView(r *http.Request, t target) ShareView {
	v := ShareView{WorkerID: t.worker.ID, Worker: t.worker.Name, URL: SharePath(t.worker.ID)}
	switch {
	case !t.operate:
		v.Note = "Only the owner of this worker, or an administrator, chooses what it shares."
		return v
	case !t.worker.Online:
		v.Note = "This worker is offline. Its sharing can be changed when it reconnects."
		return v
	}
	var rule ops.ShareResult
	if err := t.c.CallOp(r, ops.OpShareGet, ops.ShareGetArgs{WorkerID: t.worker.ID}, &rule); err != nil {
		v.Note = shareFailure(err)
		return v
	}
	var list ops.SessionsListResult
	_ = t.c.CallOp(r, ops.OpSessionsList, struct {
		WorkerID string `json:"worker_id"`
	}{t.worker.ID}, &list)
	shared := make([]string, 0, len(list.Sessions))
	for _, s := range list.Sessions {
		shared = append(shared, s.Name)
	}
	v.Mode, v.Locked = rule.Rule.Mode, rule.Locked
	v.Selected = rule.Rule.Mode
	if v.Selected == ops.ShareModeLater {
		v.Selected = ops.ShareModeNone
	}
	v.Entries = shareNames(rule.Rule.Mode, shared, rule.Unshared)
	if v.Locked {
		v.Note = "The share selection of this worker is locked on the worker (share_lock). Change it there."
	}
	return v
}

// shareFailure is the sentence for a refused or failed share call.
func shareFailure(err error) string {
	var oe *OpError
	if errors.As(err, &oe) {
		switch oe.Code {
		case "worker_offline":
			return "This worker is offline. Its sharing can be changed when it reconnects."
		case "unsupported_op":
			return "This worker is too old to change its sharing from the Hub. Update it, or use `minion worker config share` on the worker."
		case "forbidden":
			if oe.Message != "" {
				return oe.Message
			}
		}
	}
	return failureText(err)
}

func handleShareGet(w http.ResponseWriter, r *http.Request, t target) {
	writeFragmentTemplate(w, shareTemplates(), "share_panel", shareView(r, t))
}

func handleShareSet(w http.ResponseWriter, r *http.Request, t target) {
	args := ops.ShareSetArgs{WorkerID: t.worker.ID, Mode: r.PostForm.Get("mode")}
	if args.Mode == ops.ShareModeSelect {
		args.Names = slices.Compact(slices.Sorted(slices.Values(r.PostForm["name"])))
	}
	var res ops.ShareResult
	ar := ActionResult{Label: "Sharing"}
	if err := t.c.CallOp(r, ops.OpShareSet, args, &res); err != nil {
		ar.Kind, ar.Summary = "danger", shareFailure(err)
		var oe *OpError
		if errors.As(err, &oe) && oe.Field != "" {
			ar.Summary = "The choice cannot be applied: " + oe.Message
		}
	} else {
		ar.Kind, ar.Summary = "info", "Saved. "+describeRule(res.Rule)
	}
	v := shareView(r, t)
	v.Results = []ActionResult{ar}
	writeFragmentTemplate(w, shareTemplates(), "share_panel", v)
}

func describeRule(r ops.ShareRule) string {
	switch r.Mode {
	case ops.ShareModeAll:
		return "The worker shares all its sessions, also the ones created later."
	case ops.ShareModeNone:
		return "The worker shares no session."
	case ops.ShareModeSelect:
		return "The worker shares only the sessions you chose."
	}
	return "Nothing is shared until a choice is made."
}

func init() {
	for _, rt := range []struct {
		pattern string
		fn      func(http.ResponseWriter, *http.Request, target)
	}{
		{"GET " + pathSharePattern, handleShareGet},
		{"POST " + pathSharePattern, handleShareSet},
	} {
		RegisterRoute(Route{Pattern: rt.pattern, Handler: sessionRoute(rt.fn)})
	}
}
