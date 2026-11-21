//go:build e2e && unix

package e2e

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/client"
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
	t.Setenv("HOME", t.TempDir())
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
		Pin: identity.Pin{Pub: e.ph.wsv.Identity().Public()}, WorkerID: e.wid, HubURL: url, Owner: "android-worker-owner",
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.PrepareUsageIntent(ctx, url, e.ph.wsv.Identity().Fingerprint().String()); err != nil {
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
		Resolver: &config.Resolver{
			Role: config.ScopeWorker, File: config.MapSource{config.KeyServer: url},
			LookupEnv: func(string) (string, bool) { return "", false },
		},
	}
	if err := rt.Start(ctx, s); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rt.Stop(ctx) }()
	waitFor(t, "managed Worker online after its usage grant", func() bool {
		accepted, _ := db.UsageRegistration(ctx, e.wid)
		return accepted && e.ph.wsv.Registry().IsOnline(e.wid)
	})

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

// Exercise the Android app's two local commands against the real Hub. The
// supervisor starts the Worker from just the configured state. In particular,
// the TV flow must consume its URL-bound acceptance after authenticated code
// pairing without needing an external CLI inside the app's private profile.
func TestAndroidUsageEnrollment(t *testing.T) {
	bin := minionBinary(t)
	tmuxBin, err := exec.LookPath("tmux")
	if err != nil {
		t.Fatal(err)
	}
	for _, flow := range []string{"token", "code"} {
		t.Run(flow, func(t *testing.T) {
			e := &shareE2E{t: t, ph: startShareHub(t)}
			e.ownerID, e.owner = e.user("android-enrollment-owner")
			ctx, cancel := context.WithTimeout(t.Context(), 40*time.Second)
			defer cancel()
			tm := tmuxtest.New(t)
			tm.NewSession("keep-server-ready", "cat")
			dir, err := os.MkdirTemp("", "ad")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(dir) })
			command := func(args ...string) *exec.Cmd {
				cmd := exec.CommandContext(ctx, bin, append([]string{"worker"}, args...)...)
				cmd.Env = []string{
					"PATH=" + os.Getenv("PATH"), "HOME=" + dir, "TMPDIR=" + dir, "XDG_RUNTIME_DIR=" + dir,
					"MINION_STATE_DIR=" + filepath.Join(dir, "state"), "MINION_TMUX_SOCKET=" + tm.Socket(),
					"MINION_MANAGED_HOST=android", "MINION_TMUX_BINARY=" + tmuxBin,
				}
				return cmd
			}
			hub := "https://" + e.ph.addr
			args := []string{"up", "--configure-only", "--non-interactive-share", "all", "--accept-usage"}
			var token string
			if flow == "token" {
				status, body := e.ph.do("POST", "/api/v1/ops/"+ops.OpInvitationCreate,
					ops.InvitationCreateArgs{Mode: ops.InvitationOneTime, TTL: ops.TTL1h}, e.owner)
				var invitation ops.InvitationCreateResult
				if status != http.StatusOK || json.Unmarshal([]byte(body), &invitation) != nil {
					t.Fatalf("invitation creation: HTTP %d", status)
				}
				key, err := identity.ParseInviteKey(invitation.Key)
				if err != nil {
					t.Fatal(err)
				}
				join, err := identity.NewJoinToken(hub, key)
				if err != nil {
					t.Fatal(err)
				}
				token = join.Encode()
				args = append(args, "--join", "-")
			} else {
				args = append(args, "--server", hub)
			}

			up := command(args...)
			if token != "" {
				up.Stdin = strings.NewReader(token + "\n")
			}
			out, err := up.CombinedOutput()
			if err != nil || !strings.Contains(string(out), ops.UsageDisclosure) ||
				(token != "" && strings.Contains(string(out), token)) {
				t.Fatalf("configure-only did not disclose and stage the usage acceptance: %v", err)
			}

			run := command("run")
			if err := run.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() {
				_ = run.Process.Signal(os.Interrupt)
				if err := run.Wait(); err != nil {
					t.Errorf("worker run: %v", err)
				}
			}()

			if flow == "code" {
				local := client.Local(client.WithSocketPath(filepath.Join(dir, "minion", "worker.sock")))
				var pairing agent.LiveStatusReply
				waitFor(t, "the private pairing code", func() bool {
					raw, err := local.Call(ctx, agent.OpLocalStatus, nil)
					return err == nil && json.Unmarshal(raw, &pairing) == nil && pairing.Pairing != nil
				})
				status, _ := e.ph.do("POST", "/api/v1/ops/"+ops.OpPairingClaim, ops.PairingClaimArgs{
					WriteArgs: ops.WriteArgs{RequestID: ops.NewUUIDv7()}, Code: pairing.Pairing.Code,
				}, e.owner)
				if status != http.StatusOK {
					t.Fatalf("pairing claim: HTTP %d", status)
				}
			}

			waitFor(t, "the Android Worker to accept its authenticated enrollment", func() bool {
				out, err := command("config", "usage", "--json").Output()
				var usage struct {
					Enrolled bool   `json:"enrolled"`
					Accepted bool   `json:"accepted"`
					Owner    string `json:"owner"`
				}
				return err == nil && json.Unmarshal(out, &usage) == nil && usage.Enrolled && usage.Accepted && usage.Owner == "android-enrollment-owner"
			})
			rows, err := e.ph.st.ListWorkers(ctx)
			if err != nil || len(rows) != 1 {
				t.Fatalf("expected one Hub registration, got %d (%v)", len(rows), err)
			}
		})
	}
}
