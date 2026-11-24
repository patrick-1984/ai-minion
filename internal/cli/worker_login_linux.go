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
	// Re-running the installer replaces the blocks of earlier releases
	// instead of adding a second one. Best effort: a file it cannot update
	// (an unreadable inactive .bash_login, a symbolic link) is reported and
	// never stops the install.
	res, err := migrateLoginBlocksLocked(home)
	reportLoginMigration(out, res, err)
	for _, name := range profiles {
		if err := appendLoginBlock(filepath.Join(home, name)); err != nil {
			return fmt.Errorf("install Worker login startup in %s: %w", filepath.Join(home, name), err)
		}
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

func appendLoginBlock(path string) error {
	flags := os.O_CREATE | os.O_RDWR | os.O_APPEND | syscall.O_NONBLOCK
	if os.Geteuid() == 0 {
		flags |= syscall.O_NOFOLLOW
	}
	f, err := os.OpenFile(path, flags, 0600)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if os.Geteuid() == 0 {
		if st := info.Sys().(*syscall.Stat_t); st.Uid != 0 || info.Mode().Perm()&022 != 0 {
			return fmt.Errorf("root's startup file must be owned and writable only by root")
		}
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular shell profile", path)
	}
	b, err := io.ReadAll(f)
	if err != nil {
		return err
	}
	// A block of an earlier release that could not be updated (see
	// reportLoginMigration) still starts the Worker: a second block would
	// only start it twice.
	if hasLoginBlock(string(b)) {
		return nil
	}
	_, err = io.WriteString(f, loginBlock)
	return err
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
// without any user action. It does nothing unless the login registration
// names this Worker's state directory: without a login install, after the
// user removed the registration to turn shell startup off, and for any other
// Worker profile of the account (a systemd or foreground Worker) the files
// stay untouched.
func migrateLoginStartup(ctx context.Context, stateDir string) (loginMigration, error) {
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
	return migrateLoginBlocksLocked(home)
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
// failure in one file does not stop the others.
func migrateLoginBlocksLocked(home string) (loginMigration, error) {
	var res loginMigration
	fd, err := unix.Open(home, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return res, &os.PathError{Op: "open", Path: home, Err: err}
	}
	dir := os.NewFile(uintptr(fd), home)
	defer func() { _ = dir.Close() }()
	var errs []error
	for _, name := range loginStartupFiles {
		if err := migrateLoginFile(int(dir.Fd()), home, name, &res); err != nil {
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

// loginBackupSuffix names the copy of a startup file that the update writes
// next to it before changing it.
const loginBackupSuffix = ".minion-backup"

// migrateLoginFile updates the startup file name in the directory dirfd (home)
// when it holds a block of an earlier release. Only a regular file of this
// user is changed, never through a symbolic link: a link is reported and left
// as it is. The original bytes go to a backup copy next to the file first.
// The file is then changed in place, on the same inode, so its owner, group,
// mode, ACLs and hard links stay as they are; only if it still is the file
// that was read (same inode, size, modification time and content) right
// before the write. Otherwise it is left as it is and reported.
func migrateLoginFile(dirfd int, home, name string, res *loginMigration) error {
	path := filepath.Join(home, name)
	var lst unix.Stat_t
	if err := unix.Fstatat(dirfd, name, &lst, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil
		}
		return err
	}
	switch lst.Mode & unix.S_IFMT {
	case unix.S_IFLNK:
		if legacyBlockBehindLink(path) {
			res.Skipped = append(res.Skipped, fmt.Sprintf("%s is a symbolic link and still holds the login block of an earlier release (it prints shell job lines); the update does not write through links, so replace that block in the target by hand", path))
		}
		return nil
	case unix.S_IFREG:
	default:
		return nil
	}
	if int(lst.Uid) != os.Geteuid() || lst.Size > loginFileMax {
		return nil // not a startup file this account owns: leave it
	}
	fd, err := unix.Openat(dirfd, name, unix.O_RDWR|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
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
	if st.Mode&unix.S_IFMT != unix.S_IFREG || int(st.Uid) != os.Geteuid() || st.Dev != lst.Dev || st.Ino != lst.Ino || st.Size > loginFileMax {
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
	backup, err := writeLoginBackup(dirfd, name, b, &st)
	if err != nil {
		return fmt.Errorf("write the backup copy: %w; the file is left as it is", err)
	}
	backupPath := filepath.Join(home, backup)
	if loginBeforeCommit != nil {
		loginBeforeCommit(path)
	}
	// The last check, right before the write: an editor that saved the
	// file in the meantime wins, and its save is never overwritten.
	var now, cur unix.Stat_t
	if err := unix.Fstat(fd, &now); err != nil {
		return err
	}
	again, err := readLoginFile(f)
	curErr := unix.Fstatat(dirfd, name, &cur, unix.AT_SYMLINK_NOFOLLOW)
	if err != nil || curErr != nil || now.Size != st.Size || now.Mtim != st.Mtim || cur.Dev != st.Dev || cur.Ino != st.Ino || string(again) != string(b) {
		_ = unix.Unlinkat(dirfd, backup, 0)
		res.Skipped = append(res.Skipped, fmt.Sprintf("%s changed while its login block was updated; it is left as it is and updated at the next start", path))
		return nil
	}
	if _, err := f.WriteAt([]byte(text), 0); err != nil {
		return fmt.Errorf("update the login block: %w; the previous file is in %s", err, backupPath)
	}
	if err := f.Truncate(int64(len(text))); err != nil {
		return fmt.Errorf("update the login block: %w; the previous file is in %s", err, backupPath)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("update the login block: %w; the previous file is in %s", err, backupPath)
	}
	res.Changed = append(res.Changed, path)
	res.Backups = append(res.Backups, backupPath)
	return nil
}

// loginBeforeCommit, when set by a test, runs between the backup and the last
// check of migrateLoginFile (an editor saving the file at that moment).
var loginBeforeCommit func(path string)

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

// writeLoginBackup writes b, the content of the startup file name (st), to a
// new file next to it: name.minion-backup, or with a numeric suffix when that
// exists (an earlier backup is never overwritten). The copy gets the mode of
// the original when it can get its group too, otherwise only the owner's
// permissions, so it never gives anyone access the original did not. It
// returns the name of the copy.
func writeLoginBackup(dirfd int, name string, b []byte, st *unix.Stat_t) (string, error) {
	for i := 0; i < 100; i++ {
		bn := name + loginBackupSuffix
		if i > 0 {
			bn += "." + strconv.Itoa(i)
		}
		fd, err := unix.Openat(dirfd, bn, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
		if errors.Is(err, unix.EEXIST) {
			continue
		}
		if err != nil {
			return "", err
		}
		f := os.NewFile(uintptr(fd), bn)
		err = func() error {
			if _, err := f.Write(b); err != nil {
				return err
			}
			perm := st.Mode & 0o777
			var cst unix.Stat_t
			if err := unix.Fstat(fd, &cst); err != nil {
				return err
			}
			if cst.Gid != st.Gid && unix.Fchown(fd, -1, int(st.Gid)) != nil {
				perm &= 0o700
			}
			if err := unix.Fchmod(fd, perm); err != nil {
				return err
			}
			return f.Sync()
		}()
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			_ = unix.Unlinkat(dirfd, bn, 0)
			return "", err
		}
		return bn, nil
	}
	return "", fmt.Errorf("too many backup copies of %s%s in the home directory", name, loginBackupSuffix)
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
