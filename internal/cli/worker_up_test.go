package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/worker/agent"
	"github.com/patrick-1984/ai-minion/internal/worker/service"
)

// upRunner records the commands of the service installation, shares its event
// log with the test hooks and answers like a healthy systemd.
type upRunner struct {
	mu     sync.Mutex
	events *[]string
	answer func(line string) (string, error)
}

func (r *upRunner) Run(_ context.Context, name string, args ...string) (string, error) {
	line := strings.TrimSpace(name + " " + strings.Join(args, " "))
	r.mu.Lock()
	*r.events = append(*r.events, "run: "+line)
	r.mu.Unlock()
	if r.answer != nil {
		return r.answer(line)
	}
	switch {
	case strings.Contains(line, "is-active"):
		return "active\n", nil
	case strings.HasPrefix(line, "loginctl show-user"):
		return "Linger=yes\n", nil
	case strings.HasPrefix(line, "launchctl print"):
		return "state = running\n", nil
	}
	return "", nil
}

// upFixture is a "worker up" with every effect replaced.
type upFixture struct {
	t        *testing.T
	deps     workerUpDeps
	home     string
	events   []string
	runner   *upRunner
	chowned  []string
	env      map[string]string
	stderr   *bytes.Buffer
	registry *agent.Registry
}

// newUpFixture builds a Linux host with systemd. root selects "sudo" (the
// command runs as root for the account ann) or a plain login of ann.
func newUpFixture(t *testing.T, root bool) *upFixture {
	t.Helper()
	f := &upFixture{t: t, home: t.TempDir(), env: map[string]string{}, stderr: &bytes.Buffer{}, registry: agent.NewRegistry()}
	euid := 1000
	tenv := map[string]string{}
	if root {
		euid = 0
		tenv["SUDO_USER"] = "ann"
	}
	ann := &user.User{Username: "ann", Uid: "1000", Gid: "1001", HomeDir: f.home}
	rootU := &user.User{Username: "root", Uid: "0", Gid: "0", HomeDir: "/root"}
	cur := ann
	if root {
		cur = rootU
	}
	f.runner = &upRunner{events: &f.events}
	sdir := t.TempDir()
	f.deps = workerUpDeps{
		run: workerRunDeps{stderr: f.stderr, lookupEnv: func(k string) (string, bool) { v, ok := f.env[k]; return v, ok },
			loadConfig: loadWorkerConfig, registry: f.registry},
		detectEnv: service.Env{GOOS: "linux", EUID: euid,
			Getenv: func(string) string { return "" },
			Exists: func(p string) bool { return p == "/run/systemd/system" }},
		targetEnv: service.TargetEnv{EUID: euid, Getenv: func(k string) string { return tenv[k] },
			Lookup: func(n string) (*user.User, error) {
				if n == "ann" {
					return ann, nil
				}
				return nil, errors.New("unknown user")
			},
			Current: func() (*user.User, error) { return cur, nil }},
		executable: func() (string, error) { return "/usr/local/bin/minion", nil },
		service: service.Deps{
			Runner:           f.runner,
			SystemUnitDir:    filepath.Join(sdir, "etc"),
			PackagedUnitDirs: []string{filepath.Join(sdir, "lib")},
			Getenv:           func(k string) string { return map[string]string{"XDG_RUNTIME_DIR": "/run/user/1000"}[k] },
			Sleep:            func(context.Context, time.Duration) {},
			ActivePolls:      2,
		},
		chown: &upChowner{f: f},
		// These tests are about the install, not the first-run wizard.
		setup: workerSetupDeps{assumeSetUp: true},
	}
	return f
}

// upChowner records the full path of what is given to ann and changes nothing.
type upChowner struct{ f *upFixture }

func (c *upChowner) record(p string, uid, gid int) {
	if uid != 1000 || gid != 1001 {
		c.f.t.Errorf("chown %s to %d:%d", p, uid, gid)
	}
	c.f.chowned = append(c.f.chowned, p)
}

func (c *upChowner) ChownDir(d *os.File, uid, gid int) error {
	c.record(d.Name(), uid, gid)
	return nil
}

func (c *upChowner) Lchown(r *os.Root, name string, uid, gid int) error {
	c.record(filepath.Join(r.Name(), name), uid, gid)
	return nil
}

func (f *upFixture) hook(name string, fn func(agent.Settings) error) {
	f.t.Helper()
	err := f.registry.RegisterPreStart(name, func(_ context.Context, s agent.Settings) error {
		f.events = append(f.events, "hook: "+name)
		return fn(s)
	})
	if err != nil {
		f.t.Fatal(err)
	}
}

func (f *upFixture) stateDir() string { return filepath.Join(f.home, ".local", "state", "minion") }

func (f *upFixture) run(args ...string) (stdout string, err error) {
	f.t.Helper()
	root := &cobra.Command{Use: "minion", SilenceUsage: true, SilenceErrors: true}
	workerCommand(root).AddCommand(newWorkerUpCmd(f.deps))
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(f.stderr)
	root.SetArgs(append([]string{"worker", "up"}, args...))
	err = root.ExecuteContext(context.Background())
	return out.String(), err
}

func (f *upFixture) runCalls() []string {
	var out []string
	for _, e := range f.events {
		if strings.HasPrefix(e, "run: ") {
			out = append(out, strings.TrimPrefix(e, "run: "))
		}
	}
	return out
}

func readWorkerUpConfig(t *testing.T, path string) map[string]string {
	t.Helper()
	f, err := config.LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, k := range f.Keys() {
		v, _, _ := f.Lookup(k)
		out[k] = v
	}
	return out
}

func TestWorkerUpIsRegisteredWithEveryWorkerSetting(t *testing.T) {
	cmd, _, err := NewRoot().Find([]string{"worker", "up"})
	if err != nil || cmd.Name() != "up" {
		t.Fatalf("worker up not found: %v", err)
	}
	n := 0
	for _, s := range config.Default.ForScope(config.ScopeWorker) {
		if !s.HasFlag() {
			continue
		}
		n++
		if cmd.Flags().Lookup(s.FlagName()) == nil {
			t.Errorf("worker up has no flag --%s for %s", s.FlagName(), s.Key)
		}
	}
	if n == 0 {
		t.Fatal("no setting with a flag")
	}
	for _, name := range []string{"foreground", "as-user"} {
		if cmd.Flags().Lookup(name) == nil {
			t.Errorf("no flag --%s", name)
		}
	}
	for _, want := range []string{"MINION_SERVER", "MINION_INVITE_KEY_FILE", "MINION_SHARE_EXISTING", "MINION_STATE_DIR", "/data"} {
		if !strings.Contains(cmd.Long, want) {
			t.Errorf("the help does not document %s", want)
		}
	}
	if code := Execute(context.Background(), []string{"worker", "up", "stray"}, &bytes.Buffer{}, &bytes.Buffer{}); code != 1 {
		t.Errorf("exit code = %d for a stray argument", code)
	}
}

func TestWorkerUpSystemServiceUnderSudo(t *testing.T) {
	f := newUpFixture(t, true)
	var seen agent.Settings
	f.hook("enroll", func(s agent.Settings) error { seen = s; return nil })

	out, err := f.run("--name", "boxy", "--share-existing", "all", "--preview-only")
	if err != nil {
		t.Fatalf("worker up: %v\nstderr: %s", err, f.stderr)
	}

	// The settings are the ones of ann, not of root.
	if seen.StateDir != f.stateDir() {
		t.Errorf("hook state dir = %q, want %q", seen.StateDir, f.stateDir())
	}
	if want := "/tmp/tmux-1000/default"; seen.TmuxSocket != want {
		t.Errorf("hook tmux socket = %q, want %q", seen.TmuxSocket, want)
	}
	if seen.Name != "boxy" || seen.ShareExisting != "all" || !seen.PreviewOnly {
		t.Errorf("hook settings = %+v", seen.WorkerCore)
	}

	// Hooks run first and are reported; the service follows.
	if len(f.events) < 2 || f.events[0] != "hook: enroll" || !strings.HasPrefix(f.events[1], "run: systemctl") {
		t.Fatalf("events = %v", f.events)
	}
	if !strings.Contains(out, "Pre-start hooks completed: enroll") {
		t.Errorf("hooks not reported: %q", out)
	}
	want := []string{
		"systemctl daemon-reload",
		"systemctl enable minion-worker@ann.service",
		"systemctl restart minion-worker@ann.service",
		"systemctl is-active minion-worker@ann.service",
		"systemctl is-active minion-worker@ann.service",
	}
	if got := f.runCalls(); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("commands = %v", got)
	}
	unit, err := os.ReadFile(filepath.Join(f.deps.service.SystemUnitDir, service.TemplateUnitName))
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{"User=%i", "Restart=always", "ExecStart=" + service.DefaultBinary + " worker run"} {
		if !strings.Contains(string(unit), w) {
			t.Errorf("unit lacks %q", w)
		}
	}
	// The binary of this install is in the drop-in of the instance only.
	drop, err := os.ReadFile(filepath.Join(f.deps.service.SystemUnitDir, "minion-worker@ann.service.d", "10-minion-worker-up.conf"))
	if err != nil || !strings.Contains(string(drop), "ExecStart=/usr/local/bin/minion worker run") {
		t.Errorf("drop-in: %v\n%s", err, drop)
	}

	// The flags are in the config file of ann, owned by ann, mode 0600.
	cfgPath := filepath.Join(f.stateDir(), "config.json")
	got := readWorkerUpConfig(t, cfgPath)
	wantCfg := map[string]string{"name": "boxy", "share_existing": "all", "preview_only": "true"}
	if len(got) != len(wantCfg) {
		t.Errorf("config = %v, want %v", got, wantCfg)
	}
	for k, v := range wantCfg {
		if got[k] != v {
			t.Errorf("config[%s] = %q, want %q", k, got[k], v)
		}
	}
	if st, err := os.Stat(cfgPath); err != nil || st.Mode().Perm() != 0o600 {
		t.Errorf("config mode: %v %v", st, err)
	}
	if len(f.chowned) == 0 || f.chowned[len(f.chowned)-1] == "" {
		t.Errorf("nothing was given to ann: %v", f.chowned)
	}
	for _, p := range f.chowned {
		if !strings.HasPrefix(p, f.home) {
			t.Errorf("chown outside the home of ann: %s", p)
		}
	}
	if !strings.Contains(out, "minion-worker@ann.service") || !strings.Contains(out, "Settings saved") {
		t.Errorf("output: %q", out)
	}
}

func TestWorkerUpFailedHookAbortsBeforeAnyServiceChange(t *testing.T) {
	f := newUpFixture(t, true)
	f.hook("enroll", func(agent.Settings) error { return errors.New("hub refused the invitation") })

	_, err := f.run("--name", "boxy")
	if err == nil || !strings.Contains(err.Error(), "enroll") || !strings.Contains(err.Error(), "hub refused") ||
		!strings.Contains(err.Error(), "no service was installed") {
		t.Fatalf("err = %v", err)
	}
	if calls := f.runCalls(); len(calls) != 0 {
		t.Errorf("service commands ran: %v", calls)
	}
	if _, err := os.Stat(filepath.Join(f.deps.service.SystemUnitDir, service.TemplateUnitName)); err == nil {
		t.Error("a unit was installed")
	}
	if _, err := os.Stat(filepath.Join(f.stateDir(), "config.json")); err == nil {
		t.Error("the config file was written")
	}
}

func TestWorkerUpStopsAtTheFirstFailingHook(t *testing.T) {
	f := newUpFixture(t, false)
	f.hook("first", func(agent.Settings) error { return nil })
	f.hook("second", func(agent.Settings) error { return errors.New("boom") })
	f.hook("third", func(agent.Settings) error { return nil })
	_, err := f.run()
	if err == nil || !strings.Contains(err.Error(), `"second"`) {
		t.Fatalf("err = %v", err)
	}
	if strings.Join(f.events, "|") != "hook: first|hook: second" {
		t.Errorf("events = %v", f.events)
	}
}

func TestWorkerUpWithoutHooksSaysSo(t *testing.T) {
	f := newUpFixture(t, false)
	out, err := f.run()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "No pre-start hooks") {
		t.Errorf("output: %q", out)
	}
}

func TestWorkerUpUserServiceWithoutSudo(t *testing.T) {
	f := newUpFixture(t, false)
	out, err := f.run("--name", "laptop")
	if err != nil {
		t.Fatalf("%v\n%s", err, f.stderr)
	}
	calls := f.runCalls()
	for _, w := range []string{"systemctl --user daemon-reload", "systemctl --user enable minion-worker.service", "systemctl --user restart minion-worker.service"} {
		found := false
		for _, c := range calls {
			found = found || c == w
		}
		if !found {
			t.Errorf("missing %q in %v", w, calls)
		}
	}
	if _, err := os.Stat(filepath.Join(f.home, ".config", "systemd", "user", "minion-worker.service")); err != nil {
		t.Errorf("user unit: %v", err)
	}
	if len(f.chowned) != 0 {
		t.Errorf("a plain user needs no ownership changes: %v", f.chowned)
	}
	if got := readWorkerUpConfig(t, filepath.Join(f.stateDir(), "config.json")); got["name"] != "laptop" {
		t.Errorf("config = %v", got)
	}
	if !strings.Contains(out, "user service") {
		t.Errorf("output: %q", out)
	}
}

func TestWorkerUpUserServiceLingerMissingPrintsTheCommandAndFails(t *testing.T) {
	f := newUpFixture(t, false)
	f.runner.answer = func(line string) (string, error) {
		switch {
		case strings.Contains(line, "is-active"):
			return "active\n", nil
		case strings.HasPrefix(line, "loginctl show-user"):
			return "Linger=no\n", nil
		case strings.HasPrefix(line, "loginctl enable-linger"):
			return "Access denied", errors.New("exit status 1")
		}
		return "", nil
	}
	out, err := f.run()
	if err == nil {
		t.Fatal("exit status 0 although lingering is off")
	}
	if strings.Contains(out, "starts at boot") || strings.Contains(out, "and restarts by itself") {
		t.Errorf("the output claims the Worker starts at boot although lingering is off: %q", out)
	}
	const cmd = "sudo loginctl enable-linger ann"
	if !strings.Contains(err.Error(), cmd) || !strings.Contains(f.stderr.String(), cmd) {
		t.Errorf("the exact command is not printed.\nerr: %v\nstderr: %s", err, f.stderr)
	}
	attempted := false
	for _, c := range f.runCalls() {
		attempted = attempted || c == "loginctl enable-linger ann"
	}
	if !attempted {
		t.Error("enabling linger was not attempted")
	}
}

func TestWorkerUpDoesNotWriteSecretsAndWarnsAboutArgv(t *testing.T) {
	const secret = "mnk_SECRETVALUE.abcdef"
	f := newUpFixture(t, false)
	reg := config.NewRegistry()
	for _, s := range config.Default.ForScope(config.ScopeWorker) {
		reg.Declare(s)
	}
	f.deps.settings = reg
	var sawKey string
	f.hook("enroll", func(s agent.Settings) error {
		r, err := s.Resolver.Resolve("invite_key")
		sawKey = r.Value
		return err
	})

	out, err := f.run("--invite-key", secret, "--server", "https://hub.example:8443")
	if err != nil {
		t.Fatal(err)
	}
	if sawKey != secret {
		t.Errorf("the hook did not get the invitation key: %q", sawKey)
	}
	if !strings.Contains(f.stderr.String(), "--invite-key was given on the command line") {
		t.Errorf("no process-list warning: %q", f.stderr)
	}
	raw, err := os.ReadFile(filepath.Join(f.stateDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{string(raw), out, f.stderr.String(), strings.Join(f.events, "\n")} {
		if strings.Contains(text, "SECRETVALUE") {
			t.Fatalf("the secret leaked: %s", text)
		}
	}
	if got := readWorkerUpConfig(t, filepath.Join(f.stateDir(), "config.json")); got["server"] != "https://hub.example:8443" || got["invite_key"] != "" {
		t.Errorf("config = %v", got)
	}
}

func TestWorkerUpSavesEnvironmentSettingsAndKeepsTheRest(t *testing.T) {
	f := newUpFixture(t, false)
	f.env["MINION_NAME"] = "from-env"
	if err := os.MkdirAll(f.stateDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	existing, err := config.LoadFile(filepath.Join(f.stateDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	existing.Set("preview_only", "true")
	existing.Set("name", "old")
	if err := existing.Save(); err != nil {
		t.Fatal(err)
	}
	if _, err := f.run(); err != nil {
		t.Fatal(err)
	}
	got := readWorkerUpConfig(t, filepath.Join(f.stateDir(), "config.json"))
	if got["name"] != "from-env" {
		t.Errorf("the environment setting was not saved: %v", got)
	}
	if got["preview_only"] != "true" {
		t.Errorf("an existing setting was dropped: %v", got)
	}
}

func TestWorkerUpServiceUsesACustomStateDirAndPinsItInTheService(t *testing.T) {
	f := newUpFixture(t, true)
	custom := filepath.Join(f.home, "srv", "minion")
	if _, err := f.run("--state-dir", custom, "--name", "boxy"); err != nil {
		t.Fatalf("%v\n%s", err, f.stderr)
	}
	if got := readWorkerUpConfig(t, filepath.Join(custom, "config.json")); got["name"] != "boxy" {
		t.Errorf("config in the custom dir = %v", got)
	}
	if _, err := os.Stat(filepath.Join(f.stateDir(), "config.json")); err == nil {
		t.Error("the default state dir was written although --state-dir was given")
	}
	drop, err := os.ReadFile(filepath.Join(f.deps.service.SystemUnitDir, "minion-worker@ann.service.d", "10-minion-worker-up.conf"))
	if err != nil || !strings.Contains(string(drop), `Environment="MINION_STATE_DIR=`+custom+`"`) {
		t.Fatalf("the service does not read the custom dir: %v\n%s", err, drop)
	}
	owned := false
	for _, p := range f.chowned {
		owned = owned || p == filepath.Join(custom, "config.json")
	}
	if !owned {
		t.Errorf("the custom state dir was not given to ann: %v", f.chowned)
	}
	// The default state directory spelled out needs no pin.
	f = newUpFixture(t, false)
	if _, err := f.run("--state-dir", f.stateDir()); err != nil {
		t.Fatalf("the default state dir was refused: %v", err)
	}
	unit, err := os.ReadFile(filepath.Join(f.home, ".config", "systemd", "user", "minion-worker.service"))
	if err != nil || strings.Contains(string(unit), "MINION_STATE_DIR") {
		t.Errorf("the default state dir was pinned: %v\n%s", err, unit)
	}
}

// pinnedStateDir returns the MINION_STATE_DIR of a generated unit, or "".
func pinnedStateDir(unit string) string {
	for _, line := range strings.Split(unit, "\n") {
		if v, ok := strings.CutPrefix(line, `Environment="MINION_STATE_DIR=`); ok {
			return strings.TrimSuffix(v, `"`)
		}
	}
	return ""
}

// A user unit does not get the shell's XDG_STATE_HOME. The directory that
// "worker up" saved to must be the one the unit makes the service read.
func TestWorkerUpUserServiceReadsTheDirectoryWorkerUpSavedTo(t *testing.T) {
	f := newUpFixture(t, false)
	xdg := t.TempDir()
	f.env["XDG_STATE_HOME"] = xdg
	if _, err := f.run("--name", "xdg-box"); err != nil {
		t.Fatalf("%v\n%s", err, f.stderr)
	}
	savedDir := filepath.Join(xdg, "minion")
	if got := readWorkerUpConfig(t, filepath.Join(savedDir, "config.json")); got["name"] != "xdg-box" {
		t.Fatalf("config in %s = %v", savedDir, got)
	}
	unit, err := os.ReadFile(filepath.Join(f.home, ".config", "systemd", "user", "minion-worker.service"))
	if err != nil {
		t.Fatal(err)
	}
	if got := pinnedStateDir(string(unit)); got != savedDir {
		t.Fatalf("the unit pins %q, worker up saved to %q:\n%s", got, savedDir, unit)
	}
	// "worker run" as the service manager starts it: only the unit's variable.
	svcEnv := map[string]string{"MINION_STATE_DIR": pinnedStateDir(string(unit))}
	res := &config.Resolver{Role: config.ScopeWorker, LookupEnv: func(k string) (string, bool) { v, ok := svcEnv[k]; return v, ok }}
	r, err := res.Resolve(config.KeyStateDir)
	if err != nil || r.Value != savedDir {
		t.Errorf("the service resolves %q (%v), want %q", r.Value, err, savedDir)
	}

	// Without XDG_STATE_HOME the default is the same for the service: no pin.
	f = newUpFixture(t, false)
	if _, err := f.run(); err != nil {
		t.Fatal(err)
	}
	unit, _ = os.ReadFile(filepath.Join(f.home, ".config", "systemd", "user", "minion-worker.service"))
	if got := pinnedStateDir(string(unit)); got != "" {
		t.Errorf("pinned %q without need", got)
	}
}

func TestWorkerUpFailedHookStillGivesItsFilesToTheTargetUser(t *testing.T) {
	f := newUpFixture(t, true)
	var key string
	f.hook("enroll", func(s agent.Settings) error {
		key = filepath.Join(s.StateDir, "identity.key")
		if err := os.WriteFile(key, []byte("k"), 0o600); err != nil {
			return err
		}
		return errors.New("invitation expired")
	})
	if _, err := f.run(); err == nil || !strings.Contains(err.Error(), "invitation expired") {
		t.Fatalf("err = %v", err)
	}
	chowned := false
	for _, p := range f.chowned {
		chowned = chowned || p == key
	}
	if key == "" || !chowned {
		t.Errorf("%q written by the failed hook was not given to ann: %v", key, f.chowned)
	}
	if len(f.runCalls()) != 0 {
		t.Errorf("the service was touched: %v", f.runCalls())
	}
}

func TestWorkerUpUnderSudoDoesNotFollowALinkedConfigFile(t *testing.T) {
	f := newUpFixture(t, true)
	secret := filepath.Join(t.TempDir(), "other-account.json")
	if err := os.WriteFile(secret, []byte(`{"invite_key":"OTHER-ACCOUNT-SECRET"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(f.stateDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(f.stateDir(), "config.json")
	if err := os.Symlink(secret, cfg); err != nil {
		t.Fatal(err)
	}
	_, err := f.run("--name", "x")
	if err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("err = %v", err)
	}
	if st, err := os.Lstat(cfg); err != nil || st.Mode()&os.ModeSymlink == 0 {
		t.Errorf("the link was replaced by a copy: %v %v", st, err)
	}
	if len(f.chowned) != 0 || len(f.runCalls()) != 0 || len(f.events) != 0 {
		t.Errorf("work was done: chowned %v events %v", f.chowned, f.events)
	}
}

func TestWorkerUpUnderSudoDoesNotFollowALinkedStateDirectoryAncestor(t *testing.T) {
	f := newUpFixture(t, true)
	other := t.TempDir()
	if err := os.Symlink(other, filepath.Join(f.home, ".local")); err != nil {
		t.Fatal(err)
	}
	_, err := f.run()
	if err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("err = %v", err)
	}
	if entries, _ := os.ReadDir(other); len(entries) != 0 {
		t.Errorf("something was created through the link: %v", entries)
	}
	if len(f.chowned) != 0 || len(f.events) != 0 {
		t.Errorf("work was done: chowned %v events %v", f.chowned, f.events)
	}
}

func TestWorkerUpSavesTheTmuxSocketAsAnAbsolutePath(t *testing.T) {
	f := newUpFixture(t, false)
	var hookSocket string
	f.hook("enroll", func(s agent.Settings) error { hookSocket = s.TmuxSocket; return nil })
	if _, err := f.run("--tmux-socket", "rel/worker.sock"); err != nil {
		t.Fatal(err)
	}
	abs, err := filepath.Abs("rel/worker.sock")
	if err != nil {
		t.Fatal(err)
	}
	if hookSocket != abs {
		t.Errorf("hook socket = %q, want %q", hookSocket, abs)
	}
	if got := readWorkerUpConfig(t, filepath.Join(f.stateDir(), "config.json")); got["tmux_socket"] != abs {
		t.Errorf("saved tmux_socket = %q, want the absolute %q (the service starts in the home directory)", got["tmux_socket"], abs)
	}
}

func TestWorkerUpHooksSeeTheSavedTmuxSocketNotASyntheticDefault(t *testing.T) {
	f := newUpFixture(t, true)
	if err := os.MkdirAll(f.stateDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	saved, err := config.LoadFile(filepath.Join(f.stateDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	saved.Set("tmux_socket", "/srv/tmux/ann.sock")
	if err := saved.Save(); err != nil {
		t.Fatal(err)
	}
	var seen string
	f.hook("enroll", func(s agent.Settings) error { seen = s.TmuxSocket; return nil })
	if _, err := f.run(); err != nil {
		t.Fatal(err)
	}
	if seen != "/srv/tmux/ann.sock" {
		t.Errorf("the hook got socket %q, but the service will use the saved one", seen)
	}
}

func TestWorkerUpAnEmptyStateDirVariableMeansTheTargetsDefault(t *testing.T) {
	f := newUpFixture(t, true)
	f.env["MINION_STATE_DIR"] = ""
	var seen string
	f.hook("enroll", func(s agent.Settings) error { seen = s.StateDir; return nil })
	if _, err := f.run(); err != nil {
		t.Fatal(err)
	}
	if seen != f.stateDir() {
		t.Errorf("the hook state dir = %q, want the target's %q (not root's)", seen, f.stateDir())
	}
}

func TestWorkerUpRootWithoutSudoNeedsAsUser(t *testing.T) {
	f := newUpFixture(t, true)
	f.deps.targetEnv.Getenv = func(string) string { return "" } // no SUDO_USER
	f.deps.targetEnv.Current = func() (*user.User, error) {
		return &user.User{Username: "root", Uid: "0", Gid: "0", HomeDir: "/root"}, nil
	}
	_, err := f.run()
	if err == nil || !strings.Contains(err.Error(), "--as-user") {
		t.Fatalf("err = %v", err)
	}
	if len(f.runCalls()) != 0 {
		t.Error("the service was touched")
	}
	// Naming the user makes it work.
	if _, err := f.run("--as-user", "ann"); err != nil {
		t.Fatalf("--as-user ann: %v", err)
	}
}

func TestWorkerUpLaunchdAgentOnMacOS(t *testing.T) {
	f := newUpFixture(t, false)
	f.deps.detectEnv = service.Env{GOOS: "darwin", EUID: 501}
	f.deps.targetEnv.EUID = 501
	if _, err := f.run("--name", "mac"); err != nil {
		t.Fatalf("%v\n%s", err, f.stderr)
	}
	plist := service.PlistPath(f.home)
	data, err := os.ReadFile(plist)
	if err != nil || !strings.Contains(string(data), "<string>/usr/local/bin/minion</string>") {
		t.Fatalf("plist: %v\n%s", err, data)
	}
	calls := strings.Join(f.runCalls(), "\n")
	for _, w := range []string{"launchctl bootstrap gui/1000 " + plist, "launchctl kickstart -k gui/1000/" + service.LaunchdLabel} {
		if !strings.Contains(calls, w) {
			t.Errorf("missing %q in:\n%s", w, calls)
		}
	}
}

// startUp runs "worker up" with args in the background until the probe module
// started.
func startUp(t *testing.T, deps workerUpDeps, p *probe, args ...string) (cancel func(), wait func() error, out *bytes.Buffer) {
	t.Helper()
	root := &cobra.Command{Use: "minion", SilenceUsage: true, SilenceErrors: true}
	workerCommand(root).AddCommand(newWorkerUpCmd(deps))
	out = &bytes.Buffer{}
	root.SetOut(out)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs(append([]string{"worker", "up"}, args...))
	ctx, cancelCtx := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- root.ExecuteContext(ctx) }()
	t.Cleanup(cancelCtx)
	select {
	case <-p.ready:
	case err := <-done:
		t.Fatalf("worker up ended before the probe started: %v", err)
	case <-time.After(30 * time.Second):
		t.Fatal("worker up did not start")
	}
	return cancelCtx, func() error {
		select {
		case err := <-done:
			return err
		case <-time.After(15 * time.Second):
			t.Fatal("worker up did not stop")
			return nil
		}
	}, out
}

// foregroundFixture returns deps whose registry holds a hook and the probe
// module, and a service runner that fails the test when it is touched.
func foregroundFixture(t *testing.T, env map[string]string) (workerUpDeps, *probe, *[]string) {
	t.Helper()
	runDeps, p := probeOnlyDeps(t, env)
	events := &[]string{}
	if err := runDeps.registry.RegisterPreStart("enroll", func(context.Context, agent.Settings) error {
		*events = append(*events, "hook")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	deps := workerUpDeps{
		run:       runDeps,
		detectEnv: service.Env{GOOS: "linux", EUID: 1000, Exists: func(p string) bool { return p == "/run/systemd/system" }},
		targetEnv: service.SystemTargetEnv(),
		service:   service.Deps{Runner: failRunner{t}},
		setup:     workerSetupDeps{assumeSetUp: true},
	}
	return deps, p, events
}

type failRunner struct{ t *testing.T }

func (f failRunner) Run(_ context.Context, name string, args ...string) (string, error) {
	f.t.Errorf("foreground mode ran %s %v", name, args)
	return "", errors.New("must not run")
}

func TestWorkerUpForegroundFlagRunsTheWorkerWithoutAService(t *testing.T) {
	env := map[string]string{}
	deps, p, events := foregroundFixture(t, env)
	cancel, wait, _ := startUp(t, deps, p, "--foreground", "--name", "ctr", "--share-existing", "later")
	if got := p.seen(); got.Name != "ctr" || got.ShareExisting != "later" {
		t.Errorf("module settings = %+v", got.WorkerCore)
	}
	if len(*events) != 1 {
		t.Errorf("hooks = %v", *events)
	}
	cancel()
	if err := wait(); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(p.log(), ","); got != "probe start,probe stop" {
		t.Errorf("events = %s", got)
	}
	// Nothing was installed or saved: the state directory holds no config file.
	if _, err := os.Stat(filepath.Join(env["MINION_STATE_DIR"], "config.json")); err == nil {
		t.Error("foreground mode wrote a config file")
	}
}

func TestWorkerUpWithoutServiceManagerRunsInTheForegroundFromTheEnvironment(t *testing.T) {
	env := map[string]string{
		"MINION_SERVER":          "https://hub.example",
		"MINION_INVITE_KEY_FILE": "/run/secrets/invite",
		"MINION_SHARE_EXISTING":  "all",
	}
	deps, p, _ := foregroundFixture(t, env)
	// A container: no systemd, a docker marker.
	deps.detectEnv = service.Env{GOOS: "linux", EUID: 0, Exists: func(p string) bool { return p == "/.dockerenv" }}
	cancel, wait, out := startUp(t, deps, p)
	if got := p.seen(); got.ShareExisting != "all" {
		t.Errorf("MINION_SHARE_EXISTING was not applied: %+v", got.WorkerCore)
	}
	if got := p.seen().StateDir; got != env["MINION_STATE_DIR"] {
		t.Errorf("state dir = %q, want MINION_STATE_DIR %q", got, env["MINION_STATE_DIR"])
	}
	if !strings.Contains(out.String(), "No service manager") || !strings.Contains(out.String(), "/.dockerenv") {
		t.Errorf("the mode is not explained: %q", out.String())
	}
	cancel()
	if err := wait(); err != nil {
		t.Fatal(err)
	}
}

func TestWorkerUpForegroundFailedHookAbortsTheStart(t *testing.T) {
	env := map[string]string{}
	runDeps, p := probeOnlyDeps(t, env)
	_ = runDeps.registry.RegisterPreStart("enroll", func(context.Context, agent.Settings) error { return errors.New("invitation expired") })
	deps := workerUpDeps{run: runDeps, detectEnv: service.Env{GOOS: "linux"}, targetEnv: service.SystemTargetEnv(), setup: workerSetupDeps{assumeSetUp: true}}
	root := &cobra.Command{Use: "minion", SilenceUsage: true, SilenceErrors: true}
	workerCommand(root).AddCommand(newWorkerUpCmd(deps))
	root.SetArgs([]string{"worker", "up", "--foreground"})
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	err := root.ExecuteContext(context.Background())
	if err == nil || !strings.Contains(err.Error(), "invitation expired") {
		t.Fatalf("err = %v", err)
	}
	select {
	case <-p.ready:
		t.Error("the Worker started although a hook failed")
	default:
	}
}

func TestWorkerUpForegroundWarnsAboutASecretInArgvWithoutPrintingIt(t *testing.T) {
	const secret = "mnk_FOREGROUNDSECRET.abcdef"
	deps, p, _ := foregroundFixture(t, map[string]string{})
	reg := config.NewRegistry()
	for _, s := range config.Default.ForScope(config.ScopeWorker) {
		reg.Declare(s)
	}
	deps.settings = reg
	cancel, wait, out := startUp(t, deps, p, "--foreground", "--invite-key", secret)
	cancel()
	if err := wait(); err != nil {
		t.Fatal(err)
	}
	stderr := deps.run.stderr.(*bytes.Buffer).String()
	if !strings.Contains(stderr, "--invite-key was given on the command line") {
		t.Errorf("no process-list warning in foreground mode: %q", stderr)
	}
	if strings.Contains(stderr, "FOREGROUNDSECRET") || strings.Contains(out.String(), "FOREGROUNDSECRET") {
		t.Errorf("the secret leaked.\nstdout: %s\nstderr: %s", out, stderr)
	}
}

// A custom state directory outside the home of ann is checked all the way to
// the file system root: a link in its ancestry that points into another
// account's directory is not followed, and root neither reads, writes nor
// gives anything there.
func TestWorkerUpUnderSudoDoesNotFollowALinkedAncestorOfACustomStateDirectory(t *testing.T) {
	f := newUpFixture(t, true)
	shared := t.TempDir() // stands for /srv/shared
	other := t.TempDir()  // another account
	if err := os.MkdirAll(filepath.Join(other, "state"), 0o700); err != nil {
		t.Fatal(err)
	}
	otherCfg := filepath.Join(other, "state", "config.json")
	if err := os.WriteFile(otherCfg, []byte(`{"name":"other-account"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(other, filepath.Join(shared, "link")); err != nil {
		t.Fatal(err)
	}
	_, err := f.run("--state-dir", filepath.Join(shared, "link", "state"), "--share-existing", "all")
	if err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("err = %v", err)
	}
	if data, _ := os.ReadFile(otherCfg); string(data) != `{"name":"other-account"}` {
		t.Errorf("the other account's config was written: %s", data)
	}
	if entries, _ := os.ReadDir(filepath.Join(other, "state")); len(entries) != 1 {
		t.Errorf("something was created in the other account's directory: %v", entries)
	}
	if len(f.chowned) != 0 || len(f.events) != 0 {
		t.Errorf("work was done: chowned %v events %v", f.chowned, f.events)
	}
}

// ann can replace a directory of the state by a link to another account at any
// time, for example while the hooks run. Root's ownership changes and config
// handling stay inside the state directory.
func TestWorkerUpUnderSudoStaysInTheStateDirectoryWhenADirectoryIsSwappedForALink(t *testing.T) {
	f := newUpFixture(t, true)
	other := t.TempDir()
	if err := os.WriteFile(filepath.Join(other, "secret"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.hook("swap", func(s agent.Settings) error {
		if err := os.MkdirAll(filepath.Join(s.StateDir, "sub"), 0o700); err != nil {
			return err
		}
		if err := os.Remove(filepath.Join(s.StateDir, "sub")); err != nil {
			return err
		}
		if err := os.Symlink(other, filepath.Join(s.StateDir, "sub")); err != nil {
			return err
		}
		// The config file becomes a link to the other account's file, too.
		return os.Symlink(filepath.Join(other, "secret"), filepath.Join(s.StateDir, "config.json"))
	})
	_, err := f.run("--name", "x")
	if err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("err = %v", err)
	}
	if data, _ := os.ReadFile(filepath.Join(other, "secret")); string(data) != "x" {
		t.Errorf("the other account's file was written: %q", data)
	}
	for _, p := range f.chowned {
		if strings.HasPrefix(p, other) {
			t.Errorf("gave %s of another account to ann", p)
		}
	}
	if len(f.runCalls()) != 0 {
		t.Errorf("the service was touched: %v", f.runCalls())
	}
}

// inviteWarningCount runs "worker up" (service path) with the given invitation
// key arguments and counts the process-list warning lines the user sees. The
// test hook behaves like the enrollment hook: it warns about a literal key
// unless "worker up" already did (Settings.InviteArgvWarned).
func inviteWarningCount(t *testing.T, env map[string]string, args ...string) (lines int, stderr string) {
	t.Helper()
	f := newUpFixture(t, false)
	for k, v := range env {
		f.env[k] = v
	}
	reg := config.NewRegistry()
	for _, s := range config.Default.ForScope(config.ScopeWorker) {
		reg.Declare(s)
	}
	f.deps.settings = reg
	f.hook("enroll", func(s agent.Settings) error {
		if _, literal, err := config.InviteKeyGiven(s.Resolver); err != nil {
			return err
		} else if literal && !s.InviteArgvWarned {
			_, _ = fmt.Fprintln(f.stderr, config.InviteKeyArgvWarning)
		}
		return nil
	})
	if _, err := f.run(append([]string{"--server", "https://hub.example:8443"}, args...)...); err != nil {
		t.Fatal(err)
	}
	got := f.stderr.String()
	return strings.Count(got, "process list"), got
}

func TestWorkerUpWarnsExactlyOnceAboutALiteralInvitationKeyAndNeverAboutAFileOrStdin(t *testing.T) {
	const secret = "mnk_ONCESECRET.abcdef"
	keyFile := filepath.Join(t.TempDir(), "invite")
	cases := []struct {
		name string
		env  map[string]string
		args []string
		want int
	}{
		{"literal", nil, []string{"--invite-key", secret}, 1},
		{"file path", nil, []string{"--invite-key", keyFile}, 0},
		{"stdin", nil, []string{"--invite-key", "-"}, 0},
		{"environment file", map[string]string{"MINION_INVITE_KEY_FILE": keyFile}, nil, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			n, stderr := inviteWarningCount(t, c.env, c.args...)
			if n != c.want {
				t.Errorf("%d process-list warnings, want %d: %q", n, c.want, stderr)
			}
			if strings.Contains(stderr, "ONCESECRET") {
				t.Errorf("the key leaked: %q", stderr)
			}
		})
	}
}

func TestWorkerUpForegroundWarnsExactlyOnceAboutALiteralInvitationKey(t *testing.T) {
	const secret = "mnk_FGONCE.abcdef"
	for _, c := range []struct {
		arg  string
		want int
	}{{secret, 1}, {"/nonexistent/invite-key-file", 0}, {"-", 0}} {
		deps, p, _ := foregroundFixture(t, map[string]string{})
		reg := config.NewRegistry()
		for _, s := range config.Default.ForScope(config.ScopeWorker) {
			reg.Declare(s)
		}
		deps.settings = reg
		cancel, wait, _ := startUp(t, deps, p, "--foreground", "--invite-key", c.arg)
		cancel()
		if err := wait(); err != nil {
			t.Fatal(err)
		}
		stderr := deps.run.stderr.(*bytes.Buffer).String()
		if n := strings.Count(stderr, "process list"); n != c.want {
			t.Errorf("--invite-key %q: %d warnings, want %d: %q", c.arg, n, c.want, stderr)
		}
		if strings.Contains(stderr, "FGONCE") {
			t.Errorf("the key leaked: %q", stderr)
		}
	}
}
