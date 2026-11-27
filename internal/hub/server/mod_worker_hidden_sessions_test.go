package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/audit"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/transport"
)

// On the real Hub (v0.23.0): each Worker carries its own hidden-sessions
// choice. An administrator sets it on another user's private Worker, the
// owner on their own; anybody else is refused; every change is audited; the
// choice wins over the caller's account-wide preference, and "default"
// returns to it.
func TestWorkerHiddenSessionsOnTheRealHub(t *testing.T) {
	ah, svc := startRemoveHubWithService(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	alice := ah.user(t, "alice")
	ah.user(t, "bob")
	root := ah.user(t, "boss")
	if _, err := ah.st.DB().Exec(`INSERT INTO user_roles (user_id, role, source) VALUES (?, 'admin', 'local')`, root); err != nil {
		t.Fatal(err)
	}
	wk, err := identity.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	w := store.WorkerRow{ID: store.NewID(), Pub: wk.Public(), Name: "dana-asus", OwnerID: alice,
		Kind: store.WorkerKindPrivate, CreatedAt: time.Now().UnixMilli()}
	if err := ah.st.InsertWorker(ctx, nil, w); err != nil {
		t.Fatal(err)
	}
	conn, err := transport.Dial(ctx, "https://"+ah.h.srv.Addr())
	if err != nil {
		t.Fatal(err)
	}
	res, err := transport.WorkerHandshake(conn, transport.WorkerConfig{
		Key: wk, Name: w.Name, Ops: []string{"sessions_list"}, Mode: transport.ModeConnect,
		Pin: identity.Pin{Pub: svc.Identity().Public()}, Timeout: 10 * time.Second,
		Facts: transport.WorkerFacts{Build: "v0.22.0", OS: "windows", Arch: "amd64", SessionHost: "conpty", SessionsHidden: true,
			SessionHostReasonCode: transport.HostReasonConPTYNoPsmux, SessionHostReason: "ConPTY inside the Worker: psmux is not installed (winget install psmux)"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := proto.ReadFrame(res.Channel, proto.StreamFrameLimit); err != nil { // welcome
		t.Fatal(err)
	}
	sess, err := transport.NewSession(res.Channel, transport.RoleWorker, transport.SessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sess.Close() }()

	type who struct{ tok, csrf string }
	login := func(name string) who { tok, csrf := ah.login(t, name); return who{tok, csrf} }
	asAlice, asBob, asRoot := login("alice"), login("bob"), login("boss")
	op := func(as who, name string, args map[string]any) (apiResp, proto.Code, string) {
		args["request_id"] = ops.NewUUIDv7()
		r := ah.call(t, "POST", "/api/v1/ops/"+name, args, asSession(as.tok, as.csrf))
		var env struct {
			Error struct {
				Code    proto.Code `json:"code"`
				Message string     `json:"message"`
			} `json:"error"`
		}
		_ = json.Unmarshal([]byte(r.body), &env)
		return r, env.Error.Code, env.Error.Message
	}
	create := func() (proto.Code, string) {
		_, c, m := op(asAlice, ops.OpSessionCreate, map[string]any{"worker_id": w.ID})
		return c, m
	}
	setHidden := func(as who, v string) (apiResp, proto.Code) {
		r, c, _ := op(as, ops.OpWorkerHiddenSessionsSet, map[string]any{"worker_id": w.ID, "hidden_sessions": v})
		return r, c
	}
	listed := func(as who, all bool) ops.WorkerInfo {
		path := "/api/v1/ops/workers_list"
		if all {
			path += "?all=true"
		}
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			r := ah.call(t, "GET", path, nil, asSession(as.tok, as.csrf))
			var list ops.WorkersListResult
			_ = json.Unmarshal([]byte(r.body), &list)
			for _, x := range list.Workers {
				if x.ID == w.ID && x.Online {
					return x
				}
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatal("the Worker is not listed online")
		return ops.WorkerInfo{}
	}
	if info := listed(asAlice, false); info.AllowHiddenSessions != nil || info.NewSessionRefused == "" {
		t.Fatalf("before any choice: %+v", info)
	}
	if c, _ := create(); c != proto.CodeForbidden {
		t.Fatalf("session_create with no choice anywhere = %s, want forbidden", c)
	}

	// Another user may not change it.
	if r, c := setHidden(asBob, ops.WorkerHiddenAllow); c != proto.CodeForbidden && c != proto.CodeNotFound {
		t.Fatalf("bob's worker_hidden_sessions_set = %d %s", r.code, r.body)
	}
	// An administrator may, on another user's private Worker.
	if r, c := setHidden(asRoot, ops.WorkerHiddenAllow); c != "" {
		t.Fatalf("admin's worker_hidden_sessions_set = %d %s", r.code, r.body)
	}
	if info := listed(asRoot, true); info.AllowHiddenSessions == nil || !*info.AllowHiddenSessions {
		t.Fatalf("after the admin allowed: %+v", info)
	}
	if info := listed(asAlice, false); info.NewSessionRefused != "" {
		t.Fatalf("the owner still sees a refusal: %+v", info)
	}
	if c, msg := create(); c != proto.CodeUnsupportedOp {
		t.Fatalf("session_create on the allowing Worker = %s %q, want it routed", c, msg)
	}

	// The owner refuses on the Worker although she allows account-wide.
	if r, c, _ := op(asAlice, ops.OpAccountPrefsSet, map[string]any{"allow_hidden_sessions": true}); c != "" {
		t.Fatalf("account_prefs_set = %d %s", r.code, r.body)
	}
	if r, c := setHidden(asAlice, ops.WorkerHiddenRefuse); c != "" {
		t.Fatalf("owner's worker_hidden_sessions_set = %d %s", r.code, r.body)
	}
	if c, msg := create(); c != proto.CodeForbidden || !strings.Contains(msg, "refused on this Worker") {
		t.Fatalf("session_create on the refusing Worker = %s %q", c, msg)
	}
	// Default: the account-wide preference decides again.
	if r, c := setHidden(asAlice, ops.WorkerHiddenDefault); c != "" {
		t.Fatalf("worker_hidden_sessions_set default = %d %s", r.code, r.body)
	}
	if v, err := ah.st.WorkerHiddenSessions(ctx, w.ID); err != nil || v != nil {
		t.Fatalf("stored choice after default = %v, %v", v, err)
	}
	if c, msg := create(); c != proto.CodeUnsupportedOp {
		t.Fatalf("session_create with the account default = %s %q", c, msg)
	}
	// A bad value is refused by the op's validation.
	if r, c := setHidden(asAlice, "maybe"); c != proto.CodeInvalidArgument {
		t.Fatalf("worker_hidden_sessions_set maybe = %d %s", r.code, r.body)
	}

	// Every change is audited, with the choice and its actor.
	page, err := ah.audit.List(ctx, audit.Filter{Action: ops.OpWorkerHiddenSessionsSet, Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, e := range page.Events {
		if e.Outcome == "delivered" || e.Outcome == "ok" || e.Outcome == "success" {
			got[e.Detail] = e.Actor
		}
	}
	if got["hidden_sessions=allow"] != root || got["hidden_sessions=refuse"] != alice || got["hidden_sessions=default"] != alice {
		t.Fatalf("audit = %+v", page.Events)
	}
}
