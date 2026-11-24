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
// home. Re-running the installer first replaces the blocks of earlier
// releases instead of adding a second one. Best effort: a file it cannot
// update (an unreadable inactive .bash_login, a file with an ACL) is reported
// with its repair and never stops the install.
func installLoginBlocks(ctx context.Context, home string, profiles []string, out io.Writer) error {
	res, err := migrateLoginBlocksLocked(ctx, nil, home)
	reportLoginMigration(out, res, err)
	for _, name := range profiles {
		path := filepath.Join(home, name)
		legacy, err := appendLoginBlock(path)
		if err != nil {
			return fmt.Errorf("install Worker login startup in %s: %w", path, err)
		}
		// A block of an earlier release left in place still starts the
		// Worker, but prints job lines: never reported as a clean install.
		if legacy && !slices.Contains(res.Pending, path) {
			_, _ = fmt.Fprintf(out, "Warning: %s\n", loginRepair(path, path, "it could not be updated"))
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

// migrateLoginStartup replaces the login blocks of earlier releases with the
// current one in the startup files of this user. The Worker runs it once it
// started (Settings.StartAfterReady), so an update fixes existing installs
// without any user action; it publishes a change only through gate, which
// the Worker revokes before it releases its lifecycle lock. It does nothing
// unless the login registration names this Worker's state directory: without
// a login install, after the user removed the registration to turn shell
// startup off, and for any other Worker profile of the account (a systemd or
// foreground Worker) the files stay untouched.
func migrateLoginStartup(ctx context.Context, stateDir string, gate *loginGate) (loginMigration, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return loginMigration{}, err
	}
	reg := loginRegistration(home)
	if !registeredLoginState(reg, stateDir) {
		return loginMigration{}, nil
	}
	unlock, err := lockConfigFileContext(ctx, reg+".lock")
	if err != nil {
		return loginMigration{}, err
	}
	defer unlock()
	if !registeredLoginState(reg, stateDir) || ctx.Err() != nil {
		return loginMigration{}, ctx.Err()
	}
	return migrateLoginBlocksLocked(ctx, gate, home)
}

// registeredLoginState reports whether the login registration reg is valid
// (readLoginRegistration) and names stateDir as the state directory it
// starts.
func registeredLoginState(reg, stateDir string) bool {
	if stateDir == "" || !filepath.IsAbs(stateDir) {
		return false
	}
	_, state, err := readLoginRegistration(reg)
	return err == nil && filepath.Clean(state) == filepath.Clean(stateDir)
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

// migrateLoginBlocksLocked updates every startup file in home that holds a
// block of an earlier release; the caller holds the registration lock. A
// failure in one file does not stop the others; the end of ctx or of gate
// stops the update before the next file and before any publication.
func migrateLoginBlocksLocked(ctx context.Context, gate *loginGate, home string) (loginMigration, error) {
	var res loginMigration
	fd, err := unix.Open(home, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return res, &os.PathError{Op: "open", Path: home, Err: err}
	}
	dir := os.NewFile(uintptr(fd), home)
	defer func() { _ = dir.Close() }()
	m := loginMigrator{ctx: ctx, gate: gate, res: &res}
	var errs []error
	for _, name := range loginStartupFiles {
		if err := ctx.Err(); err != nil {
			errs = append(errs, err)
			break
		}
		path := filepath.Join(home, name)
		if err := m.file(int(dir.Fd()), home, name, path, false); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", path, err))
			if errors.Is(err, errLoginRevoked) || ctx.Err() != nil {
				break
			}
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

// loginKeep is a reason to leave a file with an earlier block as it is: its
// metadata cannot be kept on a replacement.
type loginKeep struct{ why string }

func (k loginKeep) Error() string { return k.why }

// publish runs fn, the publication of one staged change, unless ctx ended or
// the gate was revoked.
func (g *loginGate) publish(ctx context.Context, fn func() error) error {
	if g != nil {
		g.mu.Lock()
		defer g.mu.Unlock()
		if g.revoked {
			return errLoginRevoked
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return fn()
}

// loginMigrator updates the startup files of one run of
// migrateLoginBlocksLocked.
type loginMigrator struct {
	ctx  context.Context
	gate *loginGate
	res  *loginMigration
}

// loginRepair is the report of the startup file shown (its name in the home
// directory) whose earlier block in path (shown, or the target of the link
// shown) the update left as it is, for why, with the manual repair.
func loginRepair(shown, path, why string) string {
	where := shown
	if path != shown {
		where = fmt.Sprintf("%s (the target of the symbolic link %s)", path, shown)
	}
	first, _, _ := strings.Cut(loginBlockBody, "\n")
	return fmt.Sprintf("%s still holds the login block of an earlier release, which prints shell job lines; it was left as it is because %s. To repair it, delete the lines from %q through %q in %s with an editor, then run \"minion worker up\" again",
		shown, why, first, "# End Minion Worker login startup", where)
}

// pending records that shown keeps an earlier block, for why.
func (m *loginMigrator) pending(shown, path, why string) {
	m.res.Skipped = append(m.res.Skipped, loginRepair(shown, path, why))
	m.res.Pending = append(m.res.Pending, shown)
}

// file updates the startup file name in the directory dirfd (dir) when it
// holds a block of an earlier release; shown is its name in the home
// directory, viaLink whether name is the resolved target of a link. A link
// in the home directory is kept, and its target updated when the path to it
// is controlled by this user (link). The new version is staged in a new file
// next to it and renamed over it, so a shell that reads the file sees the
// old or the new version, never a part. Left as it is, with the repair
// reported: a file of another user, with other hard links, with an ACL or
// extended attributes or a group that the new version cannot get.
func (m *loginMigrator) file(dirfd int, dir, name, shown string, viaLink bool) error {
	path := filepath.Join(dir, name)
	var lst unix.Stat_t
	if err := unix.Fstatat(dirfd, name, &lst, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil
		}
		return err
	}
	switch lst.Mode & unix.S_IFMT {
	case unix.S_IFLNK:
		if viaLink {
			if legacyBlockBehindLink(path) {
				m.pending(shown, path, "its target changed while it was resolved")
			}
			return nil
		}
		return m.link(dir, name, shown)
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
			return nil // replaced since the check: handled at the next start
		}
		return err
	}
	f := os.NewFile(uintptr(fd), path)
	defer func() { _ = f.Close() }()
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG || st.Dev != lst.Dev || st.Ino != lst.Ino || st.Size > loginFileMax {
		return nil // replaced since the check: handled at the next start
	}
	b, err := readLoginFile(f)
	if err != nil {
		return err
	}
	text, ok := replaceLegacyLoginBlocks(string(b))
	if !ok {
		return nil
	}
	switch {
	case int(st.Uid) != os.Geteuid():
		m.pending(shown, path, "it belongs to another user")
		return nil
	case st.Nlink > 1:
		m.pending(shown, path, "it has other hard links, which a new version would no longer share")
		return nil
	}
	xattrs, err := loginXattrs(fd)
	if err != nil {
		m.pending(shown, path, "its extended attributes cannot be read")
		return nil
	}
	for _, x := range xattrs {
		if strings.HasPrefix(x, "system.") {
			m.pending(shown, path, fmt.Sprintf("it has an access control list (%s) that a new version would not keep", x))
			return nil
		}
	}
	return m.replace(fd, f, &st, b, text, xattrs, dirfd, dir, name, shown)
}

// replace publishes text as the new version of the startup file name in
// dirfd (dir), read from fd (st) as old.
func (m *loginMigrator) replace(fd int, f *os.File, st *unix.Stat_t, old []byte, text string, xattrs []string, dirfd int, dir, name, shown string) error {
	path := filepath.Join(dir, name)
	staged, err := stageLoginFile(dirfd, name, text, fd, st, xattrs)
	if k := (loginKeep{}); errors.As(err, &k) {
		m.pending(shown, path, k.why)
		return nil
	}
	if err != nil {
		return fmt.Errorf("stage the new version: %w; the file is left as it is", err)
	}
	published := false
	defer func() {
		if !published {
			_ = unix.Unlinkat(dirfd, staged, 0)
		}
	}()
	if loginBeforeCommit != nil {
		loginBeforeCommit(path)
	}
	var backup string
	linked := false
	err = m.gate.publish(m.ctx, func() error {
		// The last check: an editor that saved the file in the meantime
		// wins, and its save is never overwritten.
		if !sameLoginFile(fd, f, st, old, dirfd, name) {
			return errLoginChanged
		}
		var err error
		if backup, linked, err = writeLoginBackup(dirfd, name, st, old); err != nil {
			return err
		}
		if loginBeforeRename != nil {
			loginBeforeRename(path)
		}
		var cur unix.Stat_t
		if err := unix.Fstatat(dirfd, name, &cur, unix.AT_SYMLINK_NOFOLLOW); err != nil || cur.Dev != st.Dev || cur.Ino != st.Ino {
			_ = unix.Unlinkat(dirfd, backup, 0)
			return errLoginChanged
		}
		if err := unix.Renameat(dirfd, staged, dirfd, name); err != nil {
			_ = unix.Unlinkat(dirfd, backup, 0)
			return fmt.Errorf("publish the new version: %w; the file is left as it is", err)
		}
		published = true
		return nil
	})
	if errors.Is(err, errLoginChanged) {
		m.res.Skipped = append(m.res.Skipped, fmt.Sprintf("%s changed while its login block was updated; it is left as it is and updated at the next start", shown))
		return nil
	}
	if err != nil {
		return err
	}
	backupPath := filepath.Join(dir, backup)
	m.res.Changed = append(m.res.Changed, path)
	m.res.Backups = append(m.res.Backups, backupPath)
	if err := unix.Fsync(dirfd); err != nil {
		return fmt.Errorf("sync %s: %w (the file was updated, the previous version is in %s)", dir, err, backupPath)
	}
	// The backup is the displaced inode itself: a save in place in the last
	// moment is in it, not lost.
	if b, err := readLoginFile(f); linked && err == nil && string(b) != string(old) {
		m.res.Skipped = append(m.res.Skipped, fmt.Sprintf("%s was saved while its login block was updated; that save is in %s", shown, backupPath))
	}
	return nil
}

// link updates the target of the symbolic link name in dir (shown in the
// home directory) and keeps the link. Only a target whose every directory is
// controlled by this user (or root) is updated, through descriptors opened
// without following links; otherwise a block of an earlier release behind
// the link is reported with its repair.
func (m *loginMigrator) link(dir, name, shown string) error {
	path := filepath.Join(dir, name)
	target, err := filepath.EvalSymlinks(path)
	if err != nil {
		if legacyBlockBehindLink(path) {
			m.pending(shown, path, "its target cannot be resolved")
		}
		return nil
	}
	pfd, why := openOwnedDir(filepath.Dir(target))
	if why != "" {
		if legacyBlockBehindLink(path) {
			m.pending(shown, target, why)
		}
		return nil
	}
	defer func() { _ = unix.Close(pfd) }()
	return m.file(pfd, filepath.Dir(target), filepath.Base(target), shown, true)
}

// openOwnedDir opens the absolute directory dir, one component at a time
// without following links, and returns its descriptor when no other user
// can change the path: each directory belongs to this user or root and only
// its owner can write it, or it is sticky (such as /tmp) and the entry in it
// belongs to this user. Otherwise it returns why not.
func openOwnedDir(dir string) (int, string) {
	fd, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, "its directory cannot be opened"
	}
	euid := os.Geteuid()
	shared := false
	cur := "/"
	check := func() string {
		var st unix.Stat_t
		if err := unix.Fstat(fd, &st); err != nil {
			return fmt.Sprintf("%s cannot be checked", cur)
		}
		switch {
		case shared && int(st.Uid) != euid:
			return fmt.Sprintf("%s is in a shared directory and belongs to another user", cur)
		case int(st.Uid) != euid && st.Uid != 0:
			return fmt.Sprintf("%s belongs to another user", cur)
		case st.Mode&0o022 != 0 && st.Mode&unix.S_ISVTX == 0:
			return fmt.Sprintf("other users can change %s", cur)
		}
		shared = st.Mode&0o022 != 0
		return ""
	}
	if why := check(); why != "" {
		_ = unix.Close(fd)
		return -1, why
	}
	for _, c := range strings.Split(filepath.Clean(dir), "/") {
		if c == "" {
			continue
		}
		cur = filepath.Join(cur, c)
		nfd, err := unix.Openat(fd, c, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		_ = unix.Close(fd)
		if err != nil {
			return -1, fmt.Sprintf("%s cannot be opened without following a link", cur)
		}
		fd = nfd
		if why := check(); why != "" {
			_ = unix.Close(fd)
			return -1, why
		}
	}
	// In a shared directory the file itself must belong to this user:
	// loginMigrator.file only updates such a file.
	return fd, ""
}

// sameLoginFile reports whether the startup file name in dirfd still is the
// version read from fd: the same inode at that name, with one link, the same
// size, modification time and content old.
func sameLoginFile(fd int, f *os.File, st *unix.Stat_t, old []byte, dirfd int, name string) bool {
	var now, cur unix.Stat_t
	if unix.Fstat(fd, &now) != nil || now.Size != st.Size || now.Mtim != st.Mtim || now.Nlink != 1 {
		return false
	}
	again, err := readLoginFile(f)
	if err != nil || string(again) != string(old) {
		return false
	}
	return unix.Fstatat(dirfd, name, &cur, unix.AT_SYMLINK_NOFOLLOW) == nil && cur.Dev == st.Dev && cur.Ino == st.Ino
}

// loginBeforeCommit, when set by a test, runs after the new version is
// staged and before the last check (an editor saving the file at that
// moment); loginBeforeRename runs after the backup, right before the rename.
var loginBeforeCommit, loginBeforeRename func(path string)

// loginXattrs lists the extended attributes of the file fd (a variable so a
// test can show an ACL where the file system has none).
var loginXattrs = func(fd int) ([]string, error) {
	for {
		n, err := unix.Flistxattr(fd, nil)
		if errors.Is(err, unix.ENOTSUP) || err == nil && n == 0 {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		buf := make([]byte, n)
		n, err = unix.Flistxattr(fd, buf)
		if errors.Is(err, unix.ERANGE) {
			continue
		}
		if err != nil {
			return nil, err
		}
		var names []string
		for _, x := range strings.Split(string(buf[:n]), "\x00") {
			if x != "" {
				names = append(names, x)
			}
		}
		return names, nil
	}
}

// getLoginXattr returns the value of the extended attribute x of fd.
func getLoginXattr(fd int, x string) ([]byte, error) {
	for {
		n, err := unix.Fgetxattr(fd, x, nil)
		if err != nil {
			return nil, err
		}
		buf := make([]byte, n)
		n, err = unix.Fgetxattr(fd, x, buf)
		if errors.Is(err, unix.ERANGE) {
			continue
		}
		return buf[:n], err
	}
}

// stageLoginFile writes text to a new file next to the startup file name in
// dirfd (read from fd, st) with its owner, group, mode and extended
// attributes, synced with the directory, and returns its name. A loginKeep
// error says which of them it cannot get.
func stageLoginFile(dirfd int, name, text string, fd int, st *unix.Stat_t, xattrs []string) (string, error) {
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
			for _, x := range xattrs {
				v, err := getLoginXattr(fd, x)
				if err != nil {
					return loginKeep{fmt.Sprintf("its extended attribute %s cannot be read", x)}
				}
				if unix.Fsetxattr(tfd, x, v, 0) != nil {
					if have, err := getLoginXattr(tfd, x); err != nil || string(have) != string(v) {
						return loginKeep{fmt.Sprintf("a new version cannot get its extended attribute %s", x)}
					}
				}
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

// legacyBlockBehindLink reports whether the regular file a symbolic link
// points to holds a block of an earlier release. It only reads.
func legacyBlockBehindLink(path string) bool {
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
// link to the displaced inode itself (linked), so even a save in place
// right before the rename is kept; any other file gets a 0600 copy of old,
// as its own mode would give its group or others access to the backup. The
// caller checked that the file has no ACL. It returns the backup's name.
func writeLoginBackup(dirfd int, name string, st *unix.Stat_t, old []byte) (string, bool, error) {
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
					return "", false, errLoginChanged
				}
				return bn, true, nil
			}
			// No hard links on this file system: a copy.
		}
		fd, err := unix.Openat(dirfd, bn, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
		if errors.Is(err, unix.EEXIST) {
			continue
		}
		if err != nil {
			return "", false, fmt.Errorf("write the backup copy: %w; the file is left as it is", err)
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
			return "", false, fmt.Errorf("write the backup copy: %w; the file is left as it is", err)
		}
		return bn, false, nil
	}
	return "", false, fmt.Errorf("too many backup copies of %s%s", name, loginBackupSuffix)
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
