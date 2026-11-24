//go:build linux

package cli

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
if [ -z "${minion_worker_login_started-}" ] && [ -r "$HOME/.config/minion/worker-login" ]; then
    minion_worker_login_started=1
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

func TestMigrateLoginStartupRewritesOnlyTheLegacyBlock(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	state := filepath.Join(home, ".local", "state", "minion")
	write := func(name, content string, mode os.FileMode) {
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
	read := func(name string) string {
		t.Helper()
		b, err := os.ReadFile(filepath.Join(home, name))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	before := "# user\nalias ll='ls -l'\n"
	after := "export EDITOR=vi\n# after\n"
	write(".bashrc", before+legacyLoginBlock+after, 0o640)
	// .profile is a link into a dotfiles repository.
	write("dotfiles/profile", before+legacyLoginBlock, 0o644)
	if err := os.Symlink(filepath.Join(home, "dotfiles", "profile"), filepath.Join(home, ".profile")); err != nil {
		t.Fatal(err)
	}
	// A block the user edited is not the managed text: left alone.
	edited := strings.Replace(legacyLoginBlock, "2>&1 &", "2>&1 & disown", 1)
	write(".zprofile", edited, 0o600)
	write(".bash_login", "# no block\n", 0o600)

	// Without a login registration nothing changes.
	if changed, err := migrateLoginStartup(context.Background(), state); err != nil || changed != nil {
		t.Fatalf("without registration: %v, %v", changed, err)
	}
	if read(".bashrc") != before+legacyLoginBlock+after {
		t.Fatal("changed without registration")
	}
	write(".config/minion/worker-login", "/usr/bin/minion\n"+state+"\n", 0o600)
	// Another Worker profile of the account (systemd, foreground) leaves
	// the files alone.
	if changed, err := migrateLoginStartup(context.Background(), filepath.Join(home, "other")); err != nil || changed != nil {
		t.Fatalf("other profile: %v, %v", changed, err)
	}
	if read(".bashrc") != before+legacyLoginBlock+after {
		t.Fatal("changed by another Worker profile")
	}

	changed, err := migrateLoginStartup(context.Background(), state)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(changed, ",") != filepath.Join(home, ".profile")+","+filepath.Join(home, ".bashrc") {
		t.Fatalf("changed = %q", changed)
	}
	if got := read(".bashrc"); got != before+loginBlock+after {
		t.Fatalf(".bashrc:\n%s", got)
	}
	if fi, _ := os.Stat(filepath.Join(home, ".bashrc")); fi.Mode().Perm() != 0o640 {
		t.Fatalf("mode = %v", fi.Mode())
	}
	if fi, _ := os.Lstat(filepath.Join(home, ".profile")); fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("the .profile link was replaced by a file")
	}
	if got := read("dotfiles/profile"); got != before+loginBlock {
		t.Fatalf("profile:\n%s", got)
	}
	if read(".zprofile") != edited || read(".bash_login") != "# no block\n" {
		t.Fatal("touched a file without the managed legacy block")
	}
	entries, _ := os.ReadDir(home)
	for _, e := range entries {
		if strings.Contains(e.Name(), ".minion-") {
			t.Fatalf("temporary file left: %s", e.Name())
		}
	}
	// Idempotent: the second start changes nothing, and the installer does
	// not append a second block.
	if changed, err := migrateLoginStartup(context.Background(), state); err != nil || len(changed) != 0 {
		t.Fatalf("second run: %v, %v", changed, err)
	}
	if err := appendLoginBlock(filepath.Join(home, ".bashrc")); err != nil {
		t.Fatal(err)
	}
	if got := read(".bashrc"); got != before+loginBlock+after {
		t.Fatalf("installer added a block:\n%s", got)
	}
}

func TestReplaceLegacyLoginBlocksKeepsOneBlock(t *testing.T) {
	in := "a\n" + legacyLoginBlock + "b\n" + legacyLoginBlock + "c\n"
	got, ok := replaceLegacyLoginBlocks(in)
	if !ok || strings.Count(got, loginBlockBody) != 1 || !strings.HasPrefix(got, "a\n"+loginBlock+"b\n") || !strings.HasSuffix(got, "c\n") {
		t.Fatalf("got %q", got)
	}
	in = "a\n" + loginBlock + legacyLoginBlock
	if got, ok = replaceLegacyLoginBlocks(in); !ok || got != "a\n"+loginBlock+"\n" {
		t.Fatalf("got %q", got)
	}
	if _, ok := replaceLegacyLoginBlocks("a\n" + loginBlock); ok {
		t.Fatal("current block reported as legacy")
	}
}
