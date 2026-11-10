package releasecheck

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeSystemd prepares a fake systemctl that logs its arguments and lists the
// given loaded units, a fake /run/systemd/system and /etc/systemd/system.
type fakeSystemd struct {
	dir string
	env []string
}

func newFakeSystemd(t *testing.T, running bool, loaded []string, enabled []string) *fakeSystemd {
	t.Helper()
	needTools(t, "sh")
	dir := t.TempDir()
	log := filepath.Join(dir, "calls.log")
	listing := ""
	for _, u := range loaded {
		listing += u + " loaded active running Minion Worker\n"
	}
	script := "#!/bin/sh\n" +
		"echo \"$*\" >> '" + log + "'\n" +
		"if [ \"$1\" = list-units ]; then printf '%s' '" + listing + "'; fi\n" +
		"exit 0\n"
	if err := os.WriteFile(filepath.Join(dir, "systemctl"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	runDir := filepath.Join(dir, "run")
	if running {
		if err := os.MkdirAll(runDir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	etc := filepath.Join(dir, "etc")
	wants := filepath.Join(etc, "multi-user.target.wants")
	if err := os.MkdirAll(wants, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, u := range enabled {
		if err := os.Symlink("/usr/lib/systemd/system/minion-worker@.service", filepath.Join(wants, u)); err != nil {
			t.Fatal(err)
		}
	}
	return &fakeSystemd{dir: dir, env: []string{
		"MINION_SYSTEMCTL=" + filepath.Join(dir, "systemctl"),
		"MINION_SYSTEMD_RUN_DIR=" + runDir,
		"MINION_SYSTEMD_ETC_DIR=" + etc,
	}}
}

func (f *fakeSystemd) calls(t *testing.T) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(f.dir, "calls.log"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

func scriptPath(t *testing.T, rel string) string {
	t.Helper()
	return filepath.Join(repoRoot(t), filepath.FromSlash(rel))
}

func TestPostinstall(t *testing.T) {
	const script = "deploy/nfpm/postinstall.sh"
	const restart = "try-restart minion-worker@*.service"
	tests := []struct {
		name    string
		running bool
		args    []string
		want    []string
	}{
		{"deb first install", true, []string{"configure"}, []string{"daemon-reload"}},
		{"deb upgrade", true, []string{"configure", "0.1.0"}, []string{"daemon-reload", restart}},
		{"rpm first install", true, []string{"1"}, []string{"daemon-reload"}},
		{"rpm upgrade", true, []string{"2"}, []string{"daemon-reload", restart}},
		{"no systemd running (container build)", false, []string{"configure"}, nil},
		{"no systemd running on upgrade", false, []string{"2"}, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fs := newFakeSystemd(t, tc.running, nil, nil)
			res := run(t, "", fs.env, "sh", append([]string{scriptPath(t, script)}, tc.args...)...)
			if res.code != 0 {
				t.Fatalf("exit %d: %s", res.code, res.combined())
			}
			got := fs.calls(t)
			if strings.Join(got, "|") != strings.Join(tc.want, "|") {
				t.Errorf("systemctl calls = %q, want %q", got, tc.want)
			}
			// A new instance is never enabled or started by the package.
			for _, c := range got {
				if strings.Contains(c, "enable") || strings.HasPrefix(c, "start") {
					t.Errorf("unexpected call %q", c)
				}
			}
		})
	}
}

func TestPostinstallHintOnFirstInstallOnly(t *testing.T) {
	fs := newFakeSystemd(t, true, nil, nil)
	first := run(t, "", fs.env, "sh", scriptPath(t, "deploy/nfpm/postinstall.sh"), "configure")
	if !strings.Contains(first.stdout, "minion worker up --help") {
		t.Errorf("first install prints no hint: %q", first.stdout)
	}
	upgrade := run(t, "", fs.env, "sh", scriptPath(t, "deploy/nfpm/postinstall.sh"), "configure", "0.1.0")
	if upgrade.stdout != "" {
		t.Errorf("an upgrade prints nothing, got %q", upgrade.stdout)
	}
}

func TestPreremove(t *testing.T) {
	const script = "deploy/nfpm/preremove.sh"
	loaded := []string{"minion-worker@alice.service", "minion-worker@bob.service"}
	// carol is enabled but not loaded; alice is both (listed once).
	enabled := []string{"minion-worker@carol.service", "minion-worker@alice.service"}
	removal := []string{
		"list-units --all --plain --no-legend --no-pager minion-worker@*.service",
		"disable --now minion-worker@alice.service",
		"disable --now minion-worker@bob.service",
		"disable --now minion-worker@carol.service",
		"daemon-reload",
	}
	tests := []struct {
		name string
		args []string
		want []string
	}{
		{"deb remove", []string{"remove"}, removal},
		{"rpm erase", []string{"0"}, removal},
		{"deb upgrade", []string{"upgrade"}, nil},
		{"deb deconfigure", []string{"deconfigure"}, nil},
		{"rpm upgrade", []string{"1"}, nil},
		{"no argument", nil, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fs := newFakeSystemd(t, true, loaded, enabled)
			res := run(t, "", fs.env, "sh", append([]string{scriptPath(t, script)}, tc.args...)...)
			if res.code != 0 {
				t.Fatalf("exit %d: %s", res.code, res.combined())
			}
			got := fs.calls(t)
			if strings.Join(got, "\n") != strings.Join(tc.want, "\n") {
				t.Errorf("systemctl calls:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(tc.want, "\n"))
			}
		})
	}
}

// Offline (chroot, image build): nothing can be stopped, listed or reloaded,
// but the enabled instances are still disabled, so that a later reinstall does
// not start them at boot.
func TestPreremoveWithoutSystemdDisablesEnabledInstances(t *testing.T) {
	loaded := []string{"minion-worker@alice.service"} // not visible offline
	enabled := []string{"minion-worker@carol.service", "minion-worker@bob.service"}
	for _, args := range [][]string{{"remove"}, {"0"}} {
		fs := newFakeSystemd(t, false, loaded, enabled)
		res := run(t, "", fs.env, "sh", scriptPath(t, "deploy/nfpm/preremove.sh"), args[0])
		if res.code != 0 {
			t.Fatalf("exit %d: %s", res.code, res.combined())
		}
		want := "disable minion-worker@bob.service\ndisable minion-worker@carol.service"
		if got := strings.Join(fs.calls(t), "\n"); got != want {
			t.Errorf("%v: systemctl calls:\n%s\nwant:\n%s", args, got, want)
		}
	}
	// An upgrade offline does nothing, and neither does a removal with no
	// enabled instance.
	fs := newFakeSystemd(t, false, nil, []string{"minion-worker@carol.service"})
	run(t, "", fs.env, "sh", scriptPath(t, "deploy/nfpm/preremove.sh"), "upgrade")
	fs2 := newFakeSystemd(t, false, nil, nil)
	run(t, "", fs2.env, "sh", scriptPath(t, "deploy/nfpm/preremove.sh"), "remove")
	if len(fs.calls(t)) != 0 || len(fs2.calls(t)) != 0 {
		t.Errorf("unexpected calls: %q %q", fs.calls(t), fs2.calls(t))
	}
}

// A failing systemctl must never make the package removal or installation fail.
func TestScriptsIgnoreSystemctlFailures(t *testing.T) {
	needTools(t, "sh")
	dir := t.TempDir()
	failing := filepath.Join(dir, "systemctl")
	if err := os.WriteFile(failing, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	runDir := filepath.Join(dir, "run")
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatal(err)
	}
	env := []string{"MINION_SYSTEMCTL=" + failing, "MINION_SYSTEMD_RUN_DIR=" + runDir, "MINION_SYSTEMD_ETC_DIR=" + dir}
	for _, c := range [][]string{
		{"deploy/nfpm/postinstall.sh", "configure", "0.1.0"},
		{"deploy/nfpm/preremove.sh", "remove"},
	} {
		if res := run(t, "", env, "sh", scriptPath(t, c[0]), c[1]); res.code != 0 {
			t.Errorf("%s exited %d: %s", c[0], res.code, res.combined())
		}
	}
}

func TestShellScriptsPassShellcheck(t *testing.T) {
	needTools(t, "shellcheck")
	for _, f := range []string{
		"deploy/nfpm/postinstall.sh", "deploy/nfpm/preremove.sh",
		"scripts/assert-image-platforms.sh", "scripts/release-manifest.sh",
	} {
		if res := run(t, "", nil, "shellcheck", scriptPath(t, f)); res.code != 0 {
			t.Errorf("shellcheck %s:\n%s", f, res.combined())
		}
	}
}

func TestScriptsAreExecutable(t *testing.T) {
	for _, f := range []string{
		"deploy/nfpm/postinstall.sh", "deploy/nfpm/preremove.sh",
		"scripts/assert-image-platforms.sh", "scripts/release-manifest.sh",
	} {
		st, err := os.Stat(scriptPath(t, f))
		if err != nil {
			t.Fatal(err)
		}
		if st.Mode()&0o111 == 0 {
			t.Errorf("%s is not executable (nfpm and CI call it directly)", f)
		}
	}
}
