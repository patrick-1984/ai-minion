package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/worker/agent"
)

// probe is a test module: it records the settings and services it sees and
// when it starts and stops.
type probe struct {
	mu       sync.Mutex
	settings agent.Settings
	events   []string
	ready    chan struct{}
	services func(env *agent.Env) error
}

func newProbe() *probe { return &probe{ready: make(chan struct{})} }

func (p *probe) event(e string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.events = append(p.events, e)
}

func (p *probe) log() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.events...)
}

func (p *probe) seen() agent.Settings {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.settings
}

// register adds the probe after every module of reg (order 900).
func (p *probe) register(t *testing.T, reg *agent.Registry) {
	t.Helper()
	err := reg.Register("probe", 900, func(_ context.Context, env *agent.Env) (agent.StopFunc, error) {
		p.mu.Lock()
		p.settings = env.Settings()
		p.mu.Unlock()
		if p.services != nil {
			if err := p.services(env); err != nil {
				return nil, err
			}
		}
		p.event("probe start")
		close(p.ready)
		return func(context.Context) error { p.event("probe stop"); return nil }, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// workerRoot builds a root with only "worker run" on top of deps.
func workerRoot(deps workerRunDeps) *cobra.Command {
	root := &cobra.Command{Use: "minion", SilenceUsage: true, SilenceErrors: true}
	workerCommand(root).AddCommand(newWorkerRunCmd(deps))
	return root
}

// startWorker runs "worker run" with args in the background. cancel ends it;
// wait returns its error.
func startWorker(t *testing.T, deps workerRunDeps, p *probe, args ...string) (cancel func(), wait func() error) {
	t.Helper()
	root := workerRoot(deps)
	root.SetArgs(append([]string{"worker", "run"}, args...))
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	ctx, cancelCtx := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- root.ExecuteContext(ctx) }()
	t.Cleanup(cancelCtx)
	select {
	case <-p.ready:
	case err := <-done:
		t.Fatalf("worker run ended before the probe started: %v", err)
	case <-time.After(30 * time.Second):
		t.Fatal("worker run did not start")
	}
	return cancelCtx, func() error {
		select {
		case err := <-done:
			return err
		case <-time.After(15 * time.Second):
			t.Fatal("worker run did not stop")
			return nil
		}
	}
}

func envFrom(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
}

// probeOnlyDeps returns deps with a registry that holds just the probe, so the
// tests of the settings need neither tmux nor a state database.
func probeOnlyDeps(t *testing.T, env map[string]string) (workerRunDeps, *probe) {
	t.Helper()
	reg := agent.NewRegistry()
	p := newProbe()
	p.register(t, reg)
	if _, ok := env["MINION_STATE_DIR"]; !ok {
		env["MINION_STATE_DIR"] = t.TempDir()
	}
	return workerRunDeps{stderr: &bytes.Buffer{}, lookupEnv: envFrom(env), loadConfig: loadWorkerConfig, registry: reg}, p
}

func TestWorkerRunIsRegisteredWithFlagsFromTheRegistry(t *testing.T) {
	root := NewRoot()
	cmd, _, err := root.Find([]string{"worker", "run"})
	if err != nil || cmd.Name() != "run" {
		t.Fatalf("worker run not found: %v", err)
	}
	for _, key := range []string{config.KeyTmuxSocket, config.KeyName, config.KeyShareExisting, config.KeyPreviewOnly, config.KeyStateDir} {
		s, _ := config.Default.Lookup(key)
		f := cmd.Flags().Lookup(s.FlagName())
		if f == nil {
			t.Errorf("no flag --%s for %s", s.FlagName(), key)
			continue
		}
		if !strings.Contains(f.Usage, s.EnvName()) {
			t.Errorf("--%s usage %q does not name %s", s.FlagName(), f.Usage, s.EnvName())
		}
	}
	if f := cmd.Flags().Lookup("preview-only"); f.NoOptDefVal != "true" {
		t.Errorf("--preview-only must work without a value, NoOptDefVal = %q", f.NoOptDefVal)
	}
	if f := cmd.Flags().Lookup("name"); f.NoOptDefVal != "" {
		t.Errorf("--name must take a value")
	}
	var out, errb bytes.Buffer
	if code := Execute(context.Background(), []string{"worker", "run", "extra"}, &out, &errb); code != 1 {
		t.Fatalf("exit code = %d for a stray argument", code)
	}
}

func TestWorkerCommandIsSharedWithOtherSubcommands(t *testing.T) {
	root := &cobra.Command{Use: "minion"}
	existing := &cobra.Command{Use: "worker", Short: "from another story"}
	root.AddCommand(existing)
	if workerCommand(root) != existing || len(root.Commands()) != 1 {
		t.Fatal("workerCommand created a second worker command")
	}
}

func TestAddSettingFlagsSkipsSecretsAndOtherRoles(t *testing.T) {
	reg := config.NewRegistry()
	reg.Declare(config.Setting{Key: "plain", Scope: config.ScopeWorker})
	reg.Declare(config.Setting{Key: "hubonly", Scope: config.ScopeHub})
	reg.Declare(config.Setting{Key: "secret_key", Scope: config.ScopeWorker, Secret: true})
	reg.Declare(config.Setting{Key: "argv_key", Scope: config.ScopeWorker, Secret: true, ArgvAllowedWithWarning: true})
	reg.Declare(config.Setting{Key: "enabled", Scope: config.ScopeWorker, Default: "false", Choices: []string{"true", "false"}})
	reg.Declare(config.Setting{Key: "mode", Scope: config.ScopeWorker, Default: "a", Choices: []string{"a", "b"}})
	cmd := &cobra.Command{Use: "x"}
	addSettingFlags(cmd, reg, config.ScopeWorker)
	for flag, want := range map[string]bool{"plain": true, "hubonly": false, "secret-key": false, "argv-key": true, "enabled": true, "mode": true} {
		if got := cmd.Flags().Lookup(flag) != nil; got != want {
			t.Errorf("flag --%s present = %v, want %v", flag, got, want)
		}
	}
	if cmd.Flags().Lookup("enabled").NoOptDefVal != "true" || cmd.Flags().Lookup("mode").NoOptDefVal != "" {
		t.Error("only the true/false setting is a switch")
	}
}

func TestWorkerRunResolvesSocketOnceAsAbsolutePath(t *testing.T) {
	rel := filepath.Join("rel", "tmux.sock")
	abs, _ := filepath.Abs(rel)
	deps, p := probeOnlyDeps(t, map[string]string{})
	cancel, wait := startWorker(t, deps, p, "--tmux-socket", rel)
	if got := p.seen().TmuxSocket; got != abs {
		t.Errorf("module saw socket %q, want the absolute %q", got, abs)
	}
	cancel()
	if err := wait(); err != nil {
		t.Fatal(err)
	}
	if got := p.log(); strings.Join(got, ",") != "probe start,probe stop" {
		t.Fatalf("events = %v", got)
	}
}

func TestWorkerRunDefaultsToTheUsersDefaultTmuxSocket(t *testing.T) {
	if os.PathSeparator == '\\' {
		t.Skip("no tmux socket on Windows")
	}
	tmp := t.TempDir()
	t.Setenv("TMUX_TMPDIR", tmp)
	deps, p := probeOnlyDeps(t, map[string]string{})
	cancel, wait := startWorker(t, deps, p)
	want := filepath.Join(tmp, "tmux-"+strconv.Itoa(os.Getuid()), "default")
	if got := p.seen().TmuxSocket; got != want {
		t.Errorf("default socket = %q, want %q", got, want)
	}
	cancel()
	if err := wait(); err != nil {
		t.Fatal(err)
	}
}

func TestWorkerRunSettingsPrecedenceFlagEnvFile(t *testing.T) {
	state := t.TempDir()
	file := filepath.Join(state, workerConfigName)
	if err := os.WriteFile(file, []byte(`{"name":"from-file","share_existing":"all","preview_only":"true","tmux_socket":"/from/file.sock"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	// File only.
	deps, p := probeOnlyDeps(t, map[string]string{"MINION_STATE_DIR": state})
	cancel, wait := startWorker(t, deps, p)
	s := p.seen()
	if s.Name != "from-file" || s.ShareExisting != "all" || !s.PreviewOnly || s.TmuxSocket != "/from/file.sock" {
		t.Errorf("file: %+v", s.WorkerCore)
	}
	cancel()
	_ = wait()

	// Env beats file; flag beats env.
	deps, p = probeOnlyDeps(t, map[string]string{
		"MINION_STATE_DIR": state, "MINION_NAME": "from-env", "MINION_SHARE_EXISTING": "none", "MINION_PREVIEW_ONLY": "false",
	})
	cancel, wait = startWorker(t, deps, p, "--name", "from-flag", "--preview-only")
	s = p.seen()
	if s.Name != "from-flag" || s.ShareExisting != "none" || !s.PreviewOnly {
		t.Errorf("flag/env: %+v", s.WorkerCore)
	}
	cancel()
	_ = wait()

	// --preview-only=false turns it off again.
	deps, p = probeOnlyDeps(t, map[string]string{"MINION_STATE_DIR": state})
	cancel, wait = startWorker(t, deps, p, "--preview-only=false", "--share-existing=later")
	s = p.seen()
	if s.PreviewOnly || s.ShareExisting != "later" {
		t.Errorf("explicit flags: %+v", s.WorkerCore)
	}
	cancel()
	_ = wait()
}

func TestWorkerRunRefusesInvalidSettingsBeforeStarting(t *testing.T) {
	for name, args := range map[string][]string{
		"share":   {"--share-existing", "everyone"},
		"preview": {"--preview-only=maybe"},
		"name":    {"--name", " "},
	} {
		t.Run(name, func(t *testing.T) {
			deps, p := probeOnlyDeps(t, map[string]string{})
			root := workerRoot(deps)
			root.SetArgs(append([]string{"worker", "run"}, args...))
			root.SetOut(&bytes.Buffer{})
			root.SetErr(&bytes.Buffer{})
			if err := root.ExecuteContext(context.Background()); err == nil {
				t.Fatal("accepted")
			}
			if got := p.log(); len(got) != 0 {
				t.Fatalf("a module started although the settings are invalid: %v", got)
			}
		})
	}
}

func TestWorkerRunRefusesAConfigFileOthersCanRead(t *testing.T) {
	if os.PathSeparator == '\\' {
		t.Skip("file modes are not checked on Windows")
	}
	state := t.TempDir()
	cfg := filepath.Join(state, workerConfigName)
	if err := os.WriteFile(cfg, []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	// WriteFile applies the umask; a restrictive one would make the file safe.
	if err := os.Chmod(cfg, 0o644); err != nil {
		t.Fatal(err)
	}
	deps, p := probeOnlyDeps(t, map[string]string{"MINION_STATE_DIR": state})
	root := workerRoot(deps)
	root.SetArgs([]string{"worker", "run"})
	root.SetErr(&bytes.Buffer{})
	// Bounded, so that an unexpected acceptance cannot hang the suite.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := root.ExecuteContext(ctx)
	if err == nil || !strings.Contains(err.Error(), "chmod 600") {
		t.Fatalf("err = %v", err)
	}
	if len(p.log()) != 0 {
		t.Fatal("started with an unsafe config file")
	}
}

func TestWorkerRunReportsAModuleStartFailure(t *testing.T) {
	boom := errors.New("cannot bind")
	deps, _ := probeOnlyDeps(t, map[string]string{})
	if err := deps.registry.Register("bad", 1, func(context.Context, *agent.Env) (agent.StopFunc, error) { return nil, boom }); err != nil {
		t.Fatal(err)
	}
	root := workerRoot(deps)
	root.SetArgs([]string{"worker", "run"})
	root.SetErr(&bytes.Buffer{})
	err := root.ExecuteContext(context.Background())
	if !errors.Is(err, boom) || !strings.Contains(err.Error(), `"bad"`) {
		t.Fatalf("err = %v", err)
	}
}

func TestWorkerRunReportsAFailedRollbackOfACanceledStart(t *testing.T) {
	stopErr := errors.New("close failed")
	deps, _ := probeOnlyDeps(t, map[string]string{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// "a" starts and cancels the run (as SIGTERM during startup would); the
	// start is interrupted before "b" and the rollback of "a" fails.
	if err := deps.registry.Register("a", 1, func(context.Context, *agent.Env) (agent.StopFunc, error) {
		cancel()
		return func(context.Context) error { return stopErr }, nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := deps.registry.Register("b", 2, func(context.Context, *agent.Env) (agent.StopFunc, error) { return nil, nil }); err != nil {
		t.Fatal(err)
	}
	root := workerRoot(deps)
	root.SetArgs([]string{"worker", "run"})
	root.SetErr(&bytes.Buffer{})
	err := root.ExecuteContext(ctx)
	if !errors.Is(err, stopErr) {
		t.Fatalf("err = %v, want the failed rollback reported", err)
	}
}

func TestOnlyCanceled(t *testing.T) {
	other := errors.New("other")
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, true},
		{"plain", context.Canceled, true},
		{"wrapped", fmt.Errorf("interrupted: %w", context.Canceled), true},
		{"joined cancel only", errors.Join(fmt.Errorf("x: %w", context.Canceled), context.Canceled), true},
		{"joined with failure", errors.Join(fmt.Errorf("x: %w", context.Canceled), other), false},
		{"other", other, false},
		{"deadline", context.DeadlineExceeded, false},
	}
	for _, c := range cases {
		if got := onlyCanceled(c.err); got != c.want {
			t.Errorf("%s: onlyCanceled = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestWorkerRunRunsPreStartHooksBeforeModules(t *testing.T) {
	deps, p := probeOnlyDeps(t, map[string]string{})
	if err := deps.registry.RegisterPreStart("enroll", func(_ context.Context, s agent.Settings) error {
		p.event("hook enroll")
		if s.Resolver == nil || s.Out == nil || s.Logger == nil {
			t.Errorf("hook settings incomplete: %+v", s)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	cancel, wait := startWorker(t, deps, p)
	cancel()
	if err := wait(); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(p.log(), ","); got != "hook enroll,probe start,probe stop" {
		t.Fatalf("events = %s", got)
	}
}
