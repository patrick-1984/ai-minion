package cli

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/worker/agent"
)

// First-run "worker up --invite-key -" with the share answer and the
// invitation on the same stdin: the wizard takes only its answer line and the
// real enrollment hook receives the invitation. The key is synthetic.
func TestFirstRunUpStdinInvitationReachesHook(t *testing.T) {
	state := t.TempDir()
	writeConfigFile(t, state, map[string]string{config.KeyServer: "https://hub.example"})
	deps, _, rig := foregroundGate(t, true, map[string]string{"MINION_STATE_DIR": state})
	const secret = "mnk_STDIN_CANARY.STDIN_CANARY"
	input := strings.NewReader("1\n" + secret + "\n")
	stop := errors.New("bounded hook stop")
	var got string
	err := deps.run.registry.RegisterPreStart("observe-invitation", func(ctx context.Context, s agent.Settings) error {
		key, err := config.ResolveWorkerInviteKeyContext(ctx, s.Resolver, input)
		if err != nil {
			t.Errorf("invitation unavailable to the hook: %v", err)
		}
		got = key.Value
		return stop
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := runUp(deps, ctx, input, "--foreground", "--invite-key", "-")
	if !errors.Is(err, stop) {
		t.Fatalf("unexpected exit: %v", err)
	}
	if got != secret {
		t.Fatalf("wizard consumed the invitation; hook got %d bytes, want %d", len(got), len(secret))
	}
	if rig.pair.n() != 0 {
		t.Fatal("PAKE used with an invitation")
	}
	if strings.Contains(out, secret) || strings.Contains(err.Error(), secret) {
		t.Fatal("invitation leaked to output")
	}
}

// The same through the real native enrollment hook and a real stdin pipe in a
// child process. The synthetic key is malformed on purpose: it must reach the
// native parser, which fails before any dial.
func TestFirstRunUpStdinInvitationReachesNativeHook(t *testing.T) {
	if os.Getenv("RELEASE_GATE_STDIN_CHILD") != "1" {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestFirstRunUpStdinInvitationReachesNativeHook$")
		cmd.Env = append(os.Environ(), "RELEASE_GATE_STDIN_CHILD=1")
		cmd.Stdin = strings.NewReader("1\nmnk_SYNTHETIC_MALFORMED_CANARY\n")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("child: %v\n%s", err, out)
		}
		if strings.Contains(string(out), "SYNTHETIC_MALFORMED_CANARY") {
			t.Fatal("invitation leaked to child output")
		}
		return
	}
	state := t.TempDir()
	writeConfigFile(t, state, map[string]string{config.KeyServer: "https://hub.example"})
	deps, _, rig := foregroundGate(t, true, map[string]string{"MINION_STATE_DIR": state})
	deps.run.registry = agent.NewRegistry()
	if err := deps.run.registry.RegisterPreStart("real-native-hooks", agent.DefaultRegistry.RunPreStart); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := runUp(deps, ctx, os.Stdin, "--foreground", "--invite-key", "-")
	if err == nil || !strings.Contains(err.Error(), "invitation key is malformed") {
		t.Fatalf("native hook never received the stdin key: %v", err)
	}
	if strings.Contains(err.Error(), "SYNTHETIC_MALFORMED_CANARY") {
		t.Fatal("invitation echoed in error")
	}
	if rig.pair.n() != 0 {
		t.Fatal("PAKE over invitation")
	}
}
