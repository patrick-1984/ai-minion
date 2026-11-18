package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/worker/agent"
)

// testJoinToken is a join token for testHubFingerprint() at hub.
func testJoinToken(t *testing.T, hub string) (text string, tok identity.JoinToken) {
	t.Helper()
	k, err := identity.NewInviteKey(identity.Fingerprint(testHubFingerprint()))
	if err != nil {
		t.Fatal(err)
	}
	tok, err = identity.NewJoinToken(hub, k)
	if err != nil {
		t.Fatal(err)
	}
	return tok.Encode(), tok
}

// joinSeen is what the enrollment hook of a "worker up --join" saw.
type joinSeen struct {
	hub    config.WorkerHub
	invite config.InviteKeyInput
	warned bool
}

func joinHook(t *testing.T, f *upFixture, seen *joinSeen) {
	t.Helper()
	f.hook("enroll", func(s agent.Settings) error {
		var err error
		if seen.hub, err = config.ResolveWorkerHub(s.Resolver); err != nil {
			return err
		}
		if seen.invite, err = config.ResolveWorkerInviteKey(s.Resolver, nil); err != nil {
			return err
		}
		seen.warned = s.InviteArgvWarned
		return nil
	})
}

func TestWorkerUpJoinExpandsTheTokenAndSavesOnlyThePublicParts(t *testing.T) {
	for _, viaEnv := range []bool{false, true} {
		f := newUpFixture(t, false)
		var seen joinSeen
		joinHook(t, f, &seen)
		text, tok := testJoinToken(t, "https://hub.example:8443")
		args := []string{"--share-existing", "later"}
		if viaEnv {
			f.env["MINION_JOIN"] = text
		} else {
			args = append(args, "--join", text)
		}
		out, err := f.run(args...)
		if err != nil {
			t.Fatalf("env %v: worker up: %v\nstderr: %s", viaEnv, err, f.stderr)
		}
		if seen.hub.Server != "https://hub.example:8443" || seen.hub.Fingerprint != testHubFingerprint() {
			t.Errorf("env %v: hook saw the Hub %+v", viaEnv, seen.hub)
		}
		if !seen.invite.Set || seen.invite.Value != tok.Invite.String() || !seen.warned {
			t.Errorf("env %v: hook saw the key set=%v warned=%v", viaEnv, seen.invite.Set, seen.warned)
		}
		// The service reads the Hub address and fingerprint from the config
		// file; neither the token nor the key is ever written.
		cfg := readWorkerUpConfig(t, filepath.Join(f.stateDir(), "config.json"))
		if cfg[config.KeyServer] != "https://hub.example:8443" || cfg[config.KeyHubFingerprint] != testHubFingerprint() {
			t.Errorf("env %v: config = %v", viaEnv, cfg)
		}
		if _, ok := cfg[config.KeyInviteKey]; ok {
			t.Errorf("env %v: the invitation key was saved", viaEnv)
		}
		if _, ok := cfg[config.KeyJoin]; ok {
			t.Errorf("env %v: the token was saved", viaEnv)
		}
		raw, _ := os.ReadFile(filepath.Join(f.stateDir(), "config.json"))
		secret := tok.Invite.String()[len(tok.Invite.String())-52:]
		for name, s := range map[string]string{"stdout": out, "stderr": f.stderr.String(), "config": string(raw)} {
			if strings.Contains(s, text) || strings.Contains(s, secret) {
				t.Errorf("env %v: %s holds the secret:\n%s", viaEnv, name, s)
			}
		}
		// --join warns about the process list once; the key it carries is
		// not warned about as a literal --invite-key.
		stderr := f.stderr.String()
		if strings.Contains(stderr, "--invite-key") || strings.Contains(stderr, config.InviteKeyArgvWarning) {
			t.Errorf("env %v: warned about --invite-key: %s", viaEnv, stderr)
		}
		if got := strings.Count(stderr, "--join was given on the command line"); (viaEnv && got != 0) || (!viaEnv && got != 1) {
			t.Errorf("env %v: %d --join warnings: %s", viaEnv, got, stderr)
		}
	}
}

func TestWorkerUpJoinFromAFile(t *testing.T) {
	f := newUpFixture(t, false)
	var seen joinSeen
	joinHook(t, f, &seen)
	text, _ := testJoinToken(t, "https://hub.example")
	path := filepath.Join(t.TempDir(), "join")
	if err := os.WriteFile(path, []byte(text+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.env["MINION_JOIN_FILE"] = path
	if _, err := f.run("--share-existing", "later"); err != nil {
		t.Fatalf("worker up: %v", err)
	}
	if seen.hub.Server != "https://hub.example" || !seen.invite.Set {
		t.Errorf("hook saw %+v, key set %v", seen.hub, seen.invite.Set)
	}
}

func TestWorkerUpJoinAcceptsMatchingExplicitValues(t *testing.T) {
	f := newUpFixture(t, false)
	var seen joinSeen
	joinHook(t, f, &seen)
	text, tok := testJoinToken(t, "https://hub.example/minion")
	f.env["MINION_SERVER"] = "HTTPS://HUB.example/minion/"
	_, err := f.run("--share-existing", "later", "--join", text, "--hub-fingerprint", tok.Fingerprint().Words())
	if err != nil {
		t.Fatalf("worker up: %v", err)
	}
	if seen.hub.Fingerprint != testHubFingerprint() {
		t.Errorf("fingerprint %q", seen.hub.Fingerprint)
	}
}

func TestWorkerUpJoinConflictsNameBothSourcesAndChangeNothing(t *testing.T) {
	text, _ := testJoinToken(t, "https://hub.example")
	other, _ := identity.FingerprintOf(bytes.Repeat([]byte{7}, 32))
	keyFile := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(keyFile, []byte("mnk_x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		env  map[string]string
		args []string
		want []string
	}{
		{"server flag", nil, []string{"--join", text, "--server", "https://other.example"},
			[]string{"--join names the Hub https://hub.example", "--server names https://other.example"}},
		{"server env", map[string]string{"MINION_SERVER": "https://other.example"}, []string{"--join", text},
			[]string{"--join", "MINION_SERVER names https://other.example"}},
		{"token env, server flag", map[string]string{"MINION_JOIN": text}, []string{"--server", "https://other.example"},
			[]string{"MINION_JOIN (or MINION_JOIN_FILE) names the Hub https://hub.example", "--server"}},
		{"fingerprint flag", nil, []string{"--join", text, "--hub-fingerprint", other.String()},
			[]string{"--join names the Hub fingerprint", "--hub-fingerprint names " + other.Words()}},
		{"fingerprint env", map[string]string{"MINION_HUB_FINGERPRINT": other.Words()}, []string{"--join", text},
			[]string{"MINION_HUB_FINGERPRINT names " + other.Words()}},
		{"invite key flag", nil, []string{"--join", text, "--invite-key", keyFile},
			[]string{"--join already holds a one-time invitation key", "--invite-key", "MINION_INVITE_KEY_FILE"}},
		{"invite key env", map[string]string{"MINION_INVITE_KEY_FILE": keyFile}, []string{"--join", text},
			[]string{"already holds a one-time invitation key"}},
		{"malformed", nil, []string{"--join", "mnj1-AAAA"},
			[]string{"--join", "malformed join token", "Add worker page"}},
	}
	for _, c := range cases {
		f := newUpFixture(t, false)
		for k, v := range c.env {
			f.env[k] = v
		}
		f.hook("enroll", func(agent.Settings) error { t.Errorf("%s: the hook ran", c.name); return nil })
		_, err := f.run(append([]string{"--share-existing", "later"}, c.args...)...)
		if err == nil {
			t.Errorf("%s: accepted", c.name)
			continue
		}
		for _, w := range c.want {
			if !strings.Contains(err.Error(), w) {
				t.Errorf("%s: error lacks %q: %v", c.name, w, err)
			}
		}
		if strings.Contains(err.Error(), text) || strings.Contains(err.Error(), "mnj1-AAAA") {
			t.Errorf("%s: the error repeats the token: %v", c.name, err)
		}
		if calls := f.runCalls(); len(calls) != 0 {
			t.Errorf("%s: service commands ran: %v", c.name, calls)
		}
		if _, statErr := os.Stat(filepath.Join(f.stateDir(), "config.json")); statErr == nil {
			t.Errorf("%s: a config file was written", c.name)
		}
	}
}

func TestWorkerUpJoinIgnoresTheConfigFileAsATokenSource(t *testing.T) {
	f := newUpFixture(t, false)
	text, _ := testJoinToken(t, "https://hub.example")
	writeConfigFile(t, f.stateDir(), map[string]string{config.KeyJoin: text})
	var seen joinSeen
	joinHook(t, f, &seen)
	if _, err := f.run("--share-existing", "later"); err != nil {
		t.Fatalf("worker up: %v", err)
	}
	if seen.hub.Server != "" || seen.invite.Set {
		t.Errorf("a token in the config file was used: %+v", seen.hub)
	}
}

// The foreground path ("worker up --foreground", the container) expands the
// token once, before the first-run gate, and the hook of the Worker run sees
// it; the literal key is not warned about.
func TestWorkerUpForegroundJoin(t *testing.T) {
	state := t.TempDir()
	env := map[string]string{"MINION_STATE_DIR": state, "MINION_SHARE_EXISTING": "later", "MINION_HUB_FINGERPRINT": ""}
	deps, _, _ := foregroundGate(t, false, env)
	text, tok := testJoinToken(t, "https://hub.example")
	env["MINION_JOIN"] = text
	stop := errors.New("stop after the hook")
	var seen joinSeen
	err := deps.run.registry.RegisterPreStart("observe", func(_ context.Context, s agent.Settings) error {
		seen.hub, _ = config.ResolveWorkerHub(s.Resolver)
		seen.invite, _ = config.ResolveWorkerInviteKey(s.Resolver, nil)
		seen.warned = s.InviteArgvWarned
		return stop
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stderr := &bytes.Buffer{}
	deps.run.stderr = stderr
	_, err = runUp(deps, ctx, readFailer{t}, "--foreground")
	if !errors.Is(err, stop) {
		t.Fatalf("worker up: %v", err)
	}
	if seen.hub.Server != "https://hub.example" || seen.hub.Fingerprint != testHubFingerprint() ||
		seen.invite.Value != tok.Invite.String() || !seen.warned {
		t.Errorf("hook saw %+v, key set %v, warned %v", seen.hub, seen.invite.Set, seen.warned)
	}
	if strings.Contains(stderr.String(), "invite") {
		t.Errorf("stderr: %s", stderr)
	}
}

func TestWorkerJoinRefusesAJoinTokenWithoutEchoingIt(t *testing.T) {
	c := newPairCLI(t, nil)
	text, _ := testJoinToken(t, "https://hub.example")
	err := c.exec("join", text)
	if err == nil || !strings.Contains(err.Error(), "minion worker up --join") || strings.Contains(err.Error(), text) {
		t.Fatalf("err = %v", err)
	}
	if c.dialer.n() != 0 {
		t.Fatal("a connection for a join token")
	}
}

func TestJoinFlagOnlyOnTheCommandsThatExpandIt(t *testing.T) {
	root := NewRoot()
	for _, path := range [][]string{{"worker", "up"}, {"worker", "run"}} {
		cmd, _, err := root.Find(path)
		if err != nil || cmd.Flags().Lookup("join") == nil {
			t.Errorf("%v has no --join: %v", path, err)
		}
	}
	for _, path := range [][]string{{"worker", "pair"}, {"worker", "join"}, {"worker", "setup"}} {
		cmd, _, err := root.Find(path)
		if err != nil || cmd.Flags().Lookup("join") != nil {
			t.Errorf("%v takes --join: %v", path, err)
		}
	}
}

func TestWorkerConfigRefusesTheJoinToken(t *testing.T) {
	if _, err := workerSetting(config.Default, config.KeyJoin); err == nil || !strings.Contains(err.Error(), "never stored") {
		t.Fatalf("err = %v", err)
	}
	for _, k := range workerSettingKeys(config.Default) {
		if k == config.KeyJoin {
			t.Fatal("join is offered by worker config")
		}
	}
}

func TestSameHubURL(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"https://hub.example", "https://hub.example/", true},
		{"HTTPS://Hub.Example:8443/x", "https://hub.example:8443/x/", true},
		{"https://hub.example", "http://hub.example", false},
		{"https://hub.example/a", "https://hub.example/A", false},
		{"https://hub.example:8443", "https://hub.example", false},
	} {
		if got := sameHubURL(c.a, c.b); got != c.want {
			t.Errorf("sameHubURL(%q, %q) = %v", c.a, c.b, got)
		}
	}
}

// Review finding 1: a Hub pin provisioned in the config file beforehand is
// never replaced by a join token; a different saved address stops it too.
func TestWorkerUpJoinRespectsTheSavedHubPin(t *testing.T) {
	text, tok := testJoinToken(t, "https://hub.example")
	other, _ := identity.FingerprintOf(bytes.Repeat([]byte{7}, 32))
	cases := []struct {
		name  string
		saved map[string]string
		want  string
	}{
		{"other fingerprint", map[string]string{config.KeyHubFingerprint: other.String()}, "hub_fingerprint in the config file names " + other.Words()},
		{"other fingerprint as words", map[string]string{config.KeyHubFingerprint: other.Words()}, "hub_fingerprint in the config file"},
		{"other server", map[string]string{config.KeyServer: "https://evil.example/?token=s3cret"}, "server in the config file names https://evil.example/"},
	}
	for _, c := range cases {
		for _, foreground := range []bool{false, true} {
			f := newUpFixture(t, false)
			writeConfigFile(t, f.stateDir(), c.saved)
			before, _ := os.ReadFile(filepath.Join(f.stateDir(), "config.json"))
			f.hook("enroll", func(agent.Settings) error { t.Errorf("%s: the hook ran", c.name); return nil })
			args := []string{"--share-existing", "later", "--join", text}
			if foreground {
				// The foreground Worker reads the state directory of this process.
				f.env["MINION_STATE_DIR"] = f.stateDir()
				args = append(args, "--foreground")
			}
			_, err := f.run(args...)
			if err == nil || !strings.Contains(err.Error(), c.want) || !strings.Contains(err.Error(), "nothing was sent") {
				t.Errorf("%s (foreground %v): err = %v", c.name, foreground, err)
				continue
			}
			if strings.Contains(err.Error(), text) || strings.Contains(err.Error(), "s3cret") {
				t.Errorf("%s: the error leaks: %v", c.name, err)
			}
			if calls := f.runCalls(); len(calls) != 0 {
				t.Errorf("%s: service commands ran: %v", c.name, calls)
			}
			after, _ := os.ReadFile(filepath.Join(f.stateDir(), "config.json"))
			if !bytes.Equal(before, after) {
				t.Errorf("%s: the config file changed:\n%s", c.name, after)
			}
		}
	}
	// The same Hub saved before (words, trailing slash) is fine.
	f := newUpFixture(t, false)
	writeConfigFile(t, f.stateDir(), map[string]string{config.KeyHubFingerprint: tok.Fingerprint().Words(), config.KeyServer: "https://HUB.example/"})
	var seen joinSeen
	joinHook(t, f, &seen)
	if _, err := f.run("--share-existing", "later", "--join", text); err != nil || !seen.invite.Set {
		t.Fatalf("worker up with the same saved Hub: %v", err)
	}
}

// Review findings 2 and 3: an explicitly empty --server (Solo) conflicts
// with a token, and a blank token that was given is an error, not "none".
func TestWorkerUpJoinBlankValuesAreNotAbsent(t *testing.T) {
	text, _ := testJoinToken(t, "https://hub.example")
	blankFile := filepath.Join(t.TempDir(), "join")
	if err := os.WriteFile(blankFile, []byte("   \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		env  map[string]string
		args []string
		want string
	}{
		{"empty server", map[string]string{"MINION_JOIN": text}, []string{"--server", ""}, "--server is empty (no Hub, Solo)"},
		{"empty --join", nil, []string{"--join", ""}, "--join: malformed join token: it is empty"},
		{"blank --join", nil, []string{"--join", "  "}, "malformed join token: it is empty"},
		{"blank file", map[string]string{"MINION_JOIN_FILE": blankFile}, nil, "MINION_JOIN (or MINION_JOIN_FILE): malformed join token: it is empty"},
	}
	for _, c := range cases {
		f := newUpFixture(t, false)
		for k, v := range c.env {
			f.env[k] = v
		}
		f.hook("enroll", func(agent.Settings) error { t.Errorf("%s: the hook ran", c.name); return nil })
		_, err := f.run(append([]string{"--share-existing", "later"}, c.args...)...)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v", c.name, err)
		}
	}
}

// Review finding 4: no error repeats a token pasted in the wrong place, and
// URLs in conflict messages lose their query.
func TestJoinErrorsNeverEchoAPastedToken(t *testing.T) {
	text, _ := testJoinToken(t, "https://hub.example")
	f := newUpFixture(t, false)
	f.env["MINION_JOIN_FILE"] = text
	_, err := f.run("--share-existing", "later")
	if err == nil || strings.Contains(err.Error(), text) || !strings.Contains(err.Error(), "takes the path of a file") {
		t.Errorf("MINION_JOIN_FILE=<token>: %v", err)
	}
	_, err = newUpFixture(t, false).run(text)
	if err == nil || strings.Contains(err.Error(), text) || !strings.Contains(err.Error(), "--join <token>") {
		t.Errorf("worker up <token>: %v", err)
	}
	for _, path := range [][]string{{"worker", "run", text}, {"worker", "pair", text}, {"worker", "up", "mnk_abc.def"}} {
		var out, errOut bytes.Buffer
		Execute(context.Background(), path, &out, &errOut)
		if strings.Contains(out.String()+errOut.String(), path[2]) {
			t.Errorf("%v echoed the secret: %s", path[:2], errOut.String())
		}
	}
	f = newUpFixture(t, false)
	f.env["MINION_SERVER"] = "https://other.example/x?token=s3cret#frag"
	_, err = f.run("--share-existing", "later", "--join", text)
	if err == nil || strings.Contains(err.Error(), "s3cret") || !strings.Contains(err.Error(), "https://other.example/x") {
		t.Errorf("server conflict: %v", err)
	}
}
