package web

// Administration pages, part 5: the session-control page. A grant lets one
// controller session (a Worker and a session name) reach other Workers and
// sessions. The page lists the grants with a Remove button and offers a form
// that creates or replaces one: the controller, then the targets, All or a
// selection of Workers and sessions. Nothing is selected when the form opens,
// and a form without targets creates nothing (default deny). Every read and
// change goes through session_control_list and session_control_set.

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/ops"
)

const pathAdminSessionControl = "/admin/session-control"

// Values of the targets radio and prefixes of the target checkboxes.
const (
	scModeAll      = "all"
	scModeSelected = "selected"
	scWorkerPrefix = "worker:"
	scSessionPref  = "session:"
)

// SessionControlPage is what sessioncontrol.html sees as .Data.
type SessionControlPage struct {
	Grants  []SessionControlRow
	Workers []SessionControlWorker
	// Note says why the Workers could not be listed, when they could not.
	Note string
}

// SessionControlRow is one grant as the table shows it.
type SessionControlRow struct {
	WorkerID    string
	SessionName string
	Controller  string
	Targets     string
	Created     string
	CreatedAt   string
}

// SessionControlWorker is one Worker of the form, with the sessions it shares
// (only for an online Worker that answered sessions_list).
type SessionControlWorker struct {
	ID       string
	Label    string
	Sessions []string
}

func sessionControlData(r *http.Request, c OpCaller) (any, error) {
	var list ops.SessionControlListResult
	if err := c.CallOp(r, ops.OpSessionControlList, ops.SessionControlListArgs{}, &list); err != nil {
		return nil, err
	}
	page := SessionControlPage{}
	names := map[string]string{}
	var workers ops.WorkersListResult
	if err := c.CallOp(r, ops.OpWorkersList, ops.WorkersListArgs{All: true}, &workers); err != nil {
		page.Note = "The Workers could not be loaded, so no controller or target can be chosen."
	}
	for _, w := range workers.Workers {
		names[w.ID] = w.Name
		sw := SessionControlWorker{ID: w.ID, Label: w.Name + " (" + w.Owner + ")"}
		if w.Online {
			var sl ops.SessionsListResult
			if err := c.CallOp(r, ops.OpSessionsList, struct {
				WorkerID string `json:"worker_id"`
			}{w.ID}, &sl); err == nil {
				for _, s := range sl.Sessions {
					sw.Sessions = append(sw.Sessions, s.Name)
				}
			}
		}
		page.Workers = append(page.Workers, sw)
	}
	for _, g := range list.Grants {
		created := g.CreatedAt
		page.Grants = append(page.Grants, SessionControlRow{
			WorkerID: g.WorkerID, SessionName: g.SessionName,
			Controller: nameOrID(g.WorkerID, names) + ":" + g.SessionName,
			Targets:    sessionControlTargets(g.Targets, names),
			Created:    formatTime(&created, ""), CreatedAt: isoTime(&created),
		})
	}
	return page, nil
}

// sessionControlTargets describes the targets of a grant in one line.
func sessionControlTargets(targets []ops.KeyResource, names map[string]string) string {
	var out []string
	for _, t := range targets {
		switch t.Kind {
		case ops.KeyResourceEverything:
			return "All"
		case ops.KeyResourceWorker:
			out = append(out, "Worker "+nameOrID(t.WorkerID, names))
		case ops.KeyResourceSession:
			out = append(out, nameOrID(t.WorkerID, names)+":"+t.SessionName)
		}
	}
	return strings.Join(out, ", ")
}

// buildSessionControlArgs turns the form into the arguments of
// session_control_set. Form fields: worker, session, targets ("all" or
// "selected") and target (repeated: "worker:<id>" or "session:<id>:<name>").
// A ticked Worker covers its sessions, so their own boxes are dropped. The
// result always has targets: empty targets would remove a grant.
func buildSessionControlArgs(form url.Values) (ops.SessionControlSetArgs, error) {
	args := ops.SessionControlSetArgs{
		WorkerID: strings.TrimSpace(form.Get("worker")), SessionName: strings.TrimSpace(form.Get("session")),
	}
	if args.WorkerID == "" {
		return args, errKeyForm("Choose the Worker of the controller session.")
	}
	if args.SessionName == "" {
		return args, errKeyForm("Enter the name of the controller session.")
	}
	switch form.Get("targets") {
	case scModeAll:
		args.Targets = []ops.KeyResource{{Kind: ops.KeyResourceEverything}}
	case scModeSelected:
		values := dedupe(form["target"])
		whole := map[string]bool{}
		for _, v := range values {
			if id, ok := strings.CutPrefix(v, scWorkerPrefix); ok {
				whole[id] = true
				args.Targets = append(args.Targets, ops.KeyResource{Kind: ops.KeyResourceWorker, WorkerID: id})
			}
		}
		for _, v := range values {
			rest, ok := strings.CutPrefix(v, scSessionPref)
			if !ok {
				continue
			}
			id, name, ok := strings.Cut(rest, ":")
			if !ok || whole[id] {
				continue
			}
			args.Targets = append(args.Targets, ops.KeyResource{Kind: ops.KeyResourceSession, WorkerID: id, SessionName: name})
		}
	}
	if len(args.Targets) == 0 {
		return args, errKeyForm("Choose All, or Selected with at least one Worker or session. Without targets nothing is granted.")
	}
	return args, nil
}

func handleSessionControlSet(w http.ResponseWriter, r *http.Request, c OpCaller) {
	args, err := buildSessionControlArgs(r.PostForm)
	if err != nil {
		writeResultError(w, err.Error())
		return
	}
	args.WriteArgs = newWriteArgs()
	writeOp(w, r, c, ops.OpSessionControlSet, args, "The session-control grant of "+args.SessionName+" is saved.")
}

// handleSessionControlRemove removes a grant: session_control_set with no
// targets.
func handleSessionControlRemove(w http.ResponseWriter, r *http.Request, c OpCaller) {
	args := ops.SessionControlSetArgs{WriteArgs: newWriteArgs(), WorkerID: r.PostFormValue("worker"), SessionName: r.PostFormValue("session")}
	writeOp(w, r, c, ops.OpSessionControlSet, args, "The session-control grant of "+args.SessionName+" is removed.")
}

func init() {
	RegisterPage(Page{Pattern: pathAdminSessionControl, Title: "Session control", Template: "sessioncontrol.html", Data: adminPageData(sessionControlData)})
	RegisterNav(NavItem{Label: "Session control", Path: pathAdminSessionControl, Order: adminNavOrder + 25, Roles: []authz.Role{authz.RoleAdmin}, Group: NavGroupAdmin, Icon: "i-sessions"})
	RegisterRoute(Route{Pattern: "POST " + pathAdminSessionControl, Handler: adminRoute(handleSessionControlSet)})
	RegisterRoute(Route{Pattern: "POST " + pathAdminSessionControl + "/remove", Handler: adminRoute(handleSessionControlRemove)})
}
