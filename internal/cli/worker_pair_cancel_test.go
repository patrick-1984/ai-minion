package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"

	"github.com/patrick-1984/ai-minion/internal/client"
	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/worker/agent"
)

// pairCancelJSON runs "minion worker pair cancel --json ARGS" with the real
// pair command tree, so that "pair cancel" routes to the subcommand.
func pairCancelJSON(t *testing.T, d workerPairCancelDeps, args ...string) (map[string]any, int) {
	t.Helper()
	root := &cobra.Command{Use: "minion", SilenceUsage: true, SilenceErrors: true}
	pair := newWorkerPairCmd(workerPairDeps{lookupEnv: func(string) (string, bool) { return "", false }})
	pair.AddCommand(newWorkerPairCancelCmd(d))
	workerCommand(root).AddCommand(pair)
	var out, errOut bytes.Buffer
	code := execute(context.Background(), root, append([]string{"worker", "pair", "cancel", "--json"}, args...), &out, &errOut)
	var m map[string]any
	if err := json.Unmarshal(out.Bytes(), &m); err != nil {
		t.Fatalf("stdout %q: %v (stderr %q)", out.String(), err, errOut.String())
	}
	return m, code
}

func pairCancelDeps(t *testing.T, state string, op func(args agent.LocalPairCancelArgs) (json.RawMessage, error)) workerPairCancelDeps {
	t.Helper()
	return workerPairCancelDeps{
		lookupEnv: func(k string) (string, bool) {
			if k == "MINION_STATE_DIR" {
				return state, true
			}
			return "", false
		},
		call: func(_ context.Context, name string, args any) (json.RawMessage, error) {
			switch name {
			case agent.OpLocalStatus:
				return json.Marshal(agent.LiveStatusReply{StateDir: state, Instance: "i1"})
			case "worker_pair_cancel+pair_cancel.v1":
				a := args.(agent.LocalPairCancelArgs)
				if a.Instance != "i1" {
					t.Fatalf("instance %q", a.Instance)
				}
				return op(a)
			}
			t.Fatalf("op %q", name)
			return nil, nil
		},
	}
}

func TestPairCancelCommand(t *testing.T) {
	state := t.TempDir()
	d := pairCancelDeps(t, state, func(a agent.LocalPairCancelArgs) (json.RawMessage, error) {
		return json.Marshal(agent.PairCancelResult{Cancelled: true, Attempt: a.Attempt, ResumeDeleted: true,
			Cleared: []string{config.KeyPairingDisplay, config.KeyServer}, Kept: []string{config.KeyHubFingerprint}})
	})
	m, code := pairCancelJSON(t, d, "--attempt", "a1")
	if code != ExitOK || m["cancelled"] != true || m["attempt"] != "a1" || m["resume_deleted"] != true || m["state_dir"] != state {
		t.Fatalf("cancel = %v, %d", m, code)
	}
	if c, _ := m["cleared"].([]any); len(c) != 2 {
		t.Fatalf("cleared = %v", m["cleared"])
	}
	if k, _ := m["kept"].([]any); len(k) != 1 || k[0] != config.KeyHubFingerprint {
		t.Fatalf("kept = %v", m["kept"])
	}
}

// A refusal state_changed of the Worker (stale attempt, paired meanwhile) is
// 27; an older Worker is 20; no attempt is a usage error.
func TestPairCancelCommandRefusals(t *testing.T) {
	state := t.TempDir()
	stale := pairCancelDeps(t, state, func(agent.LocalPairCancelArgs) (json.RawMessage, error) {
		pe := proto.NewError(proto.CodeConflict, "the pairing attempt a1 is no longer the one shown; nothing was changed")
		pe.Details, _ = json.Marshal(map[string]any{"reason": "state_changed"})
		return nil, pe
	})
	if m, code := pairCancelJSON(t, stale, "--attempt", "a1"); code != ExitStateChanged || m["code"] != "state_changed" || m["cancelled"] != false {
		t.Fatalf("stale = %v, %d", m, code)
	}
	old := pairCancelDeps(t, state, func(agent.LocalPairCancelArgs) (json.RawMessage, error) {
		return nil, proto.NewError(proto.CodeUnsupportedOp, "unknown op")
	})
	if m, code := pairCancelJSON(t, old, "--attempt", "a1"); code != ExitWorkerTooOld {
		t.Fatalf("old Worker = %v, %d", m, code)
	}
	if m, code := pairCancelJSON(t, old); code != ExitUsage {
		t.Fatalf("no attempt = %v, %d", m, code)
	}
}

// No running Worker, or one of another state directory: 24.
func TestPairCancelWithoutLiveWorkerIs24(t *testing.T) {
	state := t.TempDir()
	d := pairCancelDeps(t, state, nil)
	d.call = func(context.Context, string, any) (json.RawMessage, error) { return nil, client.ErrWorkerNotRunning }
	if m, code := pairCancelJSON(t, d, "--attempt", "a1"); code != ExitNotRunning || m["code"] != "not_running" {
		t.Fatalf("no Worker = %v, %d", m, code)
	}
	d.call = func(_ context.Context, op string, _ any) (json.RawMessage, error) {
		if op != agent.OpLocalStatus {
			t.Fatalf("op %q sent to the Worker of another state directory", op)
		}
		return json.Marshal(agent.LiveStatusReply{StateDir: t.TempDir(), Instance: "i1"})
	}
	if m, code := pairCancelJSON(t, d, "--attempt", "a1"); code != ExitNotRunning {
		t.Fatalf("other state dir = %v, %d", m, code)
	}
}

// A new background join asks for the pairing that a cancel stopped: it
// removes the cancel record.
func TestJoinBackgroundRemovesPairCancel(t *testing.T) {
	c := newJoinCLI(t)
	if err := os.WriteFile(filepath.Join(c.state, agent.PairCancelFile), []byte(`{"version":1,"server":"https://hub.example:8443"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if m, code := c.run("", "https://hub.example:8443", "--background", "--share-existing", "later"); code != ExitOK {
		t.Fatalf("join = %v, %d", m, code)
	}
	if _, ok, err := agent.ReadPairCancel(c.state); ok || err != nil {
		t.Fatalf("cancel record after the join: %v, %v", ok, err)
	}
}
