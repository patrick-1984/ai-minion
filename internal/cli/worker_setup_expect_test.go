package cli

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/worker/agent"
	"github.com/patrick-1984/ai-minion/internal/worker/store"
)

// Without hub_fingerprint the wizard asks for it after the address was typed
// and before the address is checked, saves it with the address and pairs with
// the expectation.
func TestWorkerSetupAsksForTheHubFingerprintBeforeTheCheck(t *testing.T) {
	fp := fakeHubFingerprint(t)
	for name, answer := range map[string]string{"words": fp.Words(), "skip": "skip"} {
		t.Run(name, func(t *testing.T) {
			r := newSetupRig(t, true, "build")
			delete(r.env, "MINION_HUB_FINGERPRINT")
			k := newKeys(t)
			k.typeLines("1", "https://hub.example:8443", "", "nonsense", answer)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if err := r.run(ctx, k.r); err != nil {
				t.Fatalf("worker setup: %v\n%s", err, r.stdout)
			}
			out := r.stdout.String()
			ask, check := strings.Index(out, "NEW HUB - ENTER ITS FINGERPRINT"), strings.Index(out, "Checking https://hub.example:8443")
			if ask < 0 || check < 0 || ask > check {
				t.Fatalf("the fingerprint must be asked before the check:\n%s", out)
			}
			if !strings.Contains(out, "That is not a fingerprint") {
				t.Errorf("an invalid answer was not explained:\n%s", out)
			}
			e, ok := r.enrollment()
			if !ok {
				t.Fatal("not paired")
			}
			saved := r.file()[config.KeyHubFingerprint]
			if name == "skip" {
				if saved != "" || e.Check() != store.HubCheckNone {
					t.Fatalf("skip: saved %q, check %q", saved, e.Check())
				}
				return
			}
			if saved != fp.Words() || e.Check() != store.HubCheckFingerprint {
				t.Fatalf("saved %q, check %q", saved, e.Check())
			}
			if !r.pair.calls[0].ExpectHub.MatchesFingerprint(fp) {
				t.Fatal("the pairing did not get the expectation")
			}
		})
	}
}

// Round 2: the owner's command `worker up --foreground --share-existing all
// --server <url>` skips the wizard; on a terminal a first pairing still asks
// for the Hub fingerprint before the Worker starts (and dials), and saves it.
// Without a terminal nothing is asked; with --hub-fingerprint nothing is asked.
func TestWorkerUpWithShareAndServerAsksForTheHubFingerprint(t *testing.T) {
	fp := fakeHubFingerprint(t)
	for name, tc := range map[string]struct {
		tty    bool
		typed  string
		args   []string
		envFP  string
		asked  bool
		saved  string
		expect string
		starts bool
	}{
		"words":           {tty: true, typed: "nonsense\n" + fp.Words() + "\n", asked: true, saved: fp.Words(), expect: fp.Words(), starts: true},
		"empty flag":      {tty: true, typed: fp.Words() + "\n", args: []string{"--hub-fingerprint="}, asked: true, saved: fp.Words(), expect: fp.Words(), starts: true},
		"blank env":       {tty: true, typed: fp.Words() + "\n", envFP: " \t ", asked: true, saved: fp.Words(), expect: fp.Words(), starts: true},
		"skip":            {tty: true, typed: "skip\n", asked: true, starts: true},
		"aborted":         {tty: true, typed: "", asked: true},
		"no terminal":     {starts: true},
		"fingerprint set": {tty: true, args: []string{"--hub-fingerprint", fp.Words()}, envFP: "ignored", expect: fp.Words(), starts: true},
	} {
		t.Run(name, func(t *testing.T) {
			env := map[string]string{"MINION_STATE_DIR": t.TempDir(), "MINION_HUB_FINGERPRINT": tc.envFP}
			deps, p, rig := foregroundGate(t, tc.tty, env)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			out := &syncBuf{}
			done := make(chan error, 1)
			args := append([]string{"--foreground", "--share-existing", "all", "--server", "https://hub.example:8443"}, tc.args...)
			go func() { done <- runUpOut(deps, ctx, strings.NewReader(tc.typed), out, args...) }()
			select {
			case <-p.ready:
				if !tc.starts {
					t.Fatal("the Worker started after the prompt was aborted")
				}
				cancel()
				if err := <-done; err != nil {
					t.Errorf("worker up: %v", err)
				}
				hub, err := config.ResolveWorkerHub(p.seen().Resolver)
				if err != nil || hub.Fingerprint != tc.expect {
					t.Errorf("startup fingerprint = %q, %v; want %q", hub.Fingerprint, err, tc.expect)
				}
			case err := <-done:
				if tc.starts {
					t.Fatalf("worker up ended before the Worker started: %v\n%s", err, out)
				}
				if err == nil {
					t.Fatal("an aborted prompt must be an error")
				}
			case <-time.After(30 * time.Second):
				t.Fatal("worker up did not start")
			}
			got := out.String()
			if asked := strings.Contains(got, "NEW HUB - ENTER ITS FINGERPRINT"); asked != tc.asked {
				t.Fatalf("asked = %v, want %v:\n%s", asked, tc.asked, got)
			}
			if saved := rig.file()[config.KeyHubFingerprint]; saved != tc.saved {
				t.Fatalf("saved hub_fingerprint %q, want %q", saved, tc.saved)
			}
		})
	}
}

func TestWorkerUpUnderSudoAsksForTheHubFingerprint(t *testing.T) {
	fp := fakeHubFingerprint(t)
	f := newUpFixture(t, true)
	f.deps.setup = newSetupRig(t, true).deps
	f.env["MINION_HUB_FINGERPRINT"] = " \t "
	var effective string
	f.hook("capture", func(s agent.Settings) error {
		hub, err := config.ResolveWorkerHub(s.Resolver)
		effective = hub.Fingerprint
		return err
	})
	out, err := runUp(f.deps, context.Background(), strings.NewReader(fp.Words()+"\n"),
		"--as-user", "ann", "--share-existing", "all", "--server", "https://hub.example:8443", "--hub-fingerprint=")
	if err != nil || !strings.Contains(out, "NEW HUB - ENTER ITS FINGERPRINT") {
		t.Fatalf("worker up: %v\n%s", err, out)
	}
	if effective != fp.Words() {
		t.Errorf("pre-start fingerprint = %q, want %q", effective, fp.Words())
	}
	if got := readWorkerUpConfig(t, filepath.Join(f.stateDir(), workerConfigName))[config.KeyHubFingerprint]; got != fp.Words() {
		t.Errorf("service fingerprint = %q, want %q", got, fp.Words())
	}
}

func TestWorkerUpUnderSudoUsesTheExistingEnrollment(t *testing.T) {
	f := newUpFixture(t, true)
	f.deps.setup = newSetupRig(t, true).deps
	ctx := context.Background()
	st, err := store.OpenContext(ctx, f.stateDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	hub, err := identity.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SaveEnrollment(ctx, store.Enrollment{HubPub: hub.Public()}); err != nil {
		t.Fatal(err)
	}
	// Leave the database open so the latest enrollment can be in its WAL.
	out, err := runUp(f.deps, ctx, readFailer{t}, "--as-user", "ann", "--share-existing", "all", "--server", "https://hub.example:8443")
	if err != nil || len(f.runCalls()) == 0 || strings.Contains(out, "NEW HUB - ENTER ITS FINGERPRINT") {
		t.Fatalf("worker up: %v\n%s", err, out)
	}
}

// Round 2: the wizard opens the Worker state for writing (which migrates it)
// only when it saves; cancelled at the fingerprint question it never did.
func TestWorkerSetupCancelAtTheFingerprintOpensNoWritableStore(t *testing.T) {
	r := newSetupRig(t, true, "build")
	delete(r.env, "MINION_HUB_FINGERPRINT")
	st, err := store.OpenContext(context.Background(), r.state)
	if err != nil {
		t.Fatal(err)
	}
	_ = st.Close()
	opened := false
	r.deps.openStore = func(ctx context.Context, dir string) (setupStore, error) {
		opened = true
		return store.OpenContext(ctx, dir)
	}
	err = r.run(context.Background(), strings.NewReader("1\nhttps://hub.example:8443\n"))
	if err == nil {
		t.Fatalf("a cancelled wizard succeeded:\n%s", r.stdout)
	}
	if !strings.Contains(r.stdout.String(), "NEW HUB - ENTER ITS FINGERPRINT") {
		t.Fatalf("the fingerprint was not asked:\n%s", r.stdout)
	}
	if opened {
		t.Fatal("the wizard opened the state for writing before it saved anything")
	}
}
