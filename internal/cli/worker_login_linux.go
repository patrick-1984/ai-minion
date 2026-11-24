//go:build linux

package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/sys/unix"

	"github.com/patrick-1984/ai-minion/internal/client"
	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/worker/agent"
)

// The first line of the registration is the absolute executable, the second
// its state directory. Read paths as data so spaces and shell syntax are inert.
//
// The start runs in the background of a subshell, "( cmd & )": the
// interactive shell gets no job, so bash, zsh and dash print no "[1]" or
// "Done" notice. The guard holds the PID of the shell that ran the block, so
// a second copy of the block in the same shell, such as .profile sourcing
// .bashrc, does nothing, while any other shell (a child shell, a tmux pane, a
// new terminal) has another PID and checks the Worker, even when the value
// was exported to it (set -a). "worker start --at-login" itself is idempotent
// under the registration lock.
const loginBlock = "\n" + loginBlockBody + "\n"

const loginBlockBody = `# Minion Worker login startup
if [ "${minion_worker_login_started-}" != "$$" ] && [ -r "$HOME/.config/minion/worker-login" ]; then
    minion_worker_login_started=$$
    ( (IFS= read -r minion_bin && exec "$minion_bin" worker start --at-login </dev/null) < "$HOME/.config/minion/worker-login" >/dev/null 2>&1 & )
fi
# End Minion Worker login startup`

// legacyLoginBlockBodies are the managed blocks earlier releases wrote (up to
// v0.20.5). They are replaced by loginBlockBody, byte for byte, and only
// them: a block the user edited is left alone.
var legacyLoginBlockBodies = []string{`# Minion Worker login startup
if [ -r "$HOME/.config/minion/worker-login" ]; then
    (IFS= read -r minion_bin && exec "$minion_bin" worker start --at-login </dev/null) < "$HOME/.config/minion/worker-login" >/dev/null 2>&1 &
fi
# End Minion Worker login startup`}

// loginStartupFiles are the shell startup files the login block can be in.
var loginStartupFiles = []string{".profile", ".bashrc", ".bash_profile", ".bash_login", ".zprofile"}

func init() {
	Register(func(root *cobra.Command) {
		// Used after dropping root privileges. All home, log and control
		// socket operations must run as the account that owns the Worker.
		workerCommand(root).AddCommand(&cobra.Command{
			Use: "install-login <state-dir>", Hidden: true, Args: cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				bin, err := os.Executable()
				if err != nil {
					return err
				}
				return installLoginWorker(cmdContext(cmd), bin, args[0], cmd.OutOrStdout())
			},
		})
	})
}

func loginRegistration(home string) string {
	return filepath.Join(home, ".config", "minion", "worker-login")
}

func installLoginWorker(ctx context.Context, binary, state string, out io.Writer) error {
	for _, p := range []string{binary, state} {
		if !filepath.IsAbs(p) || strings.ContainsAny(p, "\r\n") {
			return fmt.Errorf("login startup requires an absolute single-line path, got %q", p)
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	reg := loginRegistration(home)
	if os.Geteuid() == 0 {
		if err := identity.CheckPrivateDir(filepath.Dir(reg)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("cannot install root's login entry in %s: %w; run from a root login with sudo -i", home, err)
		}
		if err := checkRootExecutable(binary); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(filepath.Dir(reg), 0700); err != nil {
		return err
	}
	unlock, err := lockConfigFileContext(ctx, reg+".lock")
	if err != nil {
		return err
	}
	defer unlock()

	profiles := []string{".profile", ".bashrc"}
	// A Bash login reads only the first existing file in this order.
	for _, name := range []string{".bash_profile", ".bash_login"} {
		if _, err := os.Stat(filepath.Join(home, name)); err == nil {
			profiles = append(profiles, name)
			break
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	// zsh does not read .profile: also .zprofile when zsh is the login
	// shell, or the user has zsh files (under sudo SHELL is root's).
	if _, err := os.Stat(filepath.Join(home, ".zshrc")); filepath.Base(os.Getenv("SHELL")) == "zsh" || err == nil {
		profiles = append(profiles, ".zprofile")
	}
	if err := installLoginBlocks(ctx, home, profiles, out); err != nil {
		return err
	}

	// Everything the replacement needs is written and checked before the
	// running Worker is stopped, so a failure here leaves it running.
	if err := identity.WritePrivate(reg, []byte(binary+"\n"+state+"\n")); err != nil {
		return err
	}
	if f, err := os.OpenFile(filepath.Join(state, launchLogName), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600); err != nil {
		return fmt.Errorf("the Worker log cannot be written (%w); nothing was stopped", err)
	} else {
		_ = f.Close()
	}
	// Restart an existing Worker on this profile so a foreground Worker
	// releases the terminal and new settings take effect.
	if err := stopPriorWorker(ctx, state, false, client.Local()); err != nil {
		return err
	}
	clearLoginStopped()
	if err := spawnLoginWorker(ctx, binary, state); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "Started the Worker in the background. It starts again at shell login. Log: %s\n", filepath.Join(state, launchLogName))
	return nil
}

// installLoginBlocks writes the login block to the startup files profiles of
// home. Re-running the installer ("minion worker up", which the user runs;
// nothing else changes a startup file) first replaces the blocks of earlier
// releases instead of adding a second one. Best effort: a file it cannot
// update (an unreadable inactive .bash_login, a file with an ACL) is reported
// with its repair and never stops the install.
func installLoginBlocks(ctx context.Context, home string, profiles []string, out io.Writer) error {
	res, err := migrateLoginBlocks(ctx, home)
	reportLoginMigration(out, res, err)
	for _, name := range profiles {
		path := filepath.Join(home, name)
		// It holds a block of an earlier release that was reported with its
		// repair: that block still starts the Worker, and the file is not
		// touched (it never stops the install).
		if slices.Contains(res.Pending, path) {
			continue
		}
		legacy, err := appendLoginBlock(path)
		if err != nil {
			return fmt.Errorf("install Worker login startup in %s: %w", path, err)
		}
		// A block of an earlier release left in place still starts the
		// Worker, but prints job lines: never reported as a clean install.
		if legacy {
			_, _ = fmt.Fprintf(out, "Warning: %s\n", loginRepair(path, "it could not be updated"))
		}
	}
	return nil
}

// appendLoginBlock adds the login block to the startup file path unless it
// holds one. legacy is true when the block it holds is only one of an earlier
// release.
func appendLoginBlock(path string) (legacy bool, err error) {
	flags := os.O_CREATE | os.O_RDWR | os.O_APPEND | syscall.O_NONBLOCK
	if os.Geteuid() == 0 {
		flags |= syscall.O_NOFOLLOW
	}
	f, err := os.OpenFile(path, flags, 0600)
	if err != nil {
		return false, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return false, err
	}
	if os.Geteuid() == 0 {
		if st := info.Sys().(*syscall.Stat_t); st.Uid != 0 || info.Mode().Perm()&022 != 0 {
			return false, fmt.Errorf("root's startup file must be owned and writable only by root")
		}
	}
	if !info.Mode().IsRegular() {
		return false, fmt.Errorf("%s is not a regular shell profile", path)
	}
	b, err := io.ReadAll(f)
	if err != nil {
		return false, err
	}
	// A block of an earlier release that could not be updated (see
	// reportLoginMigration) still starts the Worker: a second block would
	// only start it twice.
	if len(loginBlockAt(string(b), loginBlockBody)) > 0 {
		return false, nil
	}
	if hasLoginBlock(string(b)) {
		return true, nil
	}
	_, err = io.WriteString(f, loginBlock)
	return false, err
}

// loginRegistrationMax bounds the login registration: two paths.
const loginRegistrationMax = 2*syscall.PathMax + 2

// readLoginRegistration reads the login registration reg: a private regular
// file of this user (no symbolic link, FIFO, foreign owner or group or other
// access), at most loginRegistrationMax bytes, holding the absolute
// executable and the absolute state directory on one line each.
func readLoginRegistration(reg string) (bin, state string, err error) {
	f, err := identity.OpenPrivate(reg)
	if err != nil {
		return "", "", err
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(f, loginRegistrationMax+1))
	if err != nil {
		return "", "", err
	}
	bin, state, ok := strings.Cut(strings.TrimSuffix(string(b), "\n"), "\n")
	if len(b) > loginRegistrationMax || !ok || !filepath.IsAbs(bin) || !filepath.IsAbs(state) || strings.ContainsAny(bin, "\r") || strings.ContainsAny(state, "\r\n") {
		return "", "", fmt.Errorf("invalid Worker login entry %s; repair it with minion worker up", reg)
	}
	return bin, state, nil
}

// loginMigration is what "minion worker up" did to the login blocks of
// earlier releases.
type loginMigration struct {
	// Changed are the startup files it updated, Backups the copy of each
	// (same index) that it kept.
	Changed, Backups []string
	// Skipped says why a file with an earlier block was left as it is, with
	// the repair.
	Skipped []string
	// Pending are the startup files that still hold a block of an earlier
	// release.
	Pending []string
}

// outdatedLoginStartup returns the startup files of this user that still
// hold the login block of an earlier release. It only reads: the Worker
// (worker run, worker start, a self-update) never changes a startup file;
// only "minion worker up", which the user runs, replaces the block.
func outdatedLoginStartup() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	var out []string
	for _, name := range loginStartupFiles {
		if p := filepath.Join(home, name); legacyBlockIn(p) {
			out = append(out, p)
		}
	}
	return out
}

// reportLoginMigration tells the installer's user what the update of earlier
// login blocks did; it never fails the install.
func reportLoginMigration(out io.Writer, res loginMigration, err error) {
	for i, p := range res.Changed {
		_, _ = fmt.Fprintf(out, "Updated the Worker login startup block in %s (the previous file is in %s).\n", p, res.Backups[i])
	}
	for _, note := range res.Skipped {
		_, _ = fmt.Fprintf(out, "Warning: %s\n", note)
	}
	if err != nil {
		_, _ = fmt.Fprintf(out, "Warning: could not update an earlier Worker login startup block: %v\n", err)
	}
}

// migrateLoginBlocks replaces the login blocks of earlier releases in the
// startup files of home; only "minion worker up" runs it (the caller holds
// the registration lock). A failure in one file does not stop the others.
func migrateLoginBlocks(ctx context.Context, home string) (loginMigration, error) {
	var res loginMigration
	fd, err := unix.Open(home, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return res, &os.PathError{Op: "open", Path: home, Err: err}
	}
	defer func() { _ = unix.Close(fd) }()
	var errs []error
	for _, name := range loginStartupFiles {
		if err := ctx.Err(); err != nil {
			errs = append(errs, err)
			break
		}
		if err := migrateLoginFile(&res, fd, home, name); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", filepath.Join(home, name), err))
		}
	}
	return res, errors.Join(errs...)
}

// loginBlockAt returns the offsets of the copies of body in text that are
// whole lines: each starts at the beginning of a line and ends at the end of
// one, so its first and last lines are the complete marker lines.
func loginBlockAt(text, body string) []int {
	var at []int
	for i := 0; i <= len(text); {
		j := strings.Index(text[i:], body)
		if j < 0 {
			break
		}
		start, end := i+j, i+j+len(body)
		if (start == 0 || text[start-1] == '\n') && (end == len(text) || text[end] == '\n') {
			at = append(at, start)
			i = end
		} else {
			i = start + 1
		}
	}
	return at
}

// hasLoginBlock reports whether text holds a managed login block, current or
// of an earlier release, as whole lines.
func hasLoginBlock(text string) bool {
	if len(loginBlockAt(text, loginBlockBody)) > 0 {
		return true
	}
	for _, old := range legacyLoginBlockBodies {
		if len(loginBlockAt(text, old)) > 0 {
			return true
		}
	}
	return false
}

// replaceLegacyLoginBlocks returns text with every block of an earlier release
// (byte for byte the text it wrote, between complete marker lines) replaced
// by the current block; everything else, other copies included, stays as it
// is. changed is false when text has no such block.
func replaceLegacyLoginBlocks(text string) (string, bool) {
	changed := false
	for _, old := range legacyLoginBlockBodies {
		at := loginBlockAt(text, old)
		if len(at) == 0 {
			continue
		}
		changed = true
		var b strings.Builder
		prev := 0
		for _, i := range at {
			b.WriteString(text[prev:i])
			b.WriteString(loginBlockBody)
			prev = i + len(old)
		}
		b.WriteString(text[prev:])
		text = b.String()
	}
	return text, changed
}

// loginFileMax bounds the startup files that the update reads.
const loginFileMax = 4 << 20

// loginBackupSuffix names the copy of the displaced startup file that the
// update keeps next to it; loginStagedSuffix the new version before it is
// published.
const (
	loginBackupSuffix = ".minion-backup"
	loginStagedSuffix = ".minion-new"
)

// errLoginChanged: the startup file is no longer the version the update read.
var errLoginChanged = errors.New("the file changed")

// loginRepair is the one-line report of the startup file path whose earlier
// block the update left as it is, for why, with the manual repair.
func loginRepair(path, why string) string {
	first, _, _ := strings.Cut(loginBlockBody, "\n")
	return fmt.Sprintf("%s still holds the login block of an earlier release, which prints shell job lines; it was left as it is because %s. To repair it, delete the lines from %q through %q in %s with an editor, then run \"minion worker up\" again",
		path, why, first, "# End Minion Worker login startup", path)
}

// migrateLoginFile replaces the blocks of an earlier release in the startup
// file name of the directory dirfd (dir). The new version is staged in a new
// file next to it with the same group and mode and renamed over it, so a
// shell that reads the file sees the old or the new version, never a part;
// the displaced version is kept as an owner-only backup. Strict: a symbolic
// link, a file with extended attributes (an ACL), with other hard links or
// of another user, or in a directory with a default ACL is left as it is and
// reported with its repair; so is a file that changed while it was updated.
func migrateLoginFile(res *loginMigration, dirfd int, dir, name string) error {
	path := filepath.Join(dir, name)
	skip := func(why string) error {
		res.Skipped = append(res.Skipped, loginRepair(path, why))
		res.Pending = append(res.Pending, path)
		return nil
	}
	var lst unix.Stat_t
	if err := unix.Fstatat(dirfd, name, &lst, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil
		}
		return err
	}
	switch lst.Mode & unix.S_IFMT {
	case unix.S_IFLNK:
		if legacyBlockIn(path) {
			return skip("it is a symbolic link, which the update does not write through")
		}
		return nil
	case unix.S_IFREG:
	default:
		return nil
	}
	if lst.Size > loginFileMax {
		return nil
	}
	fd, err := unix.Openat(dirfd, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ENOENT) {
			return skip("it was replaced while it was read")
		}
		return err
	}
	f := os.NewFile(uintptr(fd), path)
	defer func() { _ = f.Close() }()
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG || st.Dev != lst.Dev || st.Ino != lst.Ino {
		return skip("it was replaced while it was read")
	}
	old, err := readLoginFile(f)
	if err != nil {
		return err
	}
	text, ok := replaceLegacyLoginBlocks(string(old))
	if !ok {
		return nil
	}
	switch {
	case int(st.Uid) != os.Geteuid():
		return skip("it belongs to another user")
	case st.Nlink > 1:
		return skip("it has other hard links, which a new version would no longer share")
	}
	if x, err := loginXattrs(fd); err != nil {
		return skip("its extended attributes cannot be read")
	} else if len(x) > 0 {
		return skip(fmt.Sprintf("it has extended attributes or an access control list (%s) that a new version would not keep", strings.Join(x, ", ")))
	}
	if acl, err := loginDefaultACL(dirfd); err != nil || acl {
		return skip("its directory has a default access control list, which a new version would inherit")
	}

	staged, err := stageLoginFile(dirfd, name, text, &st)
	if k := (loginKeep{}); errors.As(err, &k) {
		return skip(k.why)
	}
	if err != nil {
		return fmt.Errorf("stage the new version: %w; the file is left as it is", err)
	}
	published, backup := false, ""
	defer func() {
		if !published {
			_ = unix.Unlinkat(dirfd, staged, 0)
			if backup != "" {
				_ = unix.Unlinkat(dirfd, backup, 0)
			}
		}
	}()
	changed := func() error {
		return skip("it changed while its login block was updated (nothing was written)")
	}
	if loginBeforeCommit != nil {
		loginBeforeCommit(path)
	}
	if !sameLoginFile(fd, f, &st, old, dirfd, name, 1) {
		return changed()
	}
	var nlink uint64
	if backup, nlink, err = writeLoginBackup(dirfd, name, &st, old); errors.Is(err, errLoginChanged) {
		return changed()
	} else if err != nil {
		return err
	}
	if loginBeforeRename != nil {
		loginBeforeRename(path)
	}
	// The last check, right before the rename: owner, group, mode, size,
	// modification time, extended attributes and content are still the ones
	// read, and the name still is that file.
	if !sameLoginFile(fd, f, &st, old, dirfd, name, nlink) {
		return changed()
	}
	if err := unix.Renameat(dirfd, staged, dirfd, name); err != nil {
		return fmt.Errorf("publish the new version: %w; the file is left as it is", err)
	}
	published = true
	backupPath := filepath.Join(dir, backup)
	res.Changed = append(res.Changed, path)
	res.Backups = append(res.Backups, backupPath)
	if err := unix.Fsync(dirfd); err != nil {
		return fmt.Errorf("sync %s: %w (the file was updated, the previous version is in %s)", dir, err, backupPath)
	}
	return nil
}

// loginKeep is a reason to leave a file with an earlier block as it is: its
// metadata cannot be kept on a replacement.
type loginKeep struct{ why string }

func (k loginKeep) Error() string { return k.why }

// sameLoginFile reports whether the startup file name in dirfd still is the
// version read from fd (st, content old): the same inode at that name with
// nlink links, the same owner, group, mode, size and modification time, no
// extended attributes and the same content.
func sameLoginFile(fd int, f *os.File, st *unix.Stat_t, old []byte, dirfd int, name string, nlink uint64) bool {
	var now, cur unix.Stat_t
	if unix.Fstat(fd, &now) != nil || now.Uid != st.Uid || now.Gid != st.Gid || now.Mode != st.Mode ||
		now.Size != st.Size || now.Mtim != st.Mtim || uint64(now.Nlink) != nlink {
		return false
	}
	if x, err := loginXattrs(fd); err != nil || len(x) > 0 {
		return false
	}
	again, err := readLoginFile(f)
	if err != nil || string(again) != string(old) {
		return false
	}
	return unix.Fstatat(dirfd, name, &cur, unix.AT_SYMLINK_NOFOLLOW) == nil && cur.Dev == st.Dev && cur.Ino == st.Ino
}

// loginBeforeCommit, when set by a test, runs after the new version is
// staged and before the first check (an editor saving the file at that
// moment); loginBeforeRename runs after the backup, right before the last
// check and the rename.
var loginBeforeCommit, loginBeforeRename func(path string)

// loginListxattr is flistxattr(2) (a variable so a test can make an
// attribute disappear between the size query and the read).
var loginListxattr = unix.Flistxattr

// loginXattrs lists the extended attributes of the file fd (a variable so a
// test can show an ACL where the file system has none). Errors are handled
// before the buffer is sliced: an attribute that disappears in between never
// panics.
var loginXattrs = func(fd int) ([]string, error) {
	for range 10 {
		n, err := loginListxattr(fd, nil)
		if errors.Is(err, unix.ENOTSUP) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		if n <= 0 {
			return nil, nil
		}
		buf := make([]byte, n)
		n, err = loginListxattr(fd, buf)
		if errors.Is(err, unix.ERANGE) {
			continue // grew in between
		}
		if err != nil {
			return nil, err
		}
		if n < 0 || n > len(buf) {
			return nil, fmt.Errorf("flistxattr returned %d bytes for a %d-byte buffer", n, len(buf))
		}
		var names []string
		for _, x := range strings.Split(string(buf[:n]), "\x00") {
			if x != "" {
				names = append(names, x)
			}
		}
		return names, nil
	}
	return nil, errors.New("the extended attributes keep changing")
}

// loginDefaultACL reports whether the directory dirfd has a default ACL,
// which a new file in it would inherit (a variable for tests).
var loginDefaultACL = func(dirfd int) (bool, error) {
	n, err := unix.Fgetxattr(dirfd, "system.posix_acl_default", nil)
	switch {
	case errors.Is(err, unix.ENODATA) || errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.EOPNOTSUPP):
		return false, nil
	case err != nil:
		return false, err
	}
	return n > 0, nil
}

// stageLoginFile writes text to a new file next to the startup file name in
// dirfd (st) with its group and mode, synced with the directory, and returns
// its name. A loginKeep error says which of them it cannot get.
func stageLoginFile(dirfd int, name, text string, st *unix.Stat_t) (string, error) {
	for i := 0; i < 100; i++ {
		sn := fmt.Sprintf("%s%s.%d.%d", name, loginStagedSuffix, os.Getpid(), i)
		tfd, err := unix.Openat(dirfd, sn, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
		if errors.Is(err, unix.EEXIST) {
			continue
		}
		if err != nil {
			return "", err
		}
		t := os.NewFile(uintptr(tfd), sn)
		err = func() error {
			if _, err := io.WriteString(t, text); err != nil {
				return err
			}
			var cst unix.Stat_t
			if err := unix.Fstat(tfd, &cst); err != nil {
				return err
			}
			if cst.Uid != st.Uid {
				return loginKeep{"a new version would belong to another user"}
			}
			if cst.Gid != st.Gid && unix.Fchown(tfd, -1, int(st.Gid)) != nil {
				return loginKeep{fmt.Sprintf("a new version cannot get its group (%d)", st.Gid)}
			}
			if err := unix.Fchmod(tfd, st.Mode&0o7777); err != nil {
				return err
			}
			if err := unix.Fstat(tfd, &cst); err != nil {
				return err
			}
			if cst.Mode != st.Mode || cst.Gid != st.Gid {
				return loginKeep{"a new version cannot get its group and mode"}
			}
			// Nothing inherited: the original has no extended attributes.
			if x, err := loginXattrs(tfd); err != nil || len(x) > 0 {
				return loginKeep{"a new version would get extended attributes the file does not have"}
			}
			return t.Sync()
		}()
		if cerr := t.Close(); err == nil {
			err = cerr
		}
		if err == nil {
			err = unix.Fsync(dirfd)
		}
		if err != nil {
			_ = unix.Unlinkat(dirfd, sn, 0)
			return "", err
		}
		return sn, nil
	}
	return "", fmt.Errorf("too many staged copies of %s%s", name, loginStagedSuffix)
}

// readLoginFile reads all of f from its start, at most loginFileMax bytes.
func readLoginFile(f *os.File) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(io.NewSectionReader(f, 0, loginFileMax+1), loginFileMax+1))
	if err == nil && len(b) > loginFileMax {
		err = errors.New("the file is too large")
	}
	return b, err
}

// legacyBlockIn reports whether the regular file at path (a symbolic link
// is followed) holds a block of an earlier release. It only reads.
func legacyBlockIn(path string) bool {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() || fi.Size() > loginFileMax {
		return false
	}
	b, err := readLoginFile(f)
	if err != nil {
		return false
	}
	_, ok := replaceLegacyLoginBlocks(string(b))
	return ok
}

// writeLoginBackup keeps the version of the startup file name (st, content
// old) that the update displaces, next to it: name.minion-backup, or with a
// numeric suffix when that exists (an earlier backup is never overwritten).
// The backup is owner-only (0600): for a file with mode 0600 it is a hard
// link to the displaced inode itself, so even a save in place right before
// the rename is kept (nlink is then 2); any other file gets a 0600 copy of
// old, as its own mode would give its group or others access to the backup.
// It returns the backup's name and the link count the file has now.
func writeLoginBackup(dirfd int, name string, st *unix.Stat_t, old []byte) (string, uint64, error) {
	for i := 0; i < 100; i++ {
		bn := name + loginBackupSuffix
		if i > 0 {
			bn += "." + strconv.Itoa(i)
		}
		if st.Mode&0o7777 == 0o600 {
			err := unix.Linkat(dirfd, name, dirfd, bn, 0)
			if errors.Is(err, unix.EEXIST) {
				continue
			}
			if err == nil {
				var bst unix.Stat_t
				if err := unix.Fstatat(dirfd, bn, &bst, unix.AT_SYMLINK_NOFOLLOW); err != nil || bst.Dev != st.Dev || bst.Ino != st.Ino {
					_ = unix.Unlinkat(dirfd, bn, 0)
					return "", 0, errLoginChanged
				}
				return bn, 2, nil
			}
			// No hard links on this file system: a copy.
		}
		fd, err := unix.Openat(dirfd, bn, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
		if errors.Is(err, unix.EEXIST) {
			continue
		}
		if err != nil {
			return "", 0, fmt.Errorf("write the backup copy: %w; the file is left as it is", err)
		}
		f := os.NewFile(uintptr(fd), bn)
		_, err = f.Write(old)
		if err == nil {
			err = unix.Fchmod(fd, 0o600)
		}
		if err == nil {
			err = f.Sync()
		}
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			_ = unix.Unlinkat(dirfd, bn, 0)
			return "", 0, fmt.Errorf("write the backup copy: %w; the file is left as it is", err)
		}
		return bn, 1, nil
	}
	return "", 0, fmt.Errorf("too many backup copies of %s%s", name, loginBackupSuffix)
}

func startLoginWorker(ctx context.Context) (bool, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return false, err
	}
	reg := loginRegistration(home)
	if _, err := os.Stat(reg); errors.Is(err, os.ErrNotExist) {
		return false, errNotInstalled
	}
	unlock, err := lockConfigFileContext(ctx, reg+".lock")
	if err != nil {
		return false, err
	}
	defer unlock()
	bin, state, err := readLoginRegistration(reg)
	if err != nil {
		return false, err
	}
	if os.Geteuid() == 0 {
		if err := checkRootExecutable(bin); err != nil {
			return false, err
		}
	}
	// A login shell can source both profile files. Recheck under the
	// registration lock after the first start has had time to bind its socket.
	if _, err := (localLive{stateDir: state, backend: client.Local()}).LiveStatus(ctx); err == nil {
		return false, nil
	} else if !errors.Is(err, agent.ErrNoLiveWorker) {
		return false, err
	}
	return true, spawnLoginWorker(ctx, bin, state)
}

func spawnLoginWorker(ctx context.Context, bin, state string) error {
	log, err := openLaunchLog(state)
	if err != nil {
		return fmt.Errorf("open the Worker log: %w", err)
	}
	defer func() { _ = log.Close() }()
	// No enrollment or other credentials are copied to the child. Shell
	// runtime paths are needed by the local control and tmux sockets.
	cmd := exec.Command(bin, "worker", "run", "--state-dir", state)
	for _, key := range []string{"HOME", "USER", "LOGNAME", "PATH", "SHELL", "LANG", "LC_ALL", "LC_CTYPE", "XDG_RUNTIME_DIR", "TMPDIR", "TMUX_TMPDIR"} {
		if val, ok := os.LookupEnv(key); ok {
			cmd.Env = append(cmd.Env, key+"="+val)
		}
	}
	cmd.Env = append(cmd.Env, "MINION_DETACHED=1")
	cmd.Dir = os.Getenv("HOME")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start the background Worker: %w", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	live := localLive{stateDir: state, backend: client.Local()}
	for {
		select {
		case err := <-done:
			return fmt.Errorf("the background Worker exited (%v); see %s", err, filepath.Join(state, launchLogName))
		case <-ctx.Done():
			return fmt.Errorf("waiting for the background Worker: %w; see %s", ctx.Err(), filepath.Join(state, launchLogName))
		case <-ticker.C:
			if _, err := live.LiveStatus(ctx); err == nil {
				return nil
			}
		}
	}
}

// loginStoppedMarker is written by "worker down" for a Worker that the login
// entry starts: a new shell (a tmux pane, a terminal) then leaves it stopped
// until "worker start", "worker up" or the next boot.
func loginStoppedMarker() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return loginRegistration(home) + ".stopped", nil
}

func bootID() string {
	b, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// markLoginStopped records that the user stopped the login Worker.
func markLoginStopped() error {
	p, err := loginStoppedMarker()
	if err != nil {
		return err
	}
	return identity.WritePrivate(p, []byte(bootID()+"\n"))
}

// clearLoginStopped forgets that mark (an explicit start or up).
func clearLoginStopped() {
	if p, err := loginStoppedMarker(); err == nil {
		_ = os.Remove(p)
	}
}

// loginStopped reports whether the user stopped the login Worker in this boot.
func loginStopped() bool {
	p, err := loginStoppedMarker()
	if err != nil {
		return false
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(b)) == bootID()
}
