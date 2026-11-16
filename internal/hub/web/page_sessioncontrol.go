package web

// Administration pages, part 5: the session-control page. A grant lets one
// controller session (a Worker and a session name) reach other Workers and
// sessions. The page lists the grants with a Remove button and offers a form
// that creates or replaces one: the controller, then the targets, All or a
// selection of Workers and sessions. Nothing is selected when the form opens,
// and a form without targets creates nothing (default deny). Every read and
// change goes through session_control_list and session_control_set.

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/patrick-1984/ai-minion/internal/ops"
)

const pathAdminSessionControl = "/admin/session-control"

// sessionDiscoveryTimeout bounds the sessions_list calls of the page in
// total; a test shortens it.
var sessionDiscoveryTimeout = 2 * time.Second

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
	// Worker names are unique per owner only, so every label names the owner.
	names := map[string]string{}
	var workers ops.WorkersListResult
	if err := c.CallOp(r, ops.OpWorkersList, ops.WorkersListArgs{All: true}, &workers); err != nil {
		page.Note = "The Workers could not be loaded, so no controller or target can be chosen."
	}
	for _, w := range workers.Workers {
		sw := SessionControlWorker{ID: w.ID, Label: w.Name + " (" + w.Owner + ")"}
		names[w.ID] = sw.Label
		page.Workers = append(page.Workers, sw)
	}
	discoverSessions(r, c, workers.Workers, page.Workers)
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

// sessionDiscoveryParallel is how many sessions_list calls of the page run at
// once.
const sessionDiscoveryParallel = 8

// discoverSessions fills in the sessions of the online Workers (out[i] belongs
// to workers[i]). The sessions are only a convenience of the form: an
// unresponsive Worker must not hold the page or hide the other Workers, so the
// Workers are asked concurrently under one short deadline, and the page stops
// waiting at the deadline even when a call does not return (a blocked
// transport write ignores the context). Late answers are dropped; the result
// channel is buffered, so a late call still finishes.
func discoverSessions(r *http.Request, c OpCaller, workers []ops.WorkerInfo, out []SessionControlWorker) {
	ctx, cancel := context.WithTimeout(r.Context(), sessionDiscoveryTimeout)
	defer cancel()
	rs := r.WithContext(ctx)
	type found struct {
		i        int
		sessions []string
	}
	results := make(chan found, len(workers))
	slots := make(chan struct{}, sessionDiscoveryParallel)
	pending := 0
	for i, w := range workers {
		if !w.Online {
			continue
		}
		pending++
		go func() {
			select {
			case slots <- struct{}{}:
				defer func() { <-slots }()
			case <-ctx.Done():
				results <- found{i: i}
				return
			}
			var sl ops.SessionsListResult
			var names []string
			if err := c.CallOp(rs, ops.OpSessionsList, struct {
				WorkerID string `json:"worker_id"`
			}{w.ID}, &sl); err == nil {
				for _, s := range sl.Sessions {
					names = append(names, s.Name)
				}
			}
			results <- found{i: i, sessions: names}
		}()
	}
	for ; pending > 0; pending-- {
		select {
		case f := <-results:
			out[f.i].Sessions = f.sessions
		case <-ctx.Done():
			return
		}
	}
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
	// v0.9.0: Access > Minion control replaces this page in the navigation;
	// the page stays reachable for the grants it shows.
	RegisterRoute(Route{Pattern: "POST " + pathAdminSessionControl, Handler: adminRoute(handleSessionControlSet)})
	RegisterRoute(Route{Pattern: "POST " + pathAdminSessionControl + "/remove", Handler: adminRoute(handleSessionControlRemove)})
}
