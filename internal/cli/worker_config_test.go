package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/worker/store"
)

// recordingApplier records the changes handed to a running Worker.
type recordingApplier struct {
	changes []configChange
	err     error
}

func (r *recordingApplier) ApplyConfig(_ context.Context, _ string, c configChange) error {
	r.changes = append(r.changes, c)
	return r.err
}

func testConfigDeps(env map[string]string) workerConfigDeps {
	return workerConfigDeps{
		lookupEnv: mapEnv(env),
		openStore: func(ctx context.Context, dir string) (shareStore, error) { return store.OpenContext(ctx, dir) },
		applier:   noLiveApplier{},
	}
}

func readConfig(t *testing.T, dir string) map[string]string {
	t.Helper()
	f, err := config.LoadFile(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	m := map[string]string{}
	for _, k := range f.Keys() {
		m[k], _, _ = f.Lookup(k)
	}
	return m
}

func loadShare(t *testing.T, dir string) store.ShareSelection {
	t.Helper()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	sel, err := st.LoadShare()
	if err != nil {
		t.Fatal(err)
	}
	return sel
}

func configCmd(t *testing.T, cd workerConfigDeps, dir string, args ...string) (string, string, error) {
	t.Helper()
	full := append([]string{"worker", "config"}, args...)
	full = append(full, "--state-dir", dir)
	return runWorkerCmds(t, testStatusDeps(nil), cd, full...)
}

func TestConfigSetServerSavesTheURLInTheConfigFile(t *testing.T) {
	dir := t.TempDir()
	out, _, err := configCmd(t, testConfigDeps(nil), dir, "set", "server", "https://hub.example:8443/prefix")
	if err != nil {
		t.Fatal(err)
	}
	if got := readConfig(t, dir)["server"]; got != "https://hub.example:8443/prefix" {
		t.Fatalf("server = %q", got)
	}
	// Nothing was asked of a Worker, so the output says what happens to the
	// saved value and what a running Worker needs, and claims nothing else.
	if !strings.Contains(out, "used the next time the Worker starts") || !strings.Contains(out, "must be restarted to pick it up") {
		t.Fatalf("output = %q", out)
	}
	if strings.Contains(out, "No running Worker") {
		t.Fatalf("output claims no Worker runs: %q", out)
	}
	info, err := os.Stat(filepath.Join(dir, "config.json"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("config file = %v, %v", info, err)
	}

	// Status resolves the saved value through the normal precedence chain.
	sd := testStatusDeps(nil)
	st, _, err := runWorkerCmds(t, sd, workerConfigDeps{}, "worker", "status", "--json", "--state-dir", dir)
	if err != nil || !strings.Contains(st, `"hub_url": "https://hub.example:8443/prefix"`) {
		t.Fatalf("status = %s, %v", st, err)
	}
}

func TestConfigSetServerRefusesABadURLAndKeepsTheFile(t *testing.T) {
	dir := t.TempDir()
	if _, _, err := configCmd(t, testConfigDeps(nil), dir, "set", "server", "https://hub.example"); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"ftp://hub.example", "https://user:hunter2@hub.example", "not a url", "https://"} {
		_, _, err := configCmd(t, testConfigDeps(nil), dir, "set", "server", bad)
		if err == nil {
			t.Fatalf("%q was accepted", bad)
		}
		if strings.Contains(err.Error(), "hunter2") {
			t.Fatalf("the error repeats the credential: %v", err)
		}
	}
	if got := readConfig(t, dir)["server"]; got != "https://hub.example" {
		t.Fatalf("a refused value changed the file: %q", got)
	}
}

func TestConfigSetValidatesChoicesAndNames(t *testing.T) {
	dir := t.TempDir()
	cd := testConfigDeps(nil)
	for _, c := range [][]string{
		{"preview_only", "maybe"},
		{"hub_insecure", "2"},
		{"name", strings.Repeat("x", config.MaxNameLength+1)},
		{"log_level", "loud"},
	} {
		if _, _, err := configCmd(t, cd, dir, "set", c[0], c[1]); err == nil {
			t.Errorf("set %v was accepted", c)
		}
	}
	if _, _, err := configCmd(t, cd, dir, "set", "preview_only", "true"); err != nil {
		t.Fatal(err)
	}
	if got := readConfig(t, dir)["preview_only"]; got != "true" {
		t.Fatalf("preview_only = %q", got)
	}
}

func TestConfigSetRefusesUnknownKeysAndTheStateDir(t *testing.T) {
	dir := t.TempDir()
	_, _, err := configCmd(t, testConfigDeps(nil), dir, "set", "sever", "https://hub.example")
	if err == nil || !strings.Contains(err.Error(), "server") {
		t.Fatalf("unknown key error = %v", err)
	}
	if _, _, err := configCmd(t, testConfigDeps(nil), dir, "set", "state_dir", "/elsewhere"); err == nil {
		t.Fatal("state_dir was accepted")
	}
	if _, _, err := configCmd(t, testConfigDeps(nil), dir, "set", "server", " "); err == nil || !strings.Contains(err.Error(), "unset") {
		t.Fatalf("empty value error = %v", err)
	}
	if len(readConfig(t, dir)) != 0 {
		t.Fatal("a refused command wrote the config file")
	}
}

// secretRegistry is a registry with a plain secret and the invitation key
// exception, as Story 4.6 declares it.
func secretRegistry() *config.Registry {
	r := config.NewRegistry()
	r.Declare(config.Setting{Key: config.KeyStateDir, Scope: config.ScopeAny, Default: "/unused"})
	r.Declare(config.Setting{Key: "api_token", Scope: config.ScopeWorker, Secret: true})
	r.Declare(config.Setting{Key: "invite_key", Scope: config.ScopeWorker, Secret: true, ArgvAllowedWithWarning: true})
	return r
}

func TestConfigSetRefusesSecretsOnArgvExceptTheInvitationKey(t *testing.T) {
	dir := t.TempDir()
	cd := testConfigDeps(nil)
	cd.registry = secretRegistry()

	const secret = "s3cr3t-token-value"
	_, _, err := configCmd(t, cd, dir, "set", "api_token", secret)
	if err == nil || !strings.Contains(err.Error(), "MINION_API_TOKEN_FILE") {
		t.Fatalf("secret error = %v", err)
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("the error repeats the secret: %v", err)
	}
	if len(readConfig(t, dir)) != 0 {
		t.Fatal("a refused secret was written")
	}

	const key = "mnk_exampleexampleexample.secretsecret"
	out, errOut, err := configCmd(t, cd, dir, "set", "invite_key", key)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(errOut, "visible in the process list") {
		t.Fatalf("no argv warning: %q", errOut)
	}
	if strings.Contains(out+errOut, key) {
		t.Fatalf("the invitation key was echoed: %q %q", out, errOut)
	}
	if got := readConfig(t, dir)["invite_key"]; got != key {
		t.Fatalf("invite_key = %q", got)
	}
}

func TestConfigSetWarnsWhenTheEnvironmentWins(t *testing.T) {
	dir := t.TempDir()
	_, errOut, err := configCmd(t, testConfigDeps(map[string]string{"MINION_SERVER": "https://other"}), dir, "set", "server", "https://hub.example")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(errOut, "MINION_SERVER") || !strings.Contains(errOut, "wins over the config file") {
		t.Fatalf("warning = %q", errOut)
	}
	if readConfig(t, dir)["server"] != "https://hub.example" {
		t.Fatal("the value was not saved")
	}
}

func TestConfigSetHandsTheChangeToARunningWorker(t *testing.T) {
	dir := t.TempDir()
	cd := testConfigDeps(nil)
	app := &recordingApplier{}
	cd.applier = app
	out, _, err := configCmd(t, cd, dir, "set", "server", "https://hub.example")
	if err != nil {
		t.Fatal(err)
	}
	if len(app.changes) != 1 || app.changes[0] != (configChange{Key: "server", Value: "https://hub.example"}) {
		t.Fatalf("changes = %+v", app.changes)
	}
	if !strings.Contains(out, "running Worker applied the change") {
		t.Fatalf("output = %q", out)
	}

	app.err = errors.New("socket closed")
	out, _, err = configCmd(t, cd, dir, "set", "server", "https://hub2.example")
	if err != nil {
		t.Fatalf("a failed live apply must not fail a saved change: %v", err)
	}
	if !strings.Contains(out, "saved, but the running Worker could not apply it") || readConfig(t, dir)["server"] != "https://hub2.example" {
		t.Fatalf("output = %q", out)
	}
}

func TestConfigUnsetRemovesAKeyAndReturnsToSolo(t *testing.T) {
	dir := t.TempDir()
	if _, _, err := configCmd(t, testConfigDeps(nil), dir, "set", "server", "https://hub.example"); err != nil {
		t.Fatal(err)
	}
	app := &recordingApplier{}
	cd := testConfigDeps(nil)
	cd.applier = app
	if _, _, err := configCmd(t, cd, dir, "unset", "server"); err != nil {
		t.Fatal(err)
	}
	if _, ok := readConfig(t, dir)["server"]; ok {
		t.Fatal("server is still in the file")
	}
	if len(app.changes) != 1 || !app.changes[0].Unset || app.changes[0].Key != "server" {
		t.Fatalf("changes = %+v", app.changes)
	}
	out, _, err := configCmd(t, cd, dir, "unset", "server")
	if err != nil || !strings.Contains(out, "nothing to remove") {
		t.Fatalf("second unset = %q, %v", out, err)
	}
	if len(app.changes) != 1 {
		t.Fatal("an unset of an absent key reached the Worker")
	}
}

func TestConfigShareModesAreSavedInTheState(t *testing.T) {
	dir := t.TempDir()
	cd := testConfigDeps(nil)
	for _, c := range []struct {
		args []string
		mode store.ShareMode
		list []string
	}{
		{[]string{"share", "all"}, store.ShareAll, nil},
		{[]string{"share", "none"}, store.ShareNone, nil},
		{[]string{"share", "select", "build", "docs", "build"}, store.ShareSelect, []string{"build", "docs"}},
		{[]string{"share", "later"}, store.ShareLater, []string{"build", "docs"}},
		{[]string{"share", "select"}, store.ShareSelect, []string{"build", "docs"}},
	} {
		out, _, err := configCmd(t, cd, dir, c.args...)
		if err != nil {
			t.Fatalf("%v: %v", c.args, err)
		}
		sel := loadShare(t, dir)
		if sel.Mode != c.mode || !sel.Configured || (c.list != nil && strings.Join(sel.Sessions, ",") != strings.Join(c.list, ",")) {
			t.Fatalf("%v: selection = %+v", c.args, sel)
		}
		if !strings.Contains(out, "Share mode is now") {
			t.Fatalf("%v: output = %q", c.args, out)
		}
	}
}

func TestConfigShareRefusesBadInput(t *testing.T) {
	dir := t.TempDir()
	cd := testConfigDeps(nil)
	for _, args := range [][]string{
		{"share", "everything"},
		{"share", "all", "build"},
		{"share", "select"}, // nothing chosen before
		{"share", "select", " "},
		{"share"},
	} {
		if _, _, err := configCmd(t, cd, dir, args...); err == nil {
			t.Errorf("%v was accepted", args)
		}
	}
	if sel := loadShare(t, dir); sel.Configured {
		t.Fatalf("a refused command saved %+v", sel)
	}
}

func TestConfigShareKeepsTheShareExistingKeyOfTheConfigFileInStep(t *testing.T) {
	dir := t.TempDir()
	cd := testConfigDeps(nil)
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"share_existing":"all","name":"lab"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := configCmd(t, cd, dir, "share", "none"); err != nil {
		t.Fatal(err)
	}
	// Otherwise the next start would apply "all" over this choice.
	if got := readConfig(t, dir); got["share_existing"] != "none" || got["name"] != "lab" {
		t.Fatalf("config = %v", got)
	}
	if _, _, err := configCmd(t, cd, dir, "share", "select", "build"); err != nil {
		t.Fatal(err)
	}
	if got := readConfig(t, dir); got["share_existing"] != "" || got["name"] != "lab" {
		t.Fatalf("config = %v", got)
	}
	if _, ok := readConfig(t, dir)["share_existing"]; ok {
		t.Fatal("share_existing must be removed for select, which it cannot express")
	}

	// Without the key the file is not created for nothing.
	other := t.TempDir()
	if _, _, err := configCmd(t, cd, other, "share", "all"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(other, "config.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("config.json was created: %v", err)
	}
}

func TestConfigSetShareExistingGoesThroughTheShareChoice(t *testing.T) {
	dir := t.TempDir()
	cd := testConfigDeps(nil)
	if _, _, err := configCmd(t, cd, dir, "set", "share_existing", "all"); err != nil {
		t.Fatal(err)
	}
	if sel := loadShare(t, dir); sel.Mode != store.ShareAll {
		t.Fatalf("selection = %+v", sel)
	}
	if _, _, err := configCmd(t, cd, dir, "set", "share_existing", "select"); err == nil {
		t.Fatal("share_existing=select was accepted")
	}
}

func TestConfigShareWarnsWhenTheEnvironmentWillOverrideAtStart(t *testing.T) {
	dir := t.TempDir()
	_, errOut, err := configCmd(t, testConfigDeps(map[string]string{"MINION_SHARE_EXISTING": "all"}), dir, "share", "none")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(errOut, "MINION_SHARE_EXISTING") {
		t.Fatalf("warning = %q", errOut)
	}
}

func TestConfigFileThatOthersCanReadIsRefused(t *testing.T) {
	if os.PathSeparator == '\\' {
		t.Skip("file modes do not apply on Windows")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	// WriteFile is filtered by the umask; make the fixture open for real.
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := configCmd(t, testConfigDeps(nil), dir, "set", "server", "https://hub.example"); err == nil {
		t.Fatal("an open config file was accepted")
	}
}

// failingSaveStore is a share store whose write is refused.
type failingSaveStore struct{ shareStore }

func (failingSaveStore) SaveShareContext(context.Context, store.ShareSelection) error {
	return errors.New("disk full")
}

// A store that refuses the write must not leave the config file ahead of it:
// the next start would apply the new share_existing over the old store state.
func TestConfigShareLeavesNothingChangedWhenTheStoreRefuses(t *testing.T) {
	dir := t.TempDir()
	cd := testConfigDeps(nil)
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"share_existing":"all"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := configCmd(t, cd, dir, "share", "all"); err != nil {
		t.Fatal(err)
	}
	cd.openStore = func(ctx context.Context, d string) (shareStore, error) {
		st, err := store.OpenContext(ctx, d)
		if err != nil {
			return nil, err
		}
		return failingSaveStore{st}, nil
	}
	for _, args := range [][]string{{"share", "none"}, {"share", "select", "build"}} {
		if _, _, err := configCmd(t, cd, dir, args...); err == nil || !strings.Contains(err.Error(), "disk full") {
			t.Fatalf("%v: err = %v", args, err)
		}
		if got := readConfig(t, dir)["share_existing"]; got != "all" {
			t.Fatalf("%v left share_existing = %q in the file, the store still says all", args, got)
		}
	}
	if sel := loadShare(t, dir); sel.Mode != store.ShareAll {
		t.Fatalf("share = %+v", sel)
	}
}

// Two commands that run at the same time each keep their change.
func TestConfigCommandsRunningAtTheSameTimeKeepEveryChange(t *testing.T) {
	dir := t.TempDir()
	changes := [][]string{
		{"set", "server", "https://hub.example"},
		{"set", "preview_only", "true"},
		{"set", "name", "lab-pi"},
		{"set", "log_level", "debug"},
		{"set", "hub_insecure", "false"},
	}
	var wg sync.WaitGroup
	errs := make([]error, len(changes))
	for i, c := range changes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, errs[i] = configCmd(t, testConfigDeps(nil), dir, c...)
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("%v: %v", changes[i], err)
		}
	}
	got := readConfig(t, dir)
	for _, c := range changes {
		if got[c[1]] != c[2] {
			t.Errorf("%s = %q, want %q; config = %v", c[1], got[c[1]], c[2], got)
		}
	}
}

// A relative socket path means something else from another directory, and the
// service starts in the home directory; the absolute path is what gets saved.
func TestConfigSetSavesTheTmuxSocketAsAnAbsolutePath(t *testing.T) {
	dir := t.TempDir()
	if _, _, err := configCmd(t, testConfigDeps(nil), dir, "set", "tmux_socket", "./tmux.sock"); err != nil {
		t.Fatal(err)
	}
	got := readConfig(t, dir)["tmux_socket"]
	if !filepath.IsAbs(got) || filepath.Base(got) != "tmux.sock" {
		t.Fatalf("tmux_socket = %q", got)
	}
}

var _ configApplier = (*recordingApplier)(nil)
