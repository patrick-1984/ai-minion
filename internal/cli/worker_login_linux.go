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
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/patrick-1984/ai-minion/internal/client"
	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/worker/agent"
)

// The first line of the registration is the absolute executable, the second
// its state directory. Read paths as data so spaces and shell syntax are inert.
const loginBlock = `
# Minion Worker login startup
if [ -r "$HOME/.config/minion/worker-login" ]; then
    (IFS= read -r minion_bin && exec "$minion_bin" worker start --at-login </dev/null) < "$HOME/.config/minion/worker-login" >/dev/null 2>&1 &
fi
# End Minion Worker login startup
`

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
	if strings.Contains(string(b), loginBlock) {
		return nil
	}
	_, err = io.WriteString(f, loginBlock)
	return err
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
	f, err := identity.OpenPrivate(reg)
	if err != nil {
		return false, err
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(f, 2*syscall.PathMax+2))
	if err != nil {
		return false, err
	}
	bin, state, ok := strings.Cut(strings.TrimSuffix(string(b), "\n"), "\n")
	if !ok || !filepath.IsAbs(bin) || !filepath.IsAbs(state) || strings.ContainsAny(state, "\r\n") {
		return false, fmt.Errorf("invalid Worker login entry %s; repair it with minion worker up", reg)
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
