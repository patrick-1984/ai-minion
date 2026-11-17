package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/worker/agent"
	"github.com/patrick-1984/ai-minion/internal/worker/store"
)

func runVerifyHub(t *testing.T, state string, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	deps := workerVerifyHubDeps{
		stdout:    &out,
		lookupEnv: envFrom(map[string]string{"MINION_STATE_DIR": state}),
		openStore: func(ctx context.Context, dir string) (*store.Store, error) { return store.OpenContext(ctx, dir) },
	}
	root := &cobra.Command{Use: "minion", SilenceUsage: true, SilenceErrors: true}
	workerCommand(root).AddCommand(newWorkerVerifyHubCmd(deps))
	root.SetArgs(append([]string{"worker", "verify-hub"}, args...))
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	err := root.ExecuteContext(context.Background())
	return out.String(), err
}

func TestWorkerVerifyHub(t *testing.T) {
	fp := fakeHubFingerprint(t)
	c := newPairCLI(t, nil)

	out, err := runVerifyHub(t, c.state, fp.Words())
	if ee, ok := err.(*exitCodeError); !ok || ee.code != ExitError || !strings.Contains(out, "not paired") {
		t.Fatalf("not paired: %v %q", err, out)
	}
	c.pair()

	// Six words as six arguments, 4-letter prefixes.
	var prefixes []string
	for _, w := range strings.Split(fp.Words(), "-") {
		prefixes = append(prefixes, w[:min(4, len(w))])
	}
	out, err = runVerifyHub(t, c.state, prefixes...)
	if err != nil || !strings.Contains(out, "matches the fingerprint you gave") {
		t.Fatalf("match: %v %q", err, out)
	}
	if enr, _ := c.enrollment(); enr.Check() != store.HubCheckFingerprint {
		t.Fatalf("enrollment %+v", enr)
	}

	other, _ := identity.GenerateKey()
	out, err = runVerifyHub(t, c.state, string(other.Fingerprint()))
	if ee, ok := err.(*exitCodeError); !ok || ee.code != ExitError || !strings.Contains(out, "WORKER DISCONNECTED") {
		t.Fatalf("mismatch: %v %q", err, out)
	}
	if enr, _ := c.enrollment(); enr.Check() != store.HubCheckMismatch {
		t.Fatalf("enrollment %+v", enr)
	}

	if _, err = runVerifyHub(t, c.state, "--forget"); err == nil {
		t.Fatal("--forget without --pinned must fail")
	}
	if _, err = runVerifyHub(t, c.state, "--forget", "--pinned", string(other.Fingerprint())); err == nil {
		t.Fatal("--forget with another key must fail")
	}
	if _, ok := c.enrollment(); !ok {
		t.Fatal("a wrong --pinned removed the pairing")
	}
	out, err = runVerifyHub(t, c.state, "--forget", "--pinned", string(fp))
	if err != nil || !strings.Contains(out, "Removed the pairing") {
		t.Fatalf("forget: %v %q", err, out)
	}
	if _, ok := c.enrollment(); ok {
		t.Fatal("the pairing is still there")
	}
	if _, err := runVerifyHub(t, c.state, "abc"); err == nil {
		t.Fatal("a malformed fingerprint must fail")
	}
	_ = agent.VerifyMatch
}
