package cli

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/worker/setup"
	"github.com/patrick-1984/ai-minion/internal/worker/store"
)

// Regression tests of the first review of Story 4.13. They run the command
// (cobra) on the real Worker state (a store and a config file in a temp dir)
// and the real config lock; only the Hub and the terminal are faked.

// sameState is a second run of the wizard on the state of r.
func (r *setupRig) sameState(tty bool) *setupRig {
	r2 := newSetupRig(r.t, tty, "build")
	r2.state = r.state
	r2.env["MINION_STATE_DIR"] = r.state
	return r2
}

// pairedAt runs the interactive wizard once so that the Worker is paired with
// the Hub at addr, and returns the rig.
func pairedAt(t *testing.T, addr string) *setupRig {
	t.Helper()
	r := newSetupRig(t, true, "build")
	k := newKeys(t)
	k.typeLines("1", addr)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := r.run(ctx, k.r); err != nil {
		t.Fatalf("first setup: %v\n%s", err, r.stdout)
	}
	if e, ok := r.enrollment(); !ok || e.Owner != "alice" || e.HubURL != addr {
		t.Fatalf("enrollment = %+v, %v; want alice at %s", e, ok, addr)
	}
	return r
}

// Finding 1: with the Worker paired to Hub A, a setup that names Hub B must not
// save B, check B, report A's pairing as done, or change anything.
func TestWorkerSetupChangingTheHubKeepsThePairingAndTheConfiguration(t *testing.T) {
	const a, b = "https://hub-a.example:8443", "https://hub-b.example:8443"
	r := pairedAt(t, a)
	before, _ := r.enrollment()

	t.Run("flags", func(t *testing.T) {
		r2 := r.sameState(false)
		err := r2.run(context.Background(), readFailer{t}, "--share-existing", "none", "--server", b)
		if !errors.Is(err, setup.ErrHubChange) {
			t.Fatalf("err = %v, want ErrHubChange", err)
		}
		for _, want := range []string{"paired with the Hub at " + a, "not the one at " + b, "Nothing was changed", "--new-code", "minion worker reset"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error lacks %q: %v", want, err)
			}
		}
		if got := r2.file()[config.KeyServer]; got != a {
			t.Errorf("server in the file = %q, want %q kept", got, a)
		}
		if sel := r2.share(); sel.Mode != store.ShareAll {
			t.Errorf("share = %+v, want the earlier choice (all) kept", sel)
		}
		if got, _ := r2.enrollment(); got.HubURL != a || got.Owner != before.Owner || string(got.Pin.Pub) != string(before.Pin.Pub) {
			t.Errorf("enrollment changed: %+v, was %+v", got, before)
		}
		if len(r2.check.calls()) != 0 {
			t.Errorf("the new Hub was checked: %v", r2.check.calls())
		}
		if strings.Contains(r2.stdout.String(), "already paired") || strings.Contains(r2.stdout.String(), "Setup is done") {
			t.Errorf("setup reported success:\n%s", r2.stdout)
		}
		if r2.pair.n() != 0 {
			t.Error("a pairing ran")
		}
	})

	t.Run("terminal", func(t *testing.T) {
		r2 := r.sameState(true)
		k := newKeys(t)
		k.typeLines("4", b, "") // share none; the other Hub; Enter keeps the saved one
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		if err := r2.run(ctx, k.r); err != nil {
			t.Fatalf("setup: %v\n%s", err, r2.stdout)
		}
		out := r2.stdout.String()
		if !strings.Contains(out, "Nothing was changed") || !strings.Contains(out, "This Worker is already paired to alice.") {
			t.Errorf("output:\n%s", out)
		}
		for _, c := range r2.check.calls() {
			if c == b {
				t.Errorf("the new Hub was checked")
			}
		}
		if got := r2.file()[config.KeyServer]; got != a {
			t.Errorf("server = %q, want %q", got, a)
		}
	})

	t.Run("the same Hub written differently is the same Hub", func(t *testing.T) {
		r2 := r.sameState(false)
		if err := r2.run(context.Background(), readFailer{t}, "--share-existing", "none", "--server", "HTTPS://Hub-A.example:8443/"); err != nil {
			t.Fatalf("setup: %v\n%s", err, r2.stdout)
		}
		if sel := r2.share(); sel.Mode != store.ShareNone {
			t.Errorf("share = %+v, want none", sel)
		}
	})
}

// An enrollment by invitation records no Hub address: the saved address is the
// Hub it belongs to.
func TestWorkerSetupChangingTheHubOfAnInvitationEnrollmentIsRefused(t *testing.T) {
	const a, b = "https://hub-a.example:8443", "https://hub-b.example:8443"
	r := pairedAt(t, a)
	// Forget the recorded address, as for an enrollment stored by an invitation.
	st, err := store.OpenContext(context.Background(), r.state)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	r2 := r.sameState(false)
	err = r2.run(context.Background(), readFailer{t}, "--share-existing", "none", "--server", b)
	if !errors.Is(err, setup.ErrHubChange) {
		t.Fatalf("err = %v, want ErrHubChange", err)
	}
	if got := r2.file()[config.KeyServer]; got != a {
		t.Errorf("server = %q, want %q", got, a)
	}
}

func inviteFile(t *testing.T, secret string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "invite.key")
	if err := os.WriteFile(p, []byte(secret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func writeConfigFile(t *testing.T, state string, kv map[string]string) {
	t.Helper()
	f, err := config.LoadFile(filepath.Join(state, workerConfigName))
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range kv {
		f.Set(k, v)
	}
	if err := f.Save(); err != nil {
		t.Fatal(err)
	}
}

// Finding 2: "worker setup" does not enroll and does not keep an invitation
// key, so it must not promise that it is used later; it tells the user to give
// it again to "worker up". It asks nothing and prints and stores no secret.
func TestWorkerSetupStandaloneInvitationFlagDoesNotPromiseAnAutomaticEnrollment(t *testing.T) {
	const secret = "mnk_SECRETCANARY.SECRETCANARY"
	cases := map[string]func(t *testing.T) []string{
		"file":    func(t *testing.T) []string { return []string{"--invite-key", inviteFile(t, secret)} },
		"literal": func(*testing.T) []string { return []string{"--invite-key", secret} },
	}
	for name, mk := range cases {
		t.Run(name, func(t *testing.T) {
			r := newSetupRig(t, false)
			args := append([]string{"--share-existing", "none", "--server", "https://hub.example:8443"}, mk(t)...)
			if err := r.run(context.Background(), readFailer{t}, args...); err != nil {
				t.Fatalf("setup: %v\n%s", err, r.stdout)
			}
			out := r.stdout.String()
			for _, want := range []string{"does not use it", "does not enroll", "give it again to `minion worker up`", "No pairing code is needed"} {
				if !strings.Contains(out, want) {
					t.Errorf("output lacks %q:\n%s", want, out)
				}
			}
			if strings.Contains(out, "enrolls this Worker with it when it starts (minion worker up)") {
				t.Errorf("setup promised an automatic enrollment:\n%s", out)
			}
			if strings.Contains(out, "SECRETCANARY") {
				t.Errorf("the key was printed:\n%s", out)
			}
			if _, ok := r.enrollment(); ok {
				t.Error("setup enrolled the Worker")
			}
			if r.pair.n() != 0 {
				t.Error("a pairing ran")
			}
			if _, has := r.file()[config.KeyInviteKey]; has {
				t.Error("the invitation key was stored in the config file")
			}
			if raw, err := os.ReadFile(filepath.Join(r.state, workerConfigName)); err == nil && strings.Contains(string(raw), "SECRETCANARY") {
				t.Error("the config file holds the key")
			}
		})
	}
}

// A key that is already stored in the config file is still there for "worker
// up": setup says so and leaves it.
func TestWorkerSetupWithAStoredInvitationKeySaysUpUsesIt(t *testing.T) {
	r := newSetupRig(t, false)
	writeConfigFile(t, r.state, map[string]string{config.KeyInviteKey: "mnk_stored.stored"})
	if err := r.run(context.Background(), readFailer{t}, "--share-existing", "all", "--server", "https://hub.example:8443"); err != nil {
		t.Fatalf("setup: %v\n%s", err, r.stdout)
	}
	if out := r.stdout.String(); !strings.Contains(out, "stored in the config file") || !strings.Contains(out, "`minion worker up` enrolls this Worker with it") {
		t.Errorf("output:\n%s", out)
	}
	if got := r.file()[config.KeyInviteKey]; got != "mnk_stored.stored" {
		t.Errorf("the stored key = %q, want it kept", got)
	}
	if r.pair.n() != 0 {
		t.Error("a pairing ran")
	}
}

// Finding 3: the gate of "worker up" uses the detector of the enrollment hook,
// including the config file and the precedence of an explicit empty flag.
func TestWorkerUpGateHonoursAStoredInvitationKeyAndAnEmptyFlag(t *testing.T) {
	run := func(t *testing.T, args ...string) (string, *setupRig) {
		state := t.TempDir()
		writeConfigFile(t, state, map[string]string{
			config.KeyServer: "https://hub.example:8443", config.KeyInviteKey: "mnk_stored.stored",
		})
		deps, p, rig := foregroundGate(t, true, map[string]string{"MINION_STATE_DIR": state}, "build")
		k := newKeys(t)
		k.typeLines("1") // share all; the Hub address is in the file
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		var out syncBuf
		done := make(chan error, 1)
		go func() { done <- runUpOut(deps, ctx, k.r, &out, append([]string{"--foreground"}, args...)...) }()
		select {
		case <-p.ready:
		case err := <-done:
			t.Fatalf("worker up ended before the Worker started: %v\n%s", err, &out)
		case <-time.After(30 * time.Second):
			t.Fatal("worker up did not start")
		}
		cancel()
		if err := <-done; err != nil {
			t.Errorf("worker up: %v", err)
		}
		return out.String(), rig
	}

	t.Run("stored key: no pairing code", func(t *testing.T) {
		out, rig := run(t)
		if rig.pair.n() != 0 || strings.Contains(out, "Pairing code:") || !strings.Contains(out, "An invitation key was given") {
			t.Errorf("pairings = %d, output:\n%s", rig.pair.n(), out)
		}
	})
	t.Run("an empty --invite-key hides the stored key", func(t *testing.T) {
		out, rig := run(t, "--invite-key", "")
		if rig.pair.n() != 1 || strings.Contains(out, "An invitation key was given") {
			t.Errorf("pairings = %d, output:\n%s", rig.pair.n(), out)
		}
	})
}

// Finding 4: Ctrl-C (SIGINT/SIGTERM cancel the context) ends the wait for the
// config lock; nothing is written, and the lock of the other process is not
// touched.
func TestWorkerSetupCancelWhileWaitingForTheConfigLockPersistsNothing(t *testing.T) {
	r := newSetupRig(t, false)
	if err := os.MkdirAll(r.state, 0o700); err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(r.state, configLockName)
	release, err := lockConfigFile(lockPath) // another config command holds the lock
	if err != nil {
		t.Fatal(err)
	}
	held := true
	defer func() {
		if held {
			release()
		}
	}()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- r.run(ctx, readFailer{t}, "--share-existing", "all", "--server", "https://hub.example:8443")
	}()
	waitForOutput(t, r.stdout, "Checking https://hub.example:8443") // the wizard now waits for the lock
	select {
	case err := <-done:
		t.Fatalf("setup ended while the lock was held: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("setup stayed blocked on the config lock after cancellation")
	}
	if got, has := r.file()[config.KeyServer]; has {
		t.Errorf("a server was saved after the cancellation: %q", got)
	}
	if sel := r.share(); sel.Configured {
		t.Errorf("a share choice was saved after the cancellation: %+v", sel)
	}
	// The holder still holds the lock: the cancelled command did not unlock it.
	tctx, tcancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer tcancel()
	if rel, err := lockConfigFileContext(tctx, lockPath); err == nil {
		rel()
		t.Error("the lock of the other command was released")
	}
	release()
	held = false
	rel, err := lockConfigFileContext(context.Background(), lockPath)
	if err != nil {
		t.Fatalf("the lock is not free after the holder released it: %v", err)
	}
	rel()
}

// cancellingChecker cancels the command while the Hub address is checked, so
// that the cancellation arrives right before the save.
type cancellingChecker struct {
	okChecker
	cancel context.CancelFunc
}

func (c *cancellingChecker) Check(ctx context.Context, hubURL string) setup.Report {
	rep := c.okChecker.Check(ctx, hubURL)
	c.cancel()
	return rep
}

func TestWorkerSetupCancelledBeforeTheSaveWritesNeitherPersistenceLayer(t *testing.T) {
	r := newSetupRig(t, false)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cc := &cancellingChecker{cancel: cancel}
	r.deps.checker = func(config.WorkerHub) (setup.HubChecker, error) { return cc, nil }
	err := r.run(ctx, readFailer{t}, "--share-existing", "all", "--server", "https://hub.example:8443")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if got, has := r.file()[config.KeyServer]; has {
		t.Errorf("a server was saved: %q", got)
	}
	if sel := r.share(); sel.Configured {
		t.Errorf("a share choice was saved: %+v", sel)
	}
}

// Finding 5: query and fragment of a pasted address are not used by the
// connection, so they are refused early without echoing them, or never shown.
func TestWorkerSetupRefusesQueryAndFragmentWithoutEchoingThem(t *testing.T) {
	const canary = "QUERYCANARY"
	for _, addr := range []string{
		"https://hub.example:8443/base?token=" + canary,
		"https://hub.example:8443/base#frag-" + canary,
		"https://hub.example:8443/base?token=" + canary + "#frag-" + canary,
		"https://hub.example:8443?" + canary,
	} {
		t.Run(addr, func(t *testing.T) {
			r := newSetupRig(t, false)
			err := r.run(context.Background(), readFailer{t}, "--share-existing", "all", "--server", addr)
			if err == nil {
				t.Fatalf("setup accepted %q:\n%s", addr, r.stdout)
			}
			if !errors.Is(err, config.ErrInvalidServer) || !strings.Contains(err.Error(), "query") {
				t.Errorf("err = %v", err)
			}
			if strings.Contains(err.Error(), canary) || strings.Contains(r.stdout.String(), canary) {
				t.Errorf("the credential was echoed: %v\n%s", err, r.stdout)
			}
			if len(r.check.calls()) != 0 {
				t.Errorf("a Hub was checked: %v", r.check.calls())
			}
			if _, has := r.file()[config.KeyServer]; has || r.share().Configured {
				t.Error("something was saved")
			}
		})
	}

	t.Run("environment", func(t *testing.T) {
		r := newSetupRig(t, false)
		r.env["MINION_SERVER"] = "https://hub.example:8443/?token=" + canary
		r.env["MINION_SHARE_EXISTING"] = "all"
		err := r.run(context.Background(), readFailer{t})
		if err == nil || strings.Contains(err.Error(), canary) || strings.Contains(r.stdout.String(), canary) {
			t.Errorf("err = %v\n%s", err, r.stdout)
		}
	})

	t.Run("typed on a terminal", func(t *testing.T) {
		r := newSetupRig(t, true, "build")
		k := newKeys(t)
		k.typeLines("1", "https://hub.example:8443/?token="+canary, "solo")
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		if err := r.run(ctx, k.r); err != nil {
			t.Fatalf("setup: %v\n%s", err, r.stdout)
		}
		out := r.stdout.String()
		if strings.Contains(out, canary) || !strings.Contains(out, "must not contain a query") {
			t.Errorf("output:\n%s", out)
		}
		if len(r.check.calls()) != 0 {
			t.Errorf("a Hub was checked: %v", r.check.calls())
		}
	})

	t.Run("a saved address with a query is shown without it", func(t *testing.T) {
		r := newSetupRig(t, true, "build")
		writeConfigFile(t, r.state, map[string]string{config.KeyServer: "https://hub.example:8443/base?token=" + canary})
		k := newKeys(t)
		k.typeLines("1", "solo")
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		if err := r.run(ctx, k.r); err != nil {
			t.Fatalf("setup: %v\n%s", err, r.stdout)
		}
		if out := r.stdout.String(); strings.Contains(out, canary) || !strings.Contains(out, "Hub address [https://hub.example:8443/base]") {
			t.Errorf("output:\n%s", out)
		}
	})
}

// The base path, the scheme check and the self-signed certificate warning with
// its fingerprint are kept: the real address check runs against a private
// httptest TLS server, and the report shows the address without anything but
// scheme, host and path.
func TestWorkerSetupRealCheckKeepsTheBasePathAndTheSelfSignedTruth(t *testing.T) {
	srv := httptest.NewTLSServer(http.NotFoundHandler())
	defer srv.Close()
	r := newSetupRig(t, false)
	r.deps.checker = nil // the real address check
	addr := srv.URL + "/base/path"
	if err := r.run(context.Background(), readFailer{t}, "--share-existing", "all", "--server", addr); err != nil {
		t.Fatalf("setup: %v\n%s", err, r.stdout)
	}
	out := r.stdout.String()
	for _, want := range []string{"Checking " + addr, "SHA-256", "WARNING", "self-signed"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if got := r.file()[config.KeyServer]; got != addr {
		t.Errorf("saved server = %q, want %q", got, addr)
	}
}
