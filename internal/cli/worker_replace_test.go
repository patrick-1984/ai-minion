package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/worker/agent"
)

// replaceRig is an upFixture with an old profile on disk, a fake terminal and
// recorded stop and start of the old supervisor.
type replaceRig struct {
	*upFixture
	oldFP   string
	ttyText string // what the person types; empty with noTTY
	noTTY   bool
	asked   *bytes.Buffer
	live    bool
	hubSeen joinSeen
}

type fakeTTY struct {
	io.Reader
	io.Writer
}

func (fakeTTY) Close() error { return nil }

func newReplaceRig(t *testing.T) *replaceRig {
	t.Helper()
	f := newUpFixture(t, false)
	fp, err := identity.FingerprintOf(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	r := &replaceRig{upFixture: f, oldFP: string(fp), asked: &bytes.Buffer{}, live: true}
	dir := f.stateDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadFile(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Set(config.KeyServer, "https://old.example:8443")
	cfg.Set(config.KeyHubFingerprint, r.oldFP)
	cfg.Set(config.KeyName, "old-worker")
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	if err := agent.WritePause(dir, true); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "old-marker"), []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.deps.replace = replaceDeps{
		tty: func() (io.ReadWriteCloser, error) {
			if r.noTTY {
				return nil, errors.New("no tty")
			}
			return fakeTTY{strings.NewReader(r.ttyText), r.asked}, nil
		},
		stopOld: func(_ context.Context, _ io.Writer) error {
			f.events = append(f.events, "old: stop")
			r.live = false
			return nil
		},
		startOld: func(_ context.Context, _ io.Writer) error {
			f.events = append(f.events, "old: start")
			r.live = true
			return nil
		},
		live: func(string) bool { return r.live },
	}
	return r
}

func (r *replaceRig) run(t *testing.T, args ...string) (string, error) {
	t.Helper()
	text, _ := testJoinToken(t, "https://new.example:9443")
	return r.runIn(strings.NewReader(text+"\n"), append([]string{"--join", "-", "--non-interactive-share", "all"}, args...)...)
}

func (r *replaceRig) oldIntact(t *testing.T) {
	t.Helper()
	cfg := readWorkerUpConfig(t, filepath.Join(r.stateDir(), "config.json"))
	if cfg[config.KeyServer] != "https://old.example:8443" || cfg[config.KeyHubFingerprint] != r.oldFP || cfg[config.KeyName] != "old-worker" {
		t.Errorf("the old config changed: %v", cfg)
	}
	if b, err := os.ReadFile(filepath.Join(r.stateDir(), "old-marker")); err != nil || string(b) != "old" {
		t.Errorf("old-marker: %q %v", b, err)
	}
	if p, _ := agent.ReadPause(r.stateDir()); !p {
		t.Error("the pause marker of the old profile is gone")
	}
	if _, err := os.Stat(r.stateDir() + replaceBackupSuffix); err == nil {
		t.Error("the backup copy was left behind")
	}
}

func (r *replaceRig) enrolledHook() {
	r.hook("enroll", func(s agent.Settings) error {
		var err error
		r.hubSeen.hub, err = config.ResolveWorkerHub(s.Resolver)
		return err
	})
}

func TestReplaceDeclineIsANoOpWithExitZero(t *testing.T) {
	for _, answer := range []string{"\n", "n\n", "", "maybe\n"} {
		r := newReplaceRig(t)
		r.ttyText = answer
		out, err := r.run(t, "--interactive-replace")
		if err != nil {
			t.Fatalf("answer %q: %v", answer, err)
		}
		for _, want := range []string{"already set up", "ann", r.stateDir(), "old-worker", "https://old.example:8443", "paused"} {
			if !strings.Contains(out, want) {
				t.Errorf("answer %q: output lacks %q:\n%s", answer, want, out)
			}
		}
		if !strings.Contains(r.asked.String(), "Replace it with https://new.example:9443") || !strings.Contains(r.asked.String(), "[y/N]") {
			t.Errorf("question = %q", r.asked.String())
		}
		if !strings.Contains(out, "Nothing was changed") {
			t.Errorf("no message:\n%s", out)
		}
		r.oldIntact(t)
		if len(r.runCalls()) != 0 || strings.Contains(strings.Join(r.events, ","), "old:") || r.hubSeen.hub.Server != "" {
			t.Errorf("answer %q changed something: %v", answer, r.events)
		}
	}
}

func TestReplaceKeepIsANoOp(t *testing.T) {
	r := newReplaceRig(t)
	r.noTTY = true
	out, err := r.run(t, "--keep")
	if err != nil || !strings.Contains(out, "Nothing was changed") {
		t.Fatalf("err %v out %s", err, out)
	}
	r.oldIntact(t)
	if r.asked.Len() != 0 || len(r.events) != 0 {
		t.Errorf("--keep asked or acted: %q %v", r.asked, r.events)
	}
}

func TestReplaceWithoutTerminalAndFlagRefusesNamingTheFlags(t *testing.T) {
	r := newReplaceRig(t)
	r.noTTY = true
	_, err := r.run(t, "--interactive-replace")
	if err == nil || !strings.Contains(err.Error(), "--replace") || !strings.Contains(err.Error(), "--keep") {
		t.Fatalf("err = %v", err)
	}
	r.oldIntact(t)
	if len(r.events) != 0 {
		t.Errorf("events: %v", r.events)
	}
}

func TestReplaceYesJoinsWithAClearedProfile(t *testing.T) {
	for name, args := range map[string][]string{"prompt": {"--interactive-replace"}, "flag": {"--replace"}} {
		r := newReplaceRig(t)
		r.ttyText = "y\n"
		r.enrolledHook()
		out, err := r.run(t, args...)
		if err != nil {
			t.Fatalf("%s: %v\n%s", name, err, out)
		}
		if r.hubSeen.hub.Server != "https://new.example:9443" || r.hubSeen.hub.Fingerprint != testHubFingerprint() {
			t.Errorf("%s: the hook saw %+v", name, r.hubSeen.hub)
		}
		cfg := readWorkerUpConfig(t, filepath.Join(r.stateDir(), "config.json"))
		if cfg[config.KeyServer] != "https://new.example:9443" || cfg[config.KeyHubFingerprint] != testHubFingerprint() {
			t.Errorf("%s: config = %v", name, cfg)
		}
		if p, _ := agent.ReadPause(r.stateDir()); p {
			t.Errorf("%s: the pause marker survived", name)
		}
		if _, err := os.Stat(r.stateDir() + replaceBackupSuffix); err == nil {
			t.Errorf("%s: backup left after a commit", name)
		}
		if !strings.Contains(strings.Join(r.events, ","), "old: stop") || strings.Contains(strings.Join(r.events, ","), "old: start") {
			t.Errorf("%s: events %v", name, r.events)
		}
	}
}

func TestReplaceFailureRestoresTheOldProfileAndRestartsIt(t *testing.T) {
	cases := map[string]func(r *replaceRig){
		"enrollment fails": func(r *replaceRig) {
			r.hook("enroll", func(agent.Settings) error { return errors.New("the invitation was refused") })
		},
		"worker never comes up": func(r *replaceRig) {
			r.enrolledHook()
			r.deps.readStatus = func(context.Context, string, *ownerSetup) (agent.StatusReport, error) {
				return agent.StatusReport{}, errors.New("no answer")
			}
		},
	}
	for name, setup := range cases {
		r := newReplaceRig(t)
		setup(r)
		_, err := r.run(t, "--replace")
		if err == nil || !strings.Contains(err.Error(), "old Worker was kept") {
			t.Fatalf("%s: err = %v", name, err)
		}
		r.oldIntact(t)
		ev := strings.Join(r.events, ",")
		if !strings.Contains(ev, "old: stop") || !strings.Contains(ev, "old: start") {
			t.Errorf("%s: the old supervisor was not stopped and started again: %v", name, r.events)
		}
		if !r.live {
			t.Errorf("%s: the old Worker is not running again", name)
		}
	}
}

func TestReplaceFlagsNeedAJoinToken(t *testing.T) {
	r := newReplaceRig(t)
	_, err := r.runIn(strings.NewReader(""), "--replace")
	if err == nil || !strings.Contains(err.Error(), "--join") {
		t.Fatalf("err = %v", err)
	}
	r.oldIntact(t)
}

// Without any of the three flags, an existing profile is handled exactly as
// before (the tray keeps its behaviour).
func TestJoinWithoutReplaceFlagsKeepsTheOldBehaviour(t *testing.T) {
	r := newReplaceRig(t)
	r.enrolledHook()
	_, err := r.run(t)
	if err == nil {
		t.Fatal("a join over a pinned Hub must still be refused without the flags")
	}
	if r.asked.Len() != 0 || strings.Contains(strings.Join(r.events, ","), "old:") {
		t.Errorf("the replace flow ran: %v", r.events)
	}
}

func TestReplaceOnAFreshMachineJustJoins(t *testing.T) {
	f := newUpFixture(t, false)
	var seen joinSeen
	joinHook(t, f, &seen)
	f.deps.replace.tty = func() (io.ReadWriteCloser, error) { t.Fatal("asked on a fresh machine"); return nil, nil }
	text, _ := testJoinToken(t, "https://new.example:9443")
	if _, err := f.runIn(strings.NewReader(text+"\n"), "--join", "-", "--interactive-replace", "--non-interactive-share", "all"); err != nil {
		t.Fatal(err)
	}
	if seen.hub.Server != "https://new.example:9443" {
		t.Errorf("hook saw %+v", seen.hub)
	}
}
