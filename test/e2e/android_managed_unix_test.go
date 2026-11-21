//go:build e2e && unix

package e2e

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/testutil/tmuxtest"
	"github.com/patrick-1984/ai-minion/internal/worker/agent"
	wstore "github.com/patrick-1984/ai-minion/internal/worker/store"
)

func TestAndroidManagedWorkerCreatesSessionsUnderHubPolicy(t *testing.T) {
	ctx := context.Background()
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	t.Setenv("MINION_HIDDEN_SESSIONS", config.HiddenSessionsUserControlled)
	e := &shareE2E{t: t, ph: startShareHub(t), wid: store.NewID()}
	e.ownerID, e.owner = e.user("android-worker-owner")
	tm := tmuxtest.New(t)
	tm.NewSession("keep-server-ready", "cat")
	binary, err := exec.LookPath("tmux")
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	db, err := wstore.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if err := db.SaveShare(wstore.ShareSelection{Mode: wstore.ShareAll}); err != nil {
		t.Fatal(err)
	}
	url := "https://" + e.ph.addr
	enroll := agent.NewEnrollments(db, dir)
	key, err := enroll.LoadOrCreateKey()
	if err != nil {
		t.Fatal(err)
	}
	if err := enroll.Commit(ctx, agent.Enrollment{
		Pin: identity.Pin{Pub: e.ph.wsv.Identity().Public()}, WorkerID: e.wid, HubURL: url,
	}); err != nil {
		t.Fatal(err)
	}
	if err := e.ph.st.InsertWorker(ctx, nil, store.WorkerRow{
		ID: e.wid, Pub: key.Public(), Name: "android", OwnerID: e.ownerID,
		Kind: store.WorkerKindPrivate, CreatedAt: time.Now().UnixMilli(),
	}); err != nil {
		t.Fatal(err)
	}
	rt := agent.NewRuntime(agent.DefaultRegistry, agent.Options{})
	s := agent.Settings{
		WorkerCore: config.WorkerCore{
			StateDir: dir, Name: "android", TmuxSocket: tm.Socket(),
			Managed: config.ManagedHost{Android: true, TmuxBinary: binary},
		},
		Resolver: &config.Resolver{Role: config.ScopeWorker, File: config.MapSource{config.KeyServer: url}},
	}
	if err := rt.Start(ctx, s); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rt.Stop(ctx) }()
	waitFor(t, "managed Worker online", func() bool { return e.ph.wsv.Registry().IsOnline(e.wid) })

	create := func(name string) (int, string) {
		return e.rest(e.owner, ops.OpSessionCreate, map[string]any{
			"worker_id": e.wid, "request_id": ops.NewUUIDv7(), "name": name, "argv": []string{"cat"},
		})
	}
	if code, body := create("refused"); code != 403 || !strings.Contains(body, "Android app") {
		t.Fatalf("session_create without opting in = %d %s", code, body)
	}
	if err := e.ph.st.SetAllowHiddenSessions(ctx, e.ownerID, true); err != nil {
		t.Fatal(err)
	}
	if code, body := create("android-pty"); code != 200 {
		t.Fatalf("session_create after opting in = %d %s", code, body)
	}
	tm.SendKeys("android-pty", "android-pty-ready")
	tm.WaitFor("android-pty", "android-pty-ready", 5*time.Second)
	t.Setenv("MINION_HIDDEN_SESSIONS", config.HiddenSessionsDisabled)
	if code, body := create("policy-refused"); code != 403 || !strings.Contains(body, "disabled on this Hub") {
		t.Fatalf("session_create on a disabled Hub = %d %s", code, body)
	}
}
