package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/patrick-1984/ai-minion/internal/client"
	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/worker/agent"
	"github.com/patrick-1984/ai-minion/internal/worker/store"
)

func runVerifyHub(t *testing.T, state string, args ...string) (string, error) {
	t.Helper()
	return runVerifyHubWith(t, state, nil, args...)
}

func runVerifyHubWith(t *testing.T, state string, tune func(*workerVerifyHubDeps), args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	deps := workerVerifyHubDeps{
		stdout:    &out,
		lookupEnv: envFrom(map[string]string{"MINION_STATE_DIR": state}),
		openStore: func(ctx context.Context, dir string) (*store.Store, error) { return store.OpenContext(ctx, dir) },
		lockState: agent.LockLifecycle,
		sleep:     func(time.Duration) {},
	}
	if tune != nil {
		tune(&deps)
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
	if ee, ok := err.(*exitCodeError); !ok || ee.code != ExitError || !strings.Contains(out, agent.MismatchTitle) ||
		strings.Contains(out, "confirmed") || !strings.Contains(out, "No Worker is running on this state directory") {
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

// heldLock is a lifecycle lock that a running Worker holds.
func heldLock(string) (io.Closer, error) { return nil, agent.ErrStateInUse }

// Finding 4 of the round-1 review: verify-hub against a running Worker.
func TestWorkerVerifyHubWithARunningWorker(t *testing.T) {
	fp := fakeHubFingerprint(t)
	c := newPairCLI(t, nil)
	c.pair()
	other, _ := identity.GenerateKey()
	status := func(reply agent.LiveStatusReply) func(context.Context, string) (json.RawMessage, error) {
		return func(_ context.Context, op string) (json.RawMessage, error) {
			if op != agent.OpLocalStatus {
				return nil, errors.New("unexpected op " + op)
			}
			return json.Marshal(reply)
		}
	}

	// An older Worker (no hub_check_aware): nothing changes, and the command
	// says how to stop it. The state is not even opened.
	opened := false
	out, err := runVerifyHubWith(t, c.state, func(d *workerVerifyHubDeps) {
		d.lockState = heldLock
		d.call = status(agent.LiveStatusReply{StateDir: c.state, State: "connected"})
		open := d.openStore
		d.openStore = func(ctx context.Context, dir string) (*store.Store, error) { opened = true; return open(ctx, dir) }
	}, string(other.Fingerprint()))
	if err == nil || !strings.Contains(err.Error(), "older than v0.11.0") || !strings.Contains(err.Error(), "minion worker down") || opened {
		t.Fatalf("older Worker: %v %q opened=%v", err, out, opened)
	}
	if enr, _ := c.enrollment(); enr.Check() == store.HubCheckMismatch {
		t.Fatal("the check was stored although an older Worker runs")
	}

	// A running Worker that does not answer for this state directory.
	_, err = runVerifyHubWith(t, c.state, func(d *workerVerifyHubDeps) {
		d.lockState = heldLock
		d.call = func(context.Context, string) (json.RawMessage, error) { return nil, client.ErrWorkerNotRunning }
	}, string(other.Fingerprint()))
	if err == nil || !strings.Contains(err.Error(), "does not answer") {
		t.Fatalf("silent Worker: %v", err)
	}

	// A v0.11 Worker that has not gone idle: no claim of a disconnection.
	out, err = runVerifyHubWith(t, c.state, func(d *workerVerifyHubDeps) {
		d.lockState = heldLock
		d.call = status(agent.LiveStatusReply{StateDir: c.state, State: "connected", HubCheckAware: true})
	}, string(other.Fingerprint()))
	if ee, ok := err.(*exitCodeError); !ok || ee.code != ExitError || !strings.Contains(out, "has not confirmed yet") || strings.Contains(out, "confirmed: it stopped") {
		t.Fatalf("unconfirmed: %v %q", err, out)
	}
	if enr, _ := c.enrollment(); enr.Check() != store.HubCheckMismatch {
		t.Fatalf("enrollment %+v", enr)
	}

	// A v0.11 Worker that reports the quarantine: confirmed.
	out, _ = runVerifyHubWith(t, c.state, func(d *workerVerifyHubDeps) {
		d.lockState = heldLock
		d.call = status(agent.LiveStatusReply{StateDir: c.state, State: string(agent.ConnIdle), Reason: agent.ReasonQuarantined, HubCheckAware: true})
	}, string(other.Fingerprint()))
	if !strings.Contains(out, "The running Worker confirmed: it stopped talking to that Hub") {
		t.Fatalf("confirmed: %q", out)
	}

	// Words do not end the quarantine; the full key does.
	out, err = runVerifyHub(t, c.state, fp.Words())
	if err == nil || !strings.Contains(out, "full fingerprint (52 characters)") {
		t.Fatalf("words in quarantine: %v %q", err, out)
	}
	if out, err = runVerifyHub(t, c.state, string(fp)); err != nil || !strings.Contains(out, "matches") {
		t.Fatalf("full: %v %q", err, out)
	}
}

// Finding 2: --forget removes a stored invitation key from the config file
// and records the removal.
func TestWorkerVerifyHubForgetRemovesTheStoredInvitationKey(t *testing.T) {
	fp := fakeHubFingerprint(t)
	c := newPairCLI(t, nil)
	c.pair()
	if _, err := writeWorkerSettings(c.state, map[string]string{"invite_key": "mnk_placeholder.secret"}); err != nil {
		t.Fatal(err)
	}
	out, err := runVerifyHub(t, c.state, "--forget", "--pinned", string(fp))
	if err != nil || !strings.Contains(out, "Removed the invitation key") || !strings.Contains(out, "does not enroll again by itself") {
		t.Fatalf("forget: %v %q", err, out)
	}
	data, _ := os.ReadFile(filepath.Join(c.state, workerConfigName))
	if strings.Contains(string(data), "invite_key") {
		t.Fatalf("config file still holds the invitation key: %s", data)
	}
	if got, _ := agent.ForgottenHubs(c.state); len(got) != 1 || got[0] != string(fp) {
		t.Fatalf("record = %v", got)
	}
}

// Round 2: with no Worker running, verify-hub holds the lifecycle lock through
// the check, the migration and the write: a Worker that starts meanwhile (a
// supervised older one) cannot take it, and the lock is released at the end.
func TestWorkerVerifyHubHoldsTheLifecycleLock(t *testing.T) {
	c := newPairCLI(t, nil)
	c.pair()
	other, _ := identity.GenerateKey()
	var during error
	out, err := runVerifyHubWith(t, c.state, func(d *workerVerifyHubDeps) {
		open := d.openStore
		d.openStore = func(ctx context.Context, dir string) (*store.Store, error) {
			l, err := agent.LockLifecycle(dir)
			if err == nil {
				_ = l.Close()
			}
			during = err
			return open(ctx, dir)
		}
	}, string(other.Fingerprint()))
	if ee, ok := err.(*exitCodeError); !ok || ee.code != ExitError || !strings.Contains(out, agent.MismatchTitle) {
		t.Fatalf("mismatch: %v %q", err, out)
	}
	if !errors.Is(during, agent.ErrStateInUse) {
		t.Fatalf("a Worker could take the lifecycle lock while verify-hub wrote: %v", during)
	}
	l, err := agent.LockLifecycle(c.state)
	if err != nil {
		t.Fatalf("the lock was not released: %v", err)
	}
	_ = l.Close()
}
