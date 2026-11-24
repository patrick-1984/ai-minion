//go:build linux

package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/patrick-1984/ai-minion/internal/worker/agent"
)

// legacyLoginBlock is the block v0.20.5 and earlier appended, byte for byte.
const legacyLoginBlock = `
# Minion Worker login startup
if [ -r "$HOME/.config/minion/worker-login" ]; then
    (IFS= read -r minion_bin && exec "$minion_bin" worker start --at-login </dev/null) < "$HOME/.config/minion/worker-login" >/dev/null 2>&1 &
fi
# End Minion Worker login startup
`

func TestLoginBlockText(t *testing.T) {
	want := `
# Minion Worker login startup
if [ "${minion_worker_login_started-}" != "$$" ] && [ -r "$HOME/.config/minion/worker-login" ]; then
    minion_worker_login_started=$$
    ( (IFS= read -r minion_bin && exec "$minion_bin" worker start --at-login </dev/null) < "$HOME/.config/minion/worker-login" >/dev/null 2>&1 & )
fi
# End Minion Worker login startup
`
	if loginBlock != want {
		t.Fatalf("login block:\n%s", loginBlock)
	}
	if "\n"+legacyLoginBlockBodies[0]+"\n" != legacyLoginBlock {
		t.Fatal("legacy block differs from what v0.20.5 wrote")
	}
	if strings.Contains(loginBlock, "export") {
		t.Fatal("the guard must not be exported: a tmux server started from the shell would inherit it")
	}
}

// loginHome is a private HOME whose fake minion records every call.
func loginHome(t *testing.T, block string) (home, calls string) {
	t.Helper()
	home = t.TempDir()
	calls = filepath.Join(home, "calls")
	bin := filepath.Join(home, "bin dir", "minion")
	if err := os.MkdirAll(filepath.Dir(bin), 0o700); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\necho \"$*\" >> \"$HOME/calls\"\n"
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, ".config", "minion"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(loginRegistration(home), []byte(bin+"\n"+filepath.Join(home, "state")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		// The common Debian/Ubuntu layout: a login shell reads .profile,
		// which sources .bashrc; both hold the block.
		".profile":  ". \"$HOME/.bashrc\"\n" + block,
		".bashrc":   "PS1='$ '\n" + block,
		".zprofile": ". \"$HOME/.bashrc\"\n" + block,
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(home, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return home, calls
}

// runInteractive runs shell as an interactive login shell on a terminal
// (script(1) provides the pty) with a private HOME and returns its output.
func runInteractive(t *testing.T, home string, shell ...string) string {
	t.Helper()
	if _, err := exec.LookPath("script"); err != nil {
		t.Skip("script(1) is not available")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "script", "-qec", strings.Join(shell, " "), "/dev/null")
	cmd.Env = []string{"HOME=" + home, "ZDOTDIR=" + home, "PATH=/usr/bin:/bin", "TERM=dumb", "SHELL=" + shell[0]}
	cmd.Dir = home
	// The pauses let the background start finish while the shell is at its
	// prompt, where a job-control shell would print its "Done" notice.
	cmd.Stdin = strings.NewReader("sleep 1\n: one\nsleep 1\n: two\nexit\n")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %v\n%s", shell, err, out)
	}
	return string(out)
}

func waitCalls(t *testing.T, calls string) []string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(calls); err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	time.Sleep(500 * time.Millisecond) // a second start would have landed by now
	b, _ := os.ReadFile(calls)
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

func hasJobNotice(out string) bool {
	return strings.Contains(out, "Done") || strings.Contains(out, "[1]") || strings.Contains(out, "[2]")
}

func TestLoginBlockInteractiveShellsPrintNoJobs(t *testing.T) {
	shells := [][]string{
		{"bash", "-il"},
		{"bash", "-i"}, // a terminal or tmux pane: .bashrc only
		{"dash", "-il"},
		{"zsh", "-il"},
	}
	for _, sh := range shells {
		t.Run(strings.Join(sh, " "), func(t *testing.T) {
			path, err := exec.LookPath(sh[0])
			if err != nil {
				t.Skipf("%s is not installed", sh[0])
			}
			home, calls := loginHome(t, loginBlock)
			out := runInteractive(t, home, append([]string{path}, sh[1:]...)...)
			if hasJobNotice(out) {
				t.Fatalf("job-control output in an interactive shell:\n%s", out)
			}
			got := waitCalls(t, calls)
			if len(got) != 1 || got[0] != "worker start --at-login" {
				t.Fatalf("start ran %d times: %q\n%s", len(got), got, out)
			}
		})
	}
}

// The old block shows the owner's report in the same harness, so the test
// above is known to detect it.
func TestLegacyLoginBlockPrintsJobs(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is not installed")
	}
	home, calls := loginHome(t, legacyLoginBlock)
	out := runInteractive(t, home, "bash", "-il")
	if !hasJobNotice(out) {
		t.Fatalf("expected the old block to print job notices:\n%s", out)
	}
	if got := waitCalls(t, calls); len(got) != 2 {
		t.Fatalf("expected the old block to start twice, got %q", got)
	}
}

// The guard: one start per shell, however often the block runs in it, also
// with allexport (set -a), and a child shell that inherits the exported guard
// still starts.
func TestLoginBlockGuardPerShellUnderAllexport(t *testing.T) {
	for _, sh := range []string{"sh", "bash", "dash", "zsh"} {
		t.Run(sh, func(t *testing.T) {
			path, err := exec.LookPath(sh)
			if err != nil {
				t.Skipf("%s is not installed", sh)
			}
			home, calls := loginHome(t, loginBlock)
			blk := filepath.Join(home, "block")
			if err := os.WriteFile(blk, []byte(loginBlock), 0o600); err != nil {
				t.Fatal(err)
			}
			script := `set -a; . "$HOME/block"; . "$HOME/block"; . "$HOME/block"
case "${minion_worker_login_started-}" in "$$") ;; *) echo "guard not set"; exit 3 ;; esac
env | grep -q '^minion_worker_login_started=' || { echo "allexport did not export the guard"; exit 4; }
"$SHELL_UNDER_TEST" -c '. "$HOME/block"; . "$HOME/block"'
:` // the ":" keeps bash from running the child with exec in the same process
			cmd := exec.Command(path, "-c", script)
			cmd.Env = []string{"HOME=" + home, "PATH=/usr/bin:/bin", "SHELL_UNDER_TEST=" + path}
			cmd.Dir = home
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("%v\n%s", err, out)
			}
			got := waitCalls(t, calls)
			if len(got) != 2 {
				t.Fatalf("want one start in the shell and one in its child, got %q", got)
			}
		})
	}
}

// migrateHome is a private HOME with a login registration that names state.
func migrateHome(t *testing.T) (home, state string) {
	t.Helper()
	home = t.TempDir()
	t.Setenv("HOME", home)
	state = filepath.Join(home, ".local", "state", "minion")
	writeHomeFile(t, home, ".config/minion/worker-login", "/usr/bin/minion\n"+state+"\n", 0o600)
	return home, state
}

func writeHomeFile(t *testing.T, home, name, content string, mode os.FileMode) {
	t.Helper()
	p := filepath.Join(home, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
}

func readHomeFile(t *testing.T, home, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(home, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func statT(t *testing.T, p string) *syscall.Stat_t {
	t.Helper()
	fi, err := os.Lstat(p)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Sys().(*syscall.Stat_t)
}

// otherGroup is a supplementary group of this process other than its
// primary one, or -1.
func otherGroup() int {
	groups, _ := os.Getgroups()
	for _, g := range groups {
		if g != os.Getegid() {
			return g
		}
	}
	return -1
}

func TestMigrateLoginStartupUpdatesInPlaceWithABackup(t *testing.T) {
	home, state := migrateHome(t)
	before := "# user\nalias ll='ls -l'\n"
	after := "export EDITOR=vi\n# after\n"
	orig := before + legacyLoginBlock + after
	writeHomeFile(t, home, ".bashrc", orig, 0o640)
	bashrc := filepath.Join(home, ".bashrc")
	gid := os.Getegid()
	if g := otherGroup(); g >= 0 {
		if err := os.Chown(bashrc, -1, g); err == nil {
			gid = g
		}
	}
	// A hard link (a dotfiles repository that links the file) sees the
	// update, as the inode is changed in place.
	alias := filepath.Join(home, "dotfiles-bashrc")
	if err := os.Link(bashrc, alias); err != nil {
		t.Fatal(err)
	}
	// An earlier backup is never overwritten.
	writeHomeFile(t, home, ".bashrc.minion-backup", "older backup\n", 0o600)
	before1 := statT(t, bashrc)

	// Without a login registration nothing changes; nor for another Worker
	// profile of the account (systemd, foreground).
	if err := os.Rename(loginRegistration(home), loginRegistration(home)+".off"); err != nil {
		t.Fatal(err)
	}
	if res, err := migrateLoginStartup(context.Background(), state); err != nil || len(res.Changed) != 0 {
		t.Fatalf("without registration: %+v, %v", res, err)
	}
	if err := os.Rename(loginRegistration(home)+".off", loginRegistration(home)); err != nil {
		t.Fatal(err)
	}
	if res, err := migrateLoginStartup(context.Background(), filepath.Join(home, "other")); err != nil || len(res.Changed) != 0 {
		t.Fatalf("other profile: %+v, %v", res, err)
	}
	if readHomeFile(t, home, ".bashrc") != orig {
		t.Fatal("changed without a matching registration")
	}

	res, err := migrateLoginStartup(context.Background(), state)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Changed) != 1 || res.Changed[0] != bashrc || res.Backups[0] != bashrc+".minion-backup.1" || len(res.Skipped) != 0 {
		t.Fatalf("result %+v", res)
	}
	if got := readHomeFile(t, home, ".bashrc"); got != before+loginBlock+after {
		t.Fatalf(".bashrc:\n%s", got)
	}
	now := statT(t, bashrc)
	if now.Ino != before1.Ino || now.Mode != before1.Mode || int(now.Gid) != gid || now.Nlink != 2 {
		t.Fatalf("the file was replaced or its metadata changed: before %+v, after %+v", before1, now)
	}
	if readHomeFile(t, home, "dotfiles-bashrc") != before+loginBlock+after {
		t.Fatal("the hard link does not see the update")
	}
	if readHomeFile(t, home, ".bashrc.minion-backup") != "older backup\n" {
		t.Fatal("an earlier backup was overwritten")
	}
	if readHomeFile(t, home, ".bashrc.minion-backup.1") != orig {
		t.Fatal("the backup does not hold the original bytes")
	}
	bk := statT(t, bashrc+".minion-backup.1")
	if int(bk.Gid) == gid && bk.Mode&0o777 != 0o640 || int(bk.Gid) != gid && bk.Mode&0o077 != 0 {
		t.Fatalf("backup mode %o group %d (original 640, group %d)", bk.Mode&0o777, bk.Gid, gid)
	}

	// Idempotent: the next start changes nothing and writes no backup, and
	// the installer does not append a second block.
	if res, err := migrateLoginStartup(context.Background(), state); err != nil || len(res.Changed)+len(res.Skipped) != 0 {
		t.Fatalf("second run: %+v, %v", res, err)
	}
	if _, err := os.Stat(bashrc + ".minion-backup.2"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a second backup was written")
	}
	if err := appendLoginBlock(bashrc); err != nil {
		t.Fatal(err)
	}
	if got := readHomeFile(t, home, ".bashrc"); got != before+loginBlock+after {
		t.Fatalf("installer added a block:\n%s", got)
	}
}

func TestMigrateLoginStartupChangesOnlyTheExactBlock(t *testing.T) {
	home, state := migrateHome(t)
	body := legacyLoginBlockBodies[0]
	cases := map[string]string{
		// A note after the end marker: not the complete marker line.
		".profile": "a\n" + body + " # mine\nb\n",
		// A block the user edited.
		".zprofile": strings.Replace(legacyLoginBlock, "2>&1 &", "2>&1 & disown", 1),
		// Indented, as inside an "if": not at the start of a line.
		".bash_login":   "if true; then\n  " + body + "\nfi\n",
		".bash_profile": "# no block\n",
	}
	for name, content := range cases {
		writeHomeFile(t, home, name, content, 0o600)
	}
	// Two exact copies are both updated; nothing is removed.
	two := "a\n" + legacyLoginBlock + "b\n" + legacyLoginBlock
	writeHomeFile(t, home, ".bashrc", two, 0o600)
	res, err := migrateLoginStartup(context.Background(), state)
	if err != nil || len(res.Changed) != 1 {
		t.Fatalf("%+v, %v", res, err)
	}
	if got := readHomeFile(t, home, ".bashrc"); got != "a\n"+loginBlock+"b\n"+loginBlock {
		t.Fatalf(".bashrc:\n%s", got)
	}
	for name, content := range cases {
		if readHomeFile(t, home, name) != content {
			t.Errorf("%s was changed", name)
		}
		if _, err := os.Stat(filepath.Join(home, name+loginBackupSuffix)); err == nil {
			t.Errorf("%s got a backup", name)
		}
	}
}

func TestMigrateLoginStartupLeavesSymbolicLinks(t *testing.T) {
	home, state := migrateHome(t)
	target := filepath.Join(home, "dotfiles", "profile")
	writeHomeFile(t, home, "dotfiles/profile", legacyLoginBlock, 0o644)
	if err := os.Symlink(target, filepath.Join(home, ".profile")); err != nil {
		t.Fatal(err)
	}
	// A link to a file without a block is not worth a note.
	writeHomeFile(t, home, "dotfiles/bashrc", "# plain\n", 0o644)
	if err := os.Symlink(filepath.Join(home, "dotfiles", "bashrc"), filepath.Join(home, ".bashrc")); err != nil {
		t.Fatal(err)
	}
	res, err := migrateLoginStartup(context.Background(), state)
	if err != nil || len(res.Changed) != 0 || len(res.Skipped) != 1 || !strings.Contains(res.Skipped[0], filepath.Join(home, ".profile")) {
		t.Fatalf("%+v, %v", res, err)
	}
	if readHomeFile(t, home, "dotfiles/profile") != legacyLoginBlock {
		t.Fatal("the link target was changed")
	}
	if fi, _ := os.Lstat(filepath.Join(home, ".profile")); fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("the link was replaced")
	}
	entries, _ := os.ReadDir(home)
	for _, e := range entries {
		if strings.Contains(e.Name(), ".minion-") {
			t.Fatalf("left %s", e.Name())
		}
	}
	// The installer does not add a second block behind the link either.
	if err := appendLoginBlock(filepath.Join(home, ".profile")); err != nil && os.Geteuid() != 0 {
		t.Fatal(err)
	}
	if readHomeFile(t, home, "dotfiles/profile") != legacyLoginBlock {
		t.Fatal("the installer appended a second block")
	}
}

// An editor that saves the file while the update runs wins: its save is kept,
// and no backup is left behind.
func TestMigrateLoginStartupSkipsAConcurrentChange(t *testing.T) {
	for name, save := range map[string]func(t *testing.T, path string){
		"in place": func(t *testing.T, path string) {
			if err := os.WriteFile(path, []byte("# saved by the editor\n"+legacyLoginBlock), 0o600); err != nil {
				t.Error(err)
			}
		},
		"same size": func(t *testing.T, path string) {
			b, _ := os.ReadFile(path)
			b[0] = 'X'
			f, err := os.OpenFile(path, os.O_WRONLY, 0)
			if err != nil {
				t.Error(err)
				return
			}
			_, _ = f.WriteAt(b, 0)
			_ = f.Close()
		},
		"renamed over": func(t *testing.T, path string) {
			tmp := path + ".swp"
			if err := os.WriteFile(tmp, []byte("# saved by the editor\n"+legacyLoginBlock), 0o600); err != nil {
				t.Error(err)
			}
			if err := os.Rename(tmp, path); err != nil {
				t.Error(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			home, state := migrateHome(t)
			writeHomeFile(t, home, ".bashrc", "#\n"+legacyLoginBlock, 0o600)
			path := filepath.Join(home, ".bashrc")
			var saved string
			loginBeforeCommit = func(p string) {
				if p == path {
					save(t, p)
					saved = readHomeFile(t, home, ".bashrc")
				}
			}
			t.Cleanup(func() { loginBeforeCommit = nil })
			res, err := migrateLoginStartup(context.Background(), state)
			if err != nil || len(res.Changed) != 0 || len(res.Skipped) != 1 {
				t.Fatalf("%+v, %v", res, err)
			}
			if got := readHomeFile(t, home, ".bashrc"); got != saved {
				t.Fatalf("the editor's save was overwritten:\n%s", got)
			}
			if _, err := os.Stat(path + loginBackupSuffix); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("a backup was left behind")
			}
		})
	}
}

func TestLoginRegistrationIsReadOnlyWhenPrivateAndValid(t *testing.T) {
	home := t.TempDir()
	reg := filepath.Join(home, "worker-login")
	state := "/srv/state"
	valid := "/usr/bin/minion\n" + state + "\n"
	cases := map[string]func(t *testing.T){
		"group readable": func(t *testing.T) { writeHomeFile(t, home, "worker-login", valid, 0o640) },
		"symbolic link": func(t *testing.T) {
			writeHomeFile(t, home, "real", valid, 0o600)
			if err := os.Symlink(filepath.Join(home, "real"), reg); err != nil {
				t.Fatal(err)
			}
		},
		"fifo": func(t *testing.T) {
			if err := unix.Mkfifo(reg, 0o600); err != nil {
				t.Fatal(err)
			}
		},
		"oversized": func(t *testing.T) {
			writeHomeFile(t, home, "worker-login", valid+strings.Repeat("x", loginRegistrationMax), 0o600)
		},
		"one line":      func(t *testing.T) { writeHomeFile(t, home, "worker-login", "/usr/bin/minion\n", 0o600) },
		"relative":      func(t *testing.T) { writeHomeFile(t, home, "worker-login", "minion\n"+state+"\n", 0o600) },
		"three lines":   func(t *testing.T) { writeHomeFile(t, home, "worker-login", valid+"/x\n", 0o600) },
		"carriage ret.": func(t *testing.T) { writeHomeFile(t, home, "worker-login", "/usr/bin/minion\r\n"+state+"\n", 0o600) },
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			_ = os.Remove(reg)
			setup(t)
			done := make(chan bool, 1)
			go func() { done <- registeredLoginState(reg, state) }()
			select {
			case ok := <-done:
				if ok {
					t.Fatal("accepted")
				}
			case <-time.After(5 * time.Second):
				t.Fatal("blocked")
			}
			if _, _, err := readLoginRegistration(reg); err == nil {
				t.Fatal("readLoginRegistration accepted it")
			}
		})
	}
	_ = os.Remove(reg)
	writeHomeFile(t, home, "worker-login", valid, 0o600)
	if !registeredLoginState(reg, state) || !registeredLoginState(reg, state+"/") || registeredLoginState(reg, "/srv/other") {
		t.Fatal("a valid registration is not matched")
	}
}

// The installer's update of earlier blocks is best effort: a file it cannot
// read is reported and the others are still updated.
func TestMigrateLoginBlocksLockedContinuesPastAFailure(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a file without permissions")
	}
	home, _ := migrateHome(t)
	writeHomeFile(t, home, ".bash_login", legacyLoginBlock, 0o000)
	writeHomeFile(t, home, ".bashrc", legacyLoginBlock, 0o600)
	res, err := migrateLoginBlocksLocked(home)
	if err == nil || !strings.Contains(err.Error(), ".bash_login") || len(res.Changed) != 1 || res.Changed[0] != filepath.Join(home, ".bashrc") {
		t.Fatalf("%+v, %v", res, err)
	}
	var out bytes.Buffer
	reportLoginMigration(&out, res, err)
	if !strings.Contains(out.String(), "Updated the Worker login startup block in "+filepath.Join(home, ".bashrc")) || !strings.Contains(out.String(), "Warning: could not update") {
		t.Fatalf("report:\n%s", out.String())
	}
}

// The Worker updates the blocks only once it started, and joins the update
// before it returns; a failed start changes nothing.
func TestWorkerRunMigratesLoginBlocksOnlyAfterAStart(t *testing.T) {
	t.Run("failed start", func(t *testing.T) {
		home, state := migrateHome(t)
		writeHomeFile(t, home, ".bashrc", legacyLoginBlock, 0o600)
		reg := agent.NewRegistry()
		if err := reg.Register("broken", agent.OrderCore, func(context.Context, *agent.Env) (agent.StopFunc, error) {
			return nil, errors.New("the control socket is in use")
		}); err != nil {
			t.Fatal(err)
		}
		called := false
		deps := workerRunDeps{stderr: &bytes.Buffer{}, lookupEnv: envFrom(map[string]string{"MINION_STATE_DIR": state}), loadConfig: loadWorkerConfig, registry: reg,
			migrateLogin: func(ctx context.Context, dir string) (loginMigration, error) {
				called = true
				return migrateLoginStartup(ctx, dir)
			}}
		root := workerRoot(deps)
		root.SetArgs([]string{"worker", "run"})
		root.SetOut(io.Discard)
		root.SetErr(io.Discard)
		if err := root.ExecuteContext(context.Background()); err == nil {
			t.Fatal("expected the start to fail")
		}
		time.Sleep(200 * time.Millisecond)
		if called || readHomeFile(t, home, ".bashrc") != legacyLoginBlock {
			t.Fatal("a failed start updated the login blocks")
		}
	})
	t.Run("started", func(t *testing.T) {
		home, state := migrateHome(t)
		writeHomeFile(t, home, ".bashrc", legacyLoginBlock, 0o600)
		deps, p := probeOnlyDeps(t, map[string]string{"MINION_STATE_DIR": state})
		migrated := make(chan struct{})
		var ended bool
		deps.migrateLogin = func(ctx context.Context, dir string) (loginMigration, error) {
			res, err := migrateLoginStartup(ctx, dir)
			close(migrated)
			<-ctx.Done() // still running when the Worker stops
			ended = true
			return res, err
		}
		cancel, wait := startWorker(t, deps, p)
		select {
		case <-migrated:
		case <-time.After(10 * time.Second):
			t.Fatal("the started Worker did not update the login blocks")
		}
		if readHomeFile(t, home, ".bashrc") != loginBlock {
			t.Fatal("not updated")
		}
		cancel()
		if err := wait(); err != nil {
			t.Fatal(err)
		}
		if !ended {
			t.Fatal("the update was not joined before the Worker returned")
		}
	})
}
