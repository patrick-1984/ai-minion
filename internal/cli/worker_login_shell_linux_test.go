//go:build linux

package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
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

// migrateHome is a private HOME (also for os.UserHomeDir) with a login
// registration.
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

// noLeftovers fails when the update left a staged copy or a backup in dir.
func noLeftovers(t *testing.T, dir string, backups bool) {
	t.Helper()
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.Contains(e.Name(), loginStagedSuffix) || backups && strings.Contains(e.Name(), loginBackupSuffix) {
			t.Fatalf("left %s", e.Name())
		}
	}
}

// fileSnapshot is the bytes and inode of a file, to prove it was not touched.
type fileSnapshot struct {
	data  string
	ino   uint64
	mtime syscall.Timespec
}

func snapshot(t *testing.T, p string) fileSnapshot {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	st := statT(t, p)
	return fileSnapshot{string(b), st.Ino, st.Mtim}
}

// "worker up" replaces the exact block of an earlier release in a plain file
// atomically: a new inode with the group and mode of the original, and an
// owner-only backup of the displaced version.
func TestWorkerUpMigratesAPlainFileAtomicallyWithABackup(t *testing.T) {
	for _, mode := range []os.FileMode{0o644, 0o600, 0o640} {
		t.Run(fmt.Sprintf("%04o", mode), func(t *testing.T) {
			home, _ := migrateHome(t)
			before := "# user\nalias ll='ls -l'\n"
			after := "export EDITOR=vi\n# after\n"
			orig := before + legacyLoginBlock + after
			writeHomeFile(t, home, ".bashrc", orig, mode)
			bashrc := filepath.Join(home, ".bashrc")
			gid := os.Getegid()
			if g := otherGroup(); g >= 0 && mode == 0o640 {
				if err := os.Chown(bashrc, -1, g); err == nil {
					gid = g
				}
			}
			// An earlier backup is never overwritten.
			writeHomeFile(t, home, ".bashrc.minion-backup", "older backup\n", 0o600)
			st0 := statT(t, bashrc)

			var out bytes.Buffer
			if err := installLoginBlocks(context.Background(), home, []string{".profile", ".bashrc"}, &out); err != nil {
				t.Fatal(err)
			}
			backup := bashrc + ".minion-backup.1"
			if !strings.Contains(out.String(), "Updated the Worker login startup block in "+bashrc+" (the previous file is in "+backup+").") || strings.Contains(out.String(), "Warning") {
				t.Fatalf("report:\n%s", out.String())
			}
			if got := readHomeFile(t, home, ".bashrc"); got != before+loginBlock+after {
				t.Fatalf(".bashrc:\n%s", got)
			}
			now := statT(t, bashrc)
			if now.Ino == st0.Ino || now.Mode != st0.Mode || int(now.Gid) != gid || now.Uid != st0.Uid || now.Nlink != 1 {
				t.Fatalf("metadata: before %+v, after %+v", st0, now)
			}
			if readHomeFile(t, home, ".bashrc.minion-backup") != "older backup\n" {
				t.Fatal("an earlier backup was overwritten")
			}
			if readHomeFile(t, home, ".bashrc.minion-backup.1") != orig {
				t.Fatal("the backup does not hold the original bytes")
			}
			bk := statT(t, backup)
			if bk.Mode&0o7777 != 0o600 || bk.Nlink != 1 {
				t.Fatalf("backup mode %o, %d links", bk.Mode&0o7777, bk.Nlink)
			}
			// A 0600 file keeps its displaced inode as the backup.
			if mode == 0o600 && bk.Ino != st0.Ino {
				t.Fatal("the 0600 backup is not the displaced inode")
			}
			if readHomeFile(t, home, ".profile") != loginBlock {
				t.Fatal(".profile did not get the block")
			}
			noLeftovers(t, home, false)

			// Idempotent: no second backup, no second block.
			out.Reset()
			if err := installLoginBlocks(context.Background(), home, []string{".profile", ".bashrc"}, &out); err != nil || out.Len() != 0 {
				t.Fatalf("second run: %v\n%s", err, out.String())
			}
			if _, err := os.Stat(bashrc + ".minion-backup.2"); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("a second backup was written")
			}
			if got := readHomeFile(t, home, ".bashrc"); got != before+loginBlock+after {
				t.Fatalf("second run:\n%s", got)
			}
		})
	}
}

// A file the update cannot replace safely is left as it is, byte for byte
// and inode, with a one-line repair; "worker up" still succeeds.
func TestWorkerUpSkipsFilesItCannotReplaceSafely(t *testing.T) {
	cases := map[string]func(t *testing.T, home, path string){
		"symlink": func(t *testing.T, home, path string) {
			target := filepath.Join(home, "dotfiles-profile")
			if err := os.Rename(path, target); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, path); err != nil {
				t.Fatal(err)
			}
		},
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
		"user xattr": func(t *testing.T, home, path string) {
			if err := unix.Setxattr(path, "user.minion.test", []byte("1"), 0); err != nil {
				t.Skipf("no user xattrs here: %v", err)
			}
		},
		"default acl on the directory": func(t *testing.T, home, path string) {
			old := loginDefaultACL
			loginDefaultACL = func(int) (bool, error) { return true, nil }
			t.Cleanup(func() { loginDefaultACL = old })
		},
		"xattrs unreadable": func(t *testing.T, home, path string) {
			old := loginXattrs
			loginXattrs = func(int) ([]string, error) { return nil, unix.EIO }
			t.Cleanup(func() { loginXattrs = old })
		},
	}
	if os.Geteuid() == 0 {
		cases["another user"] = func(t *testing.T, home, path string) {
			if err := os.Chown(path, 65534, 65534); err != nil {
				t.Fatal(err)
			}
		}
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
			home, _ := migrateHome(t)
			orig := "#\n" + legacyLoginBlock
			writeHomeFile(t, home, ".profile", orig, 0o644)
			path := filepath.Join(home, ".profile")
			setup(t, home, path)
			snap := snapshot(t, path)
			res, err := migrateLoginBlocks(context.Background(), home)
			if err != nil || len(res.Changed) != 0 || len(res.Skipped) != 1 || len(res.Pending) != 1 || res.Pending[0] != path {
				t.Fatalf("%+v, %v", res, err)
			}
			if strings.Contains(res.Skipped[0], "\n") || !strings.Contains(res.Skipped[0], "To repair it, delete the lines from \"# Minion Worker login startup\" through \"# End Minion Worker login startup\" in "+path) {
				t.Fatalf("no one-line repair: %s", res.Skipped[0])
			}
			if snapshot(t, path) != snap {
				t.Fatal("the file was changed")
			}
			noLeftovers(t, home, true)
			// "worker up" reports it once and still installs the others.
			var out bytes.Buffer
			if err := installLoginBlocks(context.Background(), home, []string{".profile", ".bashrc"}, &out); err != nil {
				t.Fatal(err)
			}
			if n := strings.Count(out.String(), "Warning: "+path+" still holds the login block of an earlier release"); n != 1 {
				t.Fatalf("reported %d times:\n%s", n, out.String())
			}
			if snapshot(t, path) != snap {
				t.Fatal("worker up changed the file")
			}
			if readHomeFile(t, home, ".bashrc") != loginBlock {
				t.Fatal("the other profile did not get the block")
			}
		})
	}
}

// A file that changes between the read and the rename is left with the
// change: owner, group, mode, size, mtime or content; nothing partial and
// no backup is left behind.
func TestWorkerUpSkipsAConcurrentChange(t *testing.T) {
	changes := map[string]func(t *testing.T, path string){
		"in place": func(t *testing.T, path string) {
			if err := os.WriteFile(path, []byte("# saved by the editor\n"+legacyLoginBlock), 0o644); err != nil {
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
			if err := os.WriteFile(tmp, []byte("# saved by the editor\n"+legacyLoginBlock), 0o644); err != nil {
				t.Error(err)
			}
			if err := os.Rename(tmp, path); err != nil {
				t.Error(err)
			}
		},
		"mode": func(t *testing.T, path string) {
			if err := os.Chmod(path, 0o600); err != nil {
				t.Error(err)
			}
		},
		"mtime": func(t *testing.T, path string) {
			at := time.Now().Add(-time.Hour)
			if err := os.Chtimes(path, at, at); err != nil {
				t.Error(err)
			}
		},
		"xattr": func(t *testing.T, path string) {
			if err := unix.Setxattr(path, "user.minion.test", []byte("1"), 0); err != nil {
				t.Skipf("no user xattrs here: %v", err)
			}
		},
	}
	if g := otherGroup(); g >= 0 {
		changes["group"] = func(t *testing.T, path string) {
			if err := os.Chown(path, -1, g); err != nil {
				t.Error(err)
			}
		}
	}
	for _, hook := range []string{"before the check", "before the rename"} {
		for name, change := range changes {
			t.Run(hook+"/"+name, func(t *testing.T) {
				home, _ := migrateHome(t)
				writeHomeFile(t, home, ".bashrc", "#\n"+legacyLoginBlock, 0o644)
				path := filepath.Join(home, ".bashrc")
				var snap fileSnapshot
				fn := func(p string) {
					if p == path {
						change(t, p)
						snap = snapshot(t, p)
					}
				}
				hp := &loginBeforeCommit
				if hook == "before the rename" {
					hp = &loginBeforeRename
				}
				*hp = fn
				t.Cleanup(func() { *hp = nil })
				res, err := migrateLoginBlocks(context.Background(), home)
				if err != nil || len(res.Changed) != 0 || len(res.Skipped) != 1 || !strings.Contains(res.Skipped[0], "it changed while its login block was updated") {
					t.Fatalf("%+v, %v", res, err)
				}
				if snapshot(t, path) != snap {
					t.Fatal("the concurrent change was overwritten")
				}
				noLeftovers(t, home, true)
			})
		}
	}
}

// An extended attribute that disappears between the size query and the read
// (ENODATA, size -1) is an error, never a panic, and the file is skipped.
func TestLoginXattrsSurviveADisappearingAttribute(t *testing.T) {
	old := loginListxattr
	t.Cleanup(func() { loginListxattr = old })
	calls := 0
	loginListxattr = func(fd int, dest []byte) (int, error) {
		calls++
		if dest == nil {
			return 16, nil
		}
		return -1, unix.ENODATA
	}
	if _, err := loginXattrs(0); !errors.Is(err, unix.ENODATA) {
		t.Fatalf("err = %v", err)
	}
	// A bogus length is an error too.
	loginListxattr = func(fd int, dest []byte) (int, error) {
		if dest == nil {
			return 4, nil
		}
		return 99, nil
	}
	if _, err := loginXattrs(0); err == nil {
		t.Fatal("a length beyond the buffer was accepted")
	}
	// In the migration: skipped with the repair.
	loginListxattr = func(fd int, dest []byte) (int, error) {
		if dest == nil {
			return 16, nil
		}
		return -1, unix.ENODATA
	}
	home, _ := migrateHome(t)
	writeHomeFile(t, home, ".profile", legacyLoginBlock, 0o644)
	snap := snapshot(t, filepath.Join(home, ".profile"))
	res, err := migrateLoginBlocks(context.Background(), home)
	if err != nil || len(res.Pending) != 1 || !strings.Contains(res.Skipped[0], "extended attributes cannot be read") {
		t.Fatalf("%+v, %v", res, err)
	}
	if snapshot(t, filepath.Join(home, ".profile")) != snap {
		t.Fatal("changed")
	}
}

// A shell that reads the file while it is updated sees the old or the new
// version, never a part of one.
func TestWorkerUpMigrationDashNeverSeesAPartialFile(t *testing.T) {
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
		if err := os.WriteFile(stage, []byte(legacy), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(stage, path); err != nil {
			t.Fatal(err)
		}
		res, err := migrateLoginBlocks(context.Background(), home)
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

func TestWorkerUpMigrationChangesOnlyTheExactBlock(t *testing.T) {
	home, _ := migrateHome(t)
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
	res, err := migrateLoginBlocks(context.Background(), home)
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

// The update is best effort: a file it cannot read is reported and the
// others are still updated; "worker up" does not stop.
func TestWorkerUpMigrationContinuesPastAFailure(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a file without permissions")
	}
	home, _ := migrateHome(t)
	writeHomeFile(t, home, ".bash_login", legacyLoginBlock, 0o000)
	writeHomeFile(t, home, ".bashrc", legacyLoginBlock, 0o600)
	res, err := migrateLoginBlocks(context.Background(), home)
	if err == nil || !strings.Contains(err.Error(), ".bash_login") || len(res.Changed) != 1 || res.Changed[0] != filepath.Join(home, ".bashrc") {
		t.Fatalf("%+v, %v", res, err)
	}
	var out bytes.Buffer
	reportLoginMigration(&out, res, err)
	if !strings.Contains(out.String(), "Updated the Worker login startup block in "+filepath.Join(home, ".bashrc")) || !strings.Contains(out.String(), "Warning: could not update") {
		t.Fatalf("report:\n%s", out.String())
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
			done := make(chan error, 1)
			go func() { _, _, err := readLoginRegistration(reg); done <- err }()
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("accepted")
				}
			case <-time.After(5 * time.Second):
				t.Fatal("blocked")
			}
		})
	}
	_ = os.Remove(reg)
	writeHomeFile(t, home, "worker-login", valid, 0o600)
	if _, s, err := readLoginRegistration(reg); err != nil || s != state {
		t.Fatalf("a valid registration: %q, %v", s, err)
	}
}

// The Worker never changes a startup file: with legacy blocks in place,
// "worker run" leaves every file as it was (bytes, inode, mtime), writes no
// backup, logs the notice once and reports it to the Hub.
func TestWorkerRunNeverChangesStartupFiles(t *testing.T) {
	home, state := migrateHome(t)
	writeHomeFile(t, home, ".bashrc", "#\n"+legacyLoginBlock, 0o644)
	writeHomeFile(t, home, ".profile", legacyLoginBlock, 0o600)
	target := filepath.Join(home, "dotfiles", "zprofile")
	writeHomeFile(t, home, "dotfiles/zprofile", legacyLoginBlock, 0o644)
	if err := os.Symlink(target, filepath.Join(home, ".zprofile")); err != nil {
		t.Fatal(err)
	}
	files := []string{filepath.Join(home, ".bashrc"), filepath.Join(home, ".profile"), target}
	snaps := map[string]fileSnapshot{}
	for _, p := range files {
		snaps[p] = snapshot(t, p)
	}
	deps, p := probeOnlyDeps(t, map[string]string{"MINION_STATE_DIR": state})
	logs := &bytes.Buffer{}
	deps.stderr = logs
	cancel, wait := startWorker(t, deps, p)
	if !p.seen().LoginStartupOutdated {
		t.Fatal("the outdated login startup is not reported")
	}
	time.Sleep(200 * time.Millisecond)
	cancel()
	if err := wait(); err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if snapshot(t, f) != snaps[f] {
			t.Fatalf("worker run changed %s", f)
		}
	}
	noLeftovers(t, home, true)
	noLeftovers(t, filepath.Join(home, "dotfiles"), true)
	if n := strings.Count(logs.String(), "Login startup is outdated"); n != 1 {
		t.Fatalf("notice logged %d times:\n%s", n, logs.String())
	}

	// Without a legacy block nothing is reported.
	for _, f := range files {
		if err := os.WriteFile(f, []byte(loginBlock), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	deps, p = probeOnlyDeps(t, map[string]string{"MINION_STATE_DIR": state})
	cancel, wait = startWorker(t, deps, p)
	if p.seen().LoginStartupOutdated {
		t.Fatal("reported without a legacy block")
	}
	cancel()
	if err := wait(); err != nil {
		t.Fatal(err)
	}
}
