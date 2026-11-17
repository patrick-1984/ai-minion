package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/hub/audit"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/transport"
)

// On the real Hub: a Worker that reports hidden sessions (ConPTY) refuses
// session_create for a user who did not allow them, with the fix, audited;
// the user allows them with account_prefs_set and the call is routed; the
// Hub setting hidden_sessions overrides the user's choice, live.
func TestHiddenSessionsOnTheRealHub(t *testing.T) {
	ah, svc := startRemoveHubWithService(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	alice := ah.user(t, "alice")
	wk, err := identity.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	w := store.WorkerRow{ID: store.NewID(), Pub: wk.Public(), Name: "patryk-asus", OwnerID: alice,
		Kind: store.WorkerKindPrivate, CreatedAt: time.Now().UnixMilli()}
	if err := ah.st.InsertWorker(ctx, nil, w); err != nil {
		t.Fatal(err)
	}
	conn, err := transport.Dial(ctx, "https://"+ah.h.srv.Addr())
	if err != nil {
		t.Fatal(err)
	}
	// The Worker does not announce session_create: a call that passes the
	// hidden-sessions check ends as unsupported_op, before anything is sent.
	res, err := transport.WorkerHandshake(conn, transport.WorkerConfig{
		Key: wk, Name: w.Name, Ops: []string{"sessions_list"}, Mode: transport.ModeConnect,
		Pin: identity.Pin{Pub: svc.Identity().Public()}, Timeout: 10 * time.Second,
		Facts: transport.WorkerFacts{Build: "v0.10.0", OS: "windows", Arch: "amd64", SessionHost: "conpty", SessionsHidden: true,
			SessionHostReason: "ConPTY inside the Worker: psmux is not installed (winget install psmux)"},
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

	tok, csrf := ah.login(t, "alice")
	op := func(name string, args map[string]any) (apiResp, proto.Code, string) {
		args["request_id"] = ops.NewUUIDv7()
		r := ah.call(t, "POST", "/api/v1/ops/"+name, args, asSession(tok, csrf))
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
		_, c, m := op(ops.OpSessionCreate, map[string]any{"worker_id": w.ID})
		return c, m
	}

	// The Worker list carries the host and the refusal for this caller.
	wait := time.Now().Add(10 * time.Second)
	var info ops.WorkerInfo
	for time.Now().Before(wait) {
		r := ah.call(t, "GET", "/api/v1/ops/workers_list", nil, asSession(tok, csrf))
		var list ops.WorkersListResult
		_ = json.Unmarshal([]byte(r.body), &list)
		if len(list.Workers) == 1 && list.Workers[0].Online {
			info = list.Workers[0]
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if info.SessionHost != "conpty" || !info.SessionsHidden || !strings.Contains(info.NewSessionRefused, "would be hidden") {
		t.Fatalf("workers_list entry = %+v", info)
	}

	// Default: user_controlled, the user never allowed hidden sessions.
	c, msg := create()
	if c != proto.CodeForbidden || !strings.Contains(msg, "New sessions on patryk-asus would be hidden (ConPTY inside the Worker: psmux is not installed") ||
		!strings.Contains(msg, "Account > Sessions") {
		t.Fatalf("session_create = %s %q, want the hidden-session refusal", c, msg)
	}
	page, err := ah.audit.List(ctx, audit.Filter{Action: ops.OpSessionCreate, Limit: 10})
	if err != nil || len(page.Events) != 1 || page.Events[0].Outcome != "failed" || page.Events[0].Actor != alice {
		t.Fatalf("audit of the refusal = %+v, %v", page, err)
	}

	// The user's own preference.
	r := ah.call(t, "GET", "/api/v1/ops/"+ops.OpAccountPrefs, nil, asSession(tok, csrf))
	var prefs ops.AccountPrefsResult
	if err := json.Unmarshal([]byte(r.body), &prefs); err != nil || prefs.AllowHiddenSessions || prefs.HiddenSessionsAllowed || prefs.HiddenSessions != config.HiddenSessionsUserControlled {
		t.Fatalf("account_prefs = %d %s", r.code, r.body)
	}
	if r, c, _ := op(ops.OpAccountPrefsSet, map[string]any{"allow_hidden_sessions": true}); c != "" {
		t.Fatalf("account_prefs_set = %d %s", r.code, r.body)
	}
	if p, _ := ah.st.UserPrefs(ctx, alice); !p.AllowHiddenSessions {
		t.Fatal("the preference was not stored")
	}
	if c, msg := create(); c != proto.CodeUnsupportedOp {
		t.Fatalf("session_create after allowing = %s %q, want it routed (unsupported_op of this Worker)", c, msg)
	}

	// The Hub setting overrides the user's choice, without a restart.
	t.Setenv("MINION_HIDDEN_SESSIONS", config.HiddenSessionsDisabled)
	if c, msg := create(); c != proto.CodeForbidden || !strings.Contains(msg, "disabled on this Hub") {
		t.Fatalf("session_create with hidden_sessions=disabled = %s %q", c, msg)
	}
	if r, c, _ := op(ops.OpAccountPrefsSet, map[string]any{"allow_hidden_sessions": true}); c != "" || !strings.Contains(r.body, "hub_policy") {
		t.Fatalf("account_prefs_set under a disabled Hub = %d %s, want a hub_policy warning", r.code, r.body)
	}
	t.Setenv("MINION_HIDDEN_SESSIONS", config.HiddenSessionsAllowed)
	if _, err := ah.st.DB().Exec(`DELETE FROM user_prefs`); err != nil {
		t.Fatal(err)
	}
	if c, msg := create(); c != proto.CodeUnsupportedOp {
		t.Fatalf("session_create with hidden_sessions=allowed = %s %q", c, msg)
	}
	// A bad request is refused by the op's own validation.
	if r, c, _ := op(ops.OpAccountPrefsSet, map[string]any{}); c != proto.CodeInvalidArgument {
		t.Fatalf("account_prefs_set without a value = %d %s", r.code, r.body)
	}
}
