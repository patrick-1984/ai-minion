package agent

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/config"
)

// Environment of the helper process of [TestConcurrentProcessesEnrollOneWorkerIdentity].
const (
	envEnrollChild   = "MINION_TEST_ENROLL_CHILD"
	envEnrollHub     = "MINION_TEST_ENROLL_HUB"
	envEnrollKeyFile = "MINION_TEST_ENROLL_KEYFILE"
	envEnrollState   = "MINION_TEST_ENROLL_STATE"
	envEnrollGo      = "MINION_TEST_ENROLL_GO"
)

// TestEnrollChildProcess is not a test of its own: it is the body of the
// processes that TestConcurrentProcessesEnrollOneWorkerIdentity starts. It
// waits for the start barrier, then runs the production pre-start hook on the
// shared state directory, like `minion worker run --invite-key` does.
func TestEnrollChildProcess(t *testing.T) {
	if os.Getenv(envEnrollChild) != "1" {
		t.Skip("helper process of the concurrent enrollment test")
	}
	goFile := os.Getenv(envEnrollGo)
	for deadline := time.Now().Add(30 * time.Second); ; {
		if _, err := os.Stat(goFile); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no start signal")
		}
		time.Sleep(time.Millisecond)
	}
	var out bytes.Buffer
	res := inviteResolver(config.MapSource{
		config.KeyServer:    os.Getenv(envEnrollHub),
		config.KeyInviteKey: os.Getenv(envEnrollKeyFile),
	}, nil, nil)
	s := Settings{
		WorkerCore: config.WorkerCore{StateDir: os.Getenv(envEnrollState), Name: "pi"},
		Resolver:   res,
		Out:        &out,
	}
	d := defaultInviteHookDeps()
	d.stdin, d.stderr = strings.NewReader(""), nil
	d.attempts = 1
	if err := d.run(context.Background(), s); err != nil {
		t.Fatalf("hook: %v", err)
	}
}

// Two or more Workers that start on one unenrolled state directory at once
// must end with one identity: the key on disk is the key the Hub enrolled with
// the one-time invitation, every process succeeds (one enrolls, the others
// find the Worker enrolled), and the Hub saw exactly one enrolled key.
func TestConcurrentProcessesEnrollOneWorkerIdentity(t *testing.T) {
	if testing.Short() {
		t.Skip("starts several processes")
	}
	hub, invite := newInviteHub(t)
	hub.oneTime = true
	dir := t.TempDir()
	state := filepath.Join(dir, "state")
	keyFile := filepath.Join(dir, "invite.key")
	if err := os.WriteFile(keyFile, []byte(invite.String()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	goFile := filepath.Join(dir, "go")

	const procs = 8
	type result struct {
		cmd *exec.Cmd
		out *bytes.Buffer
	}
	var all []result
	for i := 0; i < procs; i++ {
		cmd := exec.Command(os.Args[0], "-test.run=^TestEnrollChildProcess$", "-test.count=1")
		cmd.Env = append(os.Environ(),
			envEnrollChild+"=1", envEnrollHub+"="+hub.url(), envEnrollKeyFile+"="+keyFile,
			envEnrollState+"="+state, envEnrollGo+"="+goFile)
		var buf bytes.Buffer
		cmd.Stdout, cmd.Stderr = &buf, &buf
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		all = append(all, result{cmd, &buf})
	}
	t.Cleanup(func() {
		for _, r := range all {
			_ = r.cmd.Process.Kill()
		}
	})
	if err := os.WriteFile(goFile, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for i, r := range all {
		if err := r.cmd.Wait(); err != nil {
			t.Errorf("process %d failed: %v\n%s", i, err, r.out.String())
		}
	}
	if t.Failed() {
		return
	}

	st := openWorkerState(t, state)
	enr, ok, err := st.Load(context.Background())
	if err != nil || !ok {
		t.Fatalf("enrollment: %v, %v", ok, err)
	}
	k, err := st.LoadKey()
	if err != nil {
		t.Fatal(err)
	}
	hub.mu.Lock()
	defer hub.mu.Unlock()
	id, known := hub.known[string(k.Public())]
	if !known {
		t.Fatalf("the Worker key on disk is not the key the Hub enrolled (the Hub enrolled %d keys)", len(hub.known))
	}
	if len(hub.known) != 1 || enr.WorkerID != id {
		t.Fatalf("hub enrolled %d keys, stored worker id %q, hub id %q", len(hub.known), enr.WorkerID, id)
	}
	if got := len(hub.received); got != 1 {
		t.Fatalf("the secret was sent %d times, want once: the others must have seen the Worker enrolled", got)
	}
}

// The enrollment lock waits for its holder and ends with the context.
func TestEnrollmentLockWaitsAndHonorsTheContext(t *testing.T) {
	path := filepath.Join(t.TempDir(), enrollLockName)
	release, err := lockEnrollment(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	if _, err := lockEnrollment(ctx, path); err == nil {
		t.Fatal("the second lock succeeded while the first was held")
	}
	release()
	again, err := lockEnrollment(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	again()
}
