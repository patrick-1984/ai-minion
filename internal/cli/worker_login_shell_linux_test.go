//go:build linux

package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
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

// noStaged fails when the update left a staged copy in dir.
func noStaged(t *testing.T, dir string) {
	t.Helper()
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.Contains(e.Name(), loginStagedSuffix) {
			t.Fatalf("left %s", e.Name())
		}
	}
}

func TestMigrateLoginStartupReplacesAtomicallyWithABackup(t *testing.T) {
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
	// An earlier backup is never overwritten.
	writeHomeFile(t, home, ".bashrc.minion-backup", "older backup\n", 0o600)
	before1 := statT(t, bashrc)

	// Without a login registration nothing changes; nor for another Worker
	// profile of the account (systemd, foreground).
	if err := os.Rename(loginRegistration(home), loginRegistration(home)+".off"); err != nil {
		t.Fatal(err)
	}
	if res, err := migrateLoginStartup(context.Background(), state, nil); err != nil || len(res.Changed) != 0 {
		t.Fatalf("without registration: %+v, %v", res, err)
	}
	if err := os.Rename(loginRegistration(home)+".off", loginRegistration(home)); err != nil {
		t.Fatal(err)
	}
	if res, err := migrateLoginStartup(context.Background(), filepath.Join(home, "other"), nil); err != nil || len(res.Changed) != 0 {
		t.Fatalf("other profile: %+v, %v", res, err)
	}
	if readHomeFile(t, home, ".bashrc") != orig {
		t.Fatal("changed without a matching registration")
	}

	res, err := migrateLoginStartup(context.Background(), state, &loginGate{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Changed) != 1 || res.Changed[0] != bashrc || res.Backups[0] != bashrc+".minion-backup.1" || len(res.Skipped)+len(res.Pending) != 0 {
		t.Fatalf("result %+v", res)
	}
	if got := readHomeFile(t, home, ".bashrc"); got != before+loginBlock+after {
		t.Fatalf(".bashrc:\n%s", got)
	}
	// A new file with the owner, group and mode of the original.
	now := statT(t, bashrc)
	if now.Ino == before1.Ino || now.Mode != before1.Mode || int(now.Gid) != gid || now.Uid != before1.Uid || now.Nlink != 1 {
		t.Fatalf("metadata changed: before %+v, after %+v", before1, now)
	}
	if readHomeFile(t, home, ".bashrc.minion-backup") != "older backup\n" {
		t.Fatal("an earlier backup was overwritten")
	}
	if readHomeFile(t, home, ".bashrc.minion-backup.1") != orig {
		t.Fatal("the backup does not hold the original bytes")
	}
	// The original gives its group access: the backup is an owner-only copy.
	if bk := statT(t, bashrc+".minion-backup.1"); bk.Mode&0o7777 != 0o600 || bk.Ino == before1.Ino {
		t.Fatalf("backup mode %o (want an owner-only copy)", bk.Mode&0o7777)
	}
	noStaged(t, home)

	// Idempotent: the next start changes nothing and writes no backup, and
	// the installer does not append a second block.
	if res, err := migrateLoginStartup(context.Background(), state, nil); err != nil || len(res.Changed)+len(res.Skipped) != 0 {
		t.Fatalf("second run: %+v, %v", res, err)
	}
	if _, err := os.Stat(bashrc + ".minion-backup.2"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a second backup was written")
	}
	if legacy, err := appendLoginBlock(bashrc); err != nil || legacy {
		t.Fatal(legacy, err)
	}
	if got := readHomeFile(t, home, ".bashrc"); got != before+loginBlock+after {
		t.Fatalf("installer added a block:\n%s", got)
	}
}

// A 0600 file keeps its displaced inode as the backup (a hard link): a save
// in place right before the rename is in the backup, never lost.
func TestMigrateLoginStartupBackupIsTheDisplacedVersion(t *testing.T) {
	home, state := migrateHome(t)
	orig := "#\n" + legacyLoginBlock
	writeHomeFile(t, home, ".profile", orig, 0o600)
	path := filepath.Join(home, ".profile")
	ino := statT(t, path).Ino
	late := "# saved by the editor at the last moment\n" + legacyLoginBlock
	loginBeforeRename = func(p string) {
		if p == path {
			if err := os.WriteFile(p, []byte(late), 0o600); err != nil {
				t.Error(err)
			}
		}
	}
	t.Cleanup(func() { loginBeforeRename = nil })
	res, err := migrateLoginStartup(context.Background(), state, &loginGate{})
	if err != nil || len(res.Changed) != 1 || len(res.Skipped) != 1 || !strings.Contains(res.Skipped[0], "that save is in "+path+loginBackupSuffix) {
		t.Fatalf("%+v, %v", res, err)
	}
	if readHomeFile(t, home, ".profile") != "#\n"+loginBlock {
		t.Fatal("not updated")
	}
	bk := statT(t, path+loginBackupSuffix)
	if bk.Ino != ino || bk.Mode&0o7777 != 0o600 || bk.Nlink != 1 {
		t.Fatalf("backup %+v: want the displaced inode %d, mode 0600", bk, ino)
	}
	if readHomeFile(t, home, ".profile"+loginBackupSuffix) != late {
		t.Fatal("the backup does not hold the displaced version")
	}
	noStaged(t, home)
}

// A shell that reads the file while it is updated sees the old or the new
// version, never a part of one.
func TestMigrateLoginStartupDashNeverSeesAPartialFile(t *testing.T) {
	dash, err := exec.LookPath("dash")
	if err != nil {
		t.Skip("dash is not installed")
	}
	home, _ := migrateHome(t)
	// Large enough that an in-place write would be seen half done.
	pad := strings.Repeat("# padding line of a long profile\n", 2000)
	legacy := "echo start\n" + pad + legacyLoginBlock + pad + "echo end\n"
	path := filepath.Join(home, ".profile")
	stage := filepath.Join(home, "staged-profile")
	// The shell runs with another HOME: the blocks start nothing.
	shellHome := t.TempDir()
	stop := make(chan struct{})
	bad := make(chan string, 1)
	var runs int
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			cmd := exec.Command(dash, "-c", ". \"$1\"", "dash", path)
			cmd.Env = []string{"HOME=" + shellHome, "PATH=/usr/bin:/bin"}
			out, err := cmd.CombinedOutput()
			runs++
			if err != nil || string(out) != "start\nend\n" {
				select {
				case bad <- fmt.Sprintf("%v: %q", err, out):
				default:
				}
				return
			}
		}
	}()
	deadline := time.Now().Add(3 * time.Second)
	for i := 0; time.Now().Before(deadline) || i < 20; i++ {
		// The legacy version is put in place atomically too.
		if err := os.WriteFile(stage, []byte(legacy), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(stage, path); err != nil {
			t.Fatal(err)
		}
		res, err := migrateLoginBlocksLocked(context.Background(), nil, home)
		if err != nil || len(res.Changed) != 1 {
			t.Fatalf("%+v, %v", res, err)
		}
		for _, b := range res.Backups {
			_ = os.Remove(b)
		}
		select {
		case msg := <-bad:
			t.Fatalf("dash saw a partial profile: %s", msg)
		default:
		}
	}
	close(stop)
	<-done
	select {
	case msg := <-bad:
		t.Fatalf("dash saw a partial profile: %s", msg)
	default:
	}
	if runs == 0 {
		t.Fatal("dash never ran")
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
	res, err := migrateLoginStartup(context.Background(), state, nil)
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

// Files whose metadata a new version cannot keep are left as they are, with
// a repair the user can follow.
func TestMigrateLoginStartupSkipsFilesItCannotReplaceSafely(t *testing.T) {
	cases := map[string]func(t *testing.T, home, path string){
		"hard link": func(t *testing.T, home, path string) {
			if err := os.Link(path, filepath.Join(home, "dotfiles-profile")); err != nil {
				t.Fatal(err)
			}
		},
		// No setfacl on every test host: the ACL is shown by the listing of
		// the extended attributes, as the file system would report it.
		"acl": func(t *testing.T, home, path string) {
			old := loginXattrs
			loginXattrs = func(int) ([]string, error) { return []string{"system.posix_acl_access"}, nil }
			t.Cleanup(func() { loginXattrs = old })
		},
	}
	if _, err := exec.LookPath("setfacl"); err == nil {
		cases["acl (setfacl)"] = func(t *testing.T, home, path string) {
			if out, err := exec.Command("setfacl", "-m", "u:65534:r", path).CombinedOutput(); err != nil {
				t.Skipf("setfacl: %v: %s", err, out)
			}
		}
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			home, state := migrateHome(t)
			orig := "#\n" + legacyLoginBlock
			writeHomeFile(t, home, ".profile", orig, 0o640)
			path := filepath.Join(home, ".profile")
			setup(t, home, path)
			ino := statT(t, path).Ino
			res, err := migrateLoginStartup(context.Background(), state, nil)
			if err != nil || len(res.Changed) != 0 || len(res.Skipped) != 1 || len(res.Pending) != 1 || res.Pending[0] != path {
				t.Fatalf("%+v, %v", res, err)
			}
			if !strings.Contains(res.Skipped[0], "To repair it, delete the lines from \"# Minion Worker login startup\" through \"# End Minion Worker login startup\" in "+path) {
				t.Fatalf("no repair: %s", res.Skipped[0])
			}
			if readHomeFile(t, home, ".profile") != orig || statT(t, path).Ino != ino {
				t.Fatal("the file was changed")
			}
			if _, err := os.Stat(path + loginBackupSuffix); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("a backup was written")
			}
			noStaged(t, home)
		})
	}
}

// A symbolic link is kept and the target it points to updated, when only
// this user controls the path to it.
func TestMigrateLoginStartupUpdatesTheTargetOfALink(t *testing.T) {
	home, state := migrateHome(t)
	target := filepath.Join(home, "dotfiles", "profile")
	writeHomeFile(t, home, "dotfiles/profile", "#\n"+legacyLoginBlock, 0o644)
	link := filepath.Join(home, ".profile")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	// A link to a file without a block is not worth a note.
	writeHomeFile(t, home, "dotfiles/bashrc", "# plain\n", 0o644)
	if err := os.Symlink(filepath.Join(home, "dotfiles", "bashrc"), filepath.Join(home, ".bashrc")); err != nil {
		t.Fatal(err)
	}
	res, err := migrateLoginStartup(context.Background(), state, &loginGate{})
	if err != nil || len(res.Changed) != 1 || res.Changed[0] != target || res.Backups[0] != target+loginBackupSuffix || len(res.Skipped) != 0 {
		t.Fatalf("%+v, %v", res, err)
	}
	if readHomeFile(t, home, "dotfiles/profile") != "#\n"+loginBlock {
		t.Fatal("the link target was not updated")
	}
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("the link was replaced")
	}
	if got, _ := os.Readlink(link); got != target {
		t.Fatalf("the link points to %s", got)
	}
	if st := statT(t, target); st.Mode&0o7777 != 0o644 {
		t.Fatalf("target mode %o", st.Mode&0o7777)
	}
	if st := statT(t, target+loginBackupSuffix); st.Mode&0o7777 != 0o600 || readHomeFile(t, home, "dotfiles/profile"+loginBackupSuffix) != "#\n"+legacyLoginBlock {
		t.Fatal("the backup is not an owner-only copy of the original")
	}
	noStaged(t, filepath.Join(home, "dotfiles"))
	if readHomeFile(t, home, "dotfiles/bashrc") != "# plain\n" {
		t.Fatal("a target without a block was changed")
	}
}

// A link whose target others can change is not written through: the update
// and "worker up" report it with the repair instead of treating the old
// block as installed.
func TestUnmigratableLinkIsReportedByWorkerUp(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root's installer refuses links")
	}
	home, state := migrateHome(t)
	shared := filepath.Join(home, "shared")
	if err := os.MkdirAll(shared, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(shared, 0o777); err != nil { // writable by others, not sticky
		t.Fatal(err)
	}
	target := filepath.Join(shared, "profile")
	writeHomeFile(t, home, "shared/profile", legacyLoginBlock, 0o600)
	link := filepath.Join(home, ".profile")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	res, err := migrateLoginStartup(context.Background(), state, nil)
	if err != nil || len(res.Changed) != 0 || len(res.Pending) != 1 || res.Pending[0] != link || !strings.Contains(res.Skipped[0], "other users can change "+shared) || !strings.Contains(res.Skipped[0], "in "+target+" (the target of the symbolic link "+link+")") {
		t.Fatalf("%+v, %v", res, err)
	}
	var out bytes.Buffer
	if err := installLoginBlocks(context.Background(), home, []string{".profile", ".bashrc"}, &out); err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(out.String(), "Warning: "+link+" still holds the login block of an earlier release"); n != 1 {
		t.Fatalf("worker up reported it %d times:\n%s", n, out.String())
	}
	if readHomeFile(t, home, "shared/profile") != legacyLoginBlock {
		t.Fatal("the target was changed")
	}
	if readHomeFile(t, home, ".bashrc") != loginBlock {
		t.Fatal("the other profile did not get the block")
	}
	// A legacy block left for another reason (here a hard link) is reported
	// by worker up as well.
	_ = os.Remove(link)
	writeHomeFile(t, home, ".profile", legacyLoginBlock, 0o600)
	if err := os.Link(filepath.Join(home, ".profile"), filepath.Join(home, "profile-link")); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := installLoginBlocks(context.Background(), home, []string{".profile"}, &out); err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(out.String(), "Warning: "+filepath.Join(home, ".profile")+" still holds"); n != 1 {
		t.Fatalf("worker up reported it %d times:\n%s", n, out.String())
	}
}

// An editor that saves the file while the update runs wins: its save is kept,
// and no backup or staged copy is left behind.
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
			res, err := migrateLoginStartup(context.Background(), state, &loginGate{})
			if err != nil || len(res.Changed) != 0 || len(res.Skipped) != 1 {
				t.Fatalf("%+v, %v", res, err)
			}
			if got := readHomeFile(t, home, ".bashrc"); got != saved {
				t.Fatalf("the editor's save was overwritten:\n%s", got)
			}
			if _, err := os.Stat(path + loginBackupSuffix); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("a backup was left behind")
			}
			noStaged(t, home)
		})
	}
}

// Nothing is published once the Worker's lifecycle ended: neither after the
// gate was revoked nor after the context ended, and the update stops before
// the next file.
func TestMigrateLoginStartupPublishesNothingAfterTheLifecycle(t *testing.T) {
	t.Run("revoked", func(t *testing.T) {
		home, state := migrateHome(t)
		writeHomeFile(t, home, ".profile", legacyLoginBlock, 0o600)
		writeHomeFile(t, home, ".bashrc", legacyLoginBlock, 0o600)
		gate := &loginGate{}
		path := filepath.Join(home, ".profile")
		loginBeforeCommit = func(p string) {
			if p == path {
				gate.revoke()
			}
		}
		t.Cleanup(func() { loginBeforeCommit = nil })
		res, err := migrateLoginStartup(context.Background(), state, gate)
		if !errors.Is(err, errLoginRevoked) || len(res.Changed) != 0 {
			t.Fatalf("%+v, %v", res, err)
		}
		if readHomeFile(t, home, ".profile") != legacyLoginBlock || readHomeFile(t, home, ".bashrc") != legacyLoginBlock {
			t.Fatal("published after the revocation")
		}
		noStaged(t, home)
	})
	t.Run("cancelled", func(t *testing.T) {
		home, state := migrateHome(t)
		writeHomeFile(t, home, ".profile", legacyLoginBlock, 0o600)
		writeHomeFile(t, home, ".bashrc", legacyLoginBlock, 0o600)
		ctx, cancel := context.WithCancel(context.Background())
		path := filepath.Join(home, ".profile")
		loginBeforeCommit = func(p string) {
			if p == path {
				cancel()
			}
		}
		t.Cleanup(func() { loginBeforeCommit = nil; cancel() })
		res, err := migrateLoginStartup(ctx, state, &loginGate{})
		if !errors.Is(err, context.Canceled) || len(res.Changed) != 0 {
			t.Fatalf("%+v, %v", res, err)
		}
		if readHomeFile(t, home, ".profile") != legacyLoginBlock || readHomeFile(t, home, ".bashrc") != legacyLoginBlock {
			t.Fatal("published after the cancellation")
		}
		noStaged(t, home)
	})
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
	res, err := migrateLoginBlocksLocked(context.Background(), nil, home)
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
			migrateLogin: func(ctx context.Context, dir string, gate *loginGate) (loginMigration, error) {
				called = true
				return migrateLoginStartup(ctx, dir, gate)
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
		var held *loginGate
		deps.migrateLogin = func(ctx context.Context, dir string, gate *loginGate) (loginMigration, error) {
			held = gate
			res, err := migrateLoginStartup(ctx, dir, gate)
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
		// An update that outlives the Worker (one that ignores its context)
		// cannot publish: the gate was revoked before the lifecycle lock
		// was released.
		writeHomeFile(t, home, ".bashrc", legacyLoginBlock, 0o600)
		if _, err := migrateLoginStartup(context.Background(), state, held); !errors.Is(err, errLoginRevoked) {
			t.Fatalf("after the Worker stopped: %v", err)
		}
		if readHomeFile(t, home, ".bashrc") != legacyLoginBlock {
			t.Fatal("published after the Worker released its lifecycle lock")
		}
		noStaged(t, home)
	})
}
