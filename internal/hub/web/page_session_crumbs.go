package web

import (
	"net/http"
	"strings"

	"github.com/patrick-1984/ai-minion/internal/hub/auth/session"
)

// The breadcrumb of the full session page (owner request A117): Control
// center / Workers / <Worker> / <session>, every part a link, and a quick
// switcher behind the Worker and the session part that lists the Worker's
// sessions and the person's other Workers. The data is the live snapshot the
// Workers and sessions lists show ([LiveSource]), so the switcher lists exactly
// what the API lets this person see there; nothing is fetched by the browser.

// crumbSwitchID is the id of the switcher popover of the session page.
const crumbSwitchID = "crumb-switch"

// maxSwitchItems bounds each list of the switcher; the full lists are a click away.
const maxSwitchItems = 200

// SessionSwitch is the quick switcher of the session page's breadcrumb.
type SessionSwitch struct {
	ID string
	// Worker is the name of the session's Worker (the heading of Sessions).
	Worker   string
	Sessions []SwitchItem
	Workers  []SwitchItem
}

// SwitchItem is one entry of the switcher: a link to a session view or a Worker page.
type SwitchItem struct {
	Label string
	// Code is the short code of a session ("" when the session has none).
	Code    string
	Meta    string
	Path    string
	Current bool
}

// sessionSwitchOf builds the switcher of the session of t from the live
// snapshot. It is a convenience: without a source, or when the snapshot fails,
// it returns nil and the page shows the plain breadcrumb. code is the current
// session's code, "" when it has none.
func sessionSwitchOf(r *http.Request, t target) (sw *SessionSwitch, code string) {
	src := currentLiveSource()
	if src == nil {
		return nil, ""
	}
	uid, admin, ok := requestUser(r)
	if !ok {
		return nil, ""
	}
	// The own scope lists the person's Workers and sessions. Only an
	// administrator on another user's Worker needs every user's snapshot, to
	// find that Worker's sessions.
	all := admin && t.worker.OwnerID != uid
	d, err := src.Snapshot(r, all)
	if err != nil {
		return nil, ""
	}
	me := ""
	if sess, found := session.FromContext(r.Context()); found {
		me = sess.Username
	}
	sw = &SessionSwitch{ID: crumbSwitchID, Worker: t.worker.Name}
	seen := false
	for _, s := range d.Sessions {
		if s.WorkerID != t.worker.ID {
			continue
		}
		cur := s.Name == t.session
		if cur {
			seen, code = true, s.Code
		}
		if len(sw.Sessions) < maxSwitchItems || cur {
			meta := stateLabel(s.State)
			if !s.Online {
				meta = "offline"
			}
			sw.Sessions = append(sw.Sessions, SwitchItem{Label: s.Name, Code: s.Code, Meta: meta, Path: SessionPath(t.ref, s.Name), Current: cur})
		}
	}
	if !seen {
		sw.Sessions = append([]SwitchItem{{Label: t.session, Path: SessionPath(t.ref, t.session), Current: true}}, sw.Sessions...)
	}
	for _, w := range d.Workers {
		cur := w.ID == t.worker.ID
		// Every user's snapshot: the switcher offers the own Workers (and the
		// one of this page), not the whole Hub.
		if all && !cur && (me == "" || w.Owner != me) {
			continue
		}
		if w.Pending || len(sw.Workers) >= maxSwitchItems && !cur {
			continue
		}
		meta := ""
		if !w.Online {
			meta = "offline"
		}
		sw.Workers = append(sw.Workers, SwitchItem{Label: w.Name, Meta: meta, Path: WorkerPath(w.ID, all && cur), Current: cur})
	}
	return sw, code
}

// crumbsOfSession is the breadcrumb of the session page p.
func crumbsOfSession(p SessionPage) []Crumb {
	sw := ""
	if p.Switch != nil {
		sw = p.Switch.ID
	}
	workers := p.WorkersURL
	if workers == "" {
		workers = pathWorkers
	}
	out := []Crumb{{Label: "Control center", Path: "/"}, {Label: "Workers", Path: workers}}
	if p.Worker != "" {
		out = append(out, Crumb{Label: p.Worker, Path: p.WorkerURL, Switch: sw})
	}
	return append(out, Crumb{Label: p.Session, Path: p.SelfURL, Code: strings.TrimSpace(p.Code), Current: true, Switch: sw})
}
