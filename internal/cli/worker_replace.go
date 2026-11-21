package cli

// "minion worker up --join" on a machine that already has a Worker (v0.17).
//
// The hosted installer (deploy/install.sh --join) runs the installed binary
// as `minion worker up --join - --interactive-replace`. Before anything is
// installed, stopped or saved, the command looks at the target read-only,
// shows what is there (account, state path, name, Hub and Hub key words) and
// asks on the terminal, not on standard input (which carries the token),
// whether the new Hub replaces it. The default is No: nothing changes and the
// exit status is 0. --replace and --keep answer for a script; with neither
// and without a terminal the command refuses and names the flags.
//
// Replacing is a transaction. The old Worker is stopped, a private copy of its
// state directory (key pair, pin, enrollment, config file, pause marker) is
// kept, the profile is cleared of ALL trust settings (the pairing, the share
// choice, the Hub settings of the config file including hub_fingerprint, and
// the pause marker), and the join runs on it. It commits, deleting the copy,
// only when the new Worker is live (waiting for approval counts). On any
// failure the copy replaces the state directory again and the old supervisor
// is started again. Callers that give none of the three flags (the tray, for
// example) keep the behaviour they had.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/spf13/cobra"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/worker/agent"
	"github.com/patrick-1984/ai-minion/internal/worker/service"
	"github.com/patrick-1984/ai-minion/internal/worker/store"
)

// replaceBackupSuffix names the private copy of the old state directory.
const replaceBackupSuffix = ".replace-backup"

// replaceDeps are the seams of the replace flow that tests replace.
type replaceDeps struct {
	// tty opens the terminal for the question; nil means the real one.
	tty func() (io.ReadWriteCloser, error)
	// stopOld stops the running Worker of this user; startOld starts it again.
	stopOld, startOld func(ctx context.Context, out io.Writer) error
	// live reports whether a Worker holds the state directory.
	live func(stateDir string) bool
}

func (d replaceDeps) openTTY() (io.ReadWriteCloser, error) {
	if d.tty != nil {
		return d.tty()
	}
	if runtime.GOOS == "windows" {
		return nil, errors.New("no terminal")
	}
	return os.OpenFile("/dev/tty", os.O_RDWR, 0)
}

func (d replaceDeps) isLive(dir string) bool {
	if d.live != nil {
		return d.live(dir)
	}
	return !workerStateFree(dir)
}

func (d replaceDeps) stop(ctx context.Context, out io.Writer) error {
	if d.stopOld != nil {
		return d.stopOld(ctx, out)
	}
	c := &cobra.Command{}
	c.SetOut(out)
	return runWorkerDown(c, defaultWorkerDownDeps())
}

func (d replaceDeps) start(ctx context.Context, out io.Writer) error {
	if d.startOld != nil {
		return d.startOld(ctx, out)
	}
	c := &cobra.Command{}
	c.SetOut(out)
	err := runWorkerStart(c, defaultWorkerStartDeps())
	if errors.Is(err, errNotInstalled) {
		return nil
	}
	return err
}

// addReplaceFlags adds the flags of the replace flow to "worker up".
func addReplaceFlags(cmd *cobra.Command) {
	f := cmd.Flags()
	f.Bool("replace", false, "With --join: replace an existing Worker of this account without asking")
	f.Bool("keep", false, "With --join: leave an existing Worker of this account unchanged (exit 0) without asking")
	f.Bool("interactive-replace", false, "With --join: show an existing Worker of this account and ask on the terminal whether to replace it (used by install.sh)")
	cmd.MarkFlagsMutuallyExclusive("replace", "keep")
}

func replaceRequested(cmd *cobra.Command) bool {
	for _, n := range []string{"replace", "keep", "interactive-replace"} {
		if f := cmd.Flags().Lookup(n); f != nil && f.Changed {
			return true
		}
	}
	return false
}

func boolFlag(cmd *cobra.Command, name string) bool {
	v, _ := cmd.Flags().GetBool(name)
	return v
}

// existingWorker is what a read-only look finds in a state directory.
type existingWorker struct {
	Account, StateDir string
	// Configured: a config file with settings. Enrolled: a Hub pairing.
	Configured, Enrolled, Paused, Live bool
	Name, HubURL, HubWords             string
}

func (e existingWorker) present() bool { return e.Configured || e.Enrolled || e.Paused || e.Live }

func (e existingWorker) states() string {
	var s []string
	if e.Enrolled {
		s = append(s, "enrolled")
	}
	if e.Configured {
		s = append(s, "configured")
	}
	if e.Paused {
		s = append(s, "paused")
	}
	if e.Live {
		s = append(s, "running")
	} else {
		s = append(s, "not running")
	}
	return strings.Join(s, ", ")
}

// inspectExisting looks at stateDir without creating, migrating or locking
// anything.
func inspectExisting(ctx context.Context, d replaceDeps, account, stateDir string) (existingWorker, error) {
	e := existingWorker{Account: account, StateDir: stateDir}
	if _, err := os.Stat(stateDir); errors.Is(err, fs.ErrNotExist) {
		return e, nil
	} else if err != nil {
		return e, fmt.Errorf("look at %s: %w", stateDir, err)
	}
	cfg, err := config.LoadFile(filepath.Join(stateDir, workerConfigName))
	if err != nil {
		return e, fmt.Errorf("read the config file of the Worker in %s: %w", stateDir, err)
	}
	for _, k := range cfg.Keys() {
		if k != config.KeyStateDir {
			e.Configured = true
		}
	}
	e.HubURL, _, _ = cfg.Lookup(config.KeyServer)
	e.Name, _, _ = cfg.Lookup(config.KeyName)
	if v, ok, _ := cfg.Lookup(config.KeyHubFingerprint); ok {
		if m, err := agent.ParseHubExpectation(v); err == nil {
			e.HubWords = m.Words()
		}
	}
	// A pause marker that cannot be read still counts as a paused Worker.
	e.Paused, _ = agent.ReadPause(stateDir)
	e.Live = d.isLive(stateDir)
	if _, err := os.Stat(filepath.Join(stateDir, store.DBFile)); err == nil {
		if st, err := store.OpenReadOnlyContext(ctx, stateDir); err == nil {
			defer func() { _ = st.Close() }()
			if en, err := st.LoadEnrollment(ctx); err == nil {
				e.Enrolled = true
				if en.Name != "" {
					e.Name = en.Name
				}
				if fp, err := identity.FingerprintOf(en.HubPub); err == nil {
					e.HubWords = fp.Words()
				}
			}
		}
	}
	if e.Name == "" {
		e.Name = "(not named yet)"
	}
	if e.HubURL == "" {
		e.HubURL = "(not saved)"
	}
	if e.HubWords == "" {
		e.HubWords = "(none saved)"
	}
	return e, nil
}

// runWorkerUpReplace is "worker up --join" with --replace, --keep or
// --interactive-replace.
func runWorkerUpReplace(cmd *cobra.Command, deps workerUpDeps, det service.Detection, asUser string, configureOnly bool) error {
	ctx := cmdContext(cmd)
	out := cmd.OutOrStdout()
	stderr := deps.run.stderr
	if stderr == nil {
		stderr = cmd.ErrOrStderr()
	}
	if runningAsRoot() && det.Manager != service.ManagerWindows {
		return errors.New("--replace, --keep and --interactive-replace work in user mode: run the command as the user whose Worker it is, without sudo")
	}
	target, err := service.ResolveTarget(deps.targetEnv, asUser)
	if err != nil {
		return err
	}
	lookupEnv := deps.run.lookupEnv
	if lookupEnv == nil {
		lookupEnv = os.LookupEnv
	}
	reg := deps.registry()
	pe := targetPathEnv(deps, target, lookupEnv)
	stateDir, err := pe.StateDir()
	if err != nil {
		return err
	}
	plain := &config.Resolver{Registry: reg, Role: config.ScopeWorker, Flags: changedFlags{cmd: cmd, reg: reg}, LookupEnv: lookupEnv}
	if r, err := plain.Resolve(config.KeyStateDir); err != nil {
		return err
	} else if r.Source != config.SourceDefault {
		if stateDir, err = filepath.Abs(r.Value); err != nil {
			return fmt.Errorf("setting %q: %w", config.KeyStateDir, err)
		}
	}

	old, err := inspectExisting(ctx, deps.replace, target.User, stateDir)
	if err != nil {
		return err
	}
	if !old.present() {
		return installWorkerService(cmd, deps, det, asUser, configureOnly)
	}

	newHub := joinedSetting(cmd, reg, config.KeyServer)
	newWords := "(unknown)"
	if m, err := agent.ParseHubExpectation(joinedSetting(cmd, reg, config.KeyHubFingerprint)); err == nil && m.Words() != "" {
		newWords = m.Words()
	}
	_, _ = fmt.Fprintf(out, "A Minion Worker is already set up here:\n  Account:    %s\n  State path: %s\n  Name:       %s\n  Hub:        %s\n  Hub key:    %s\n  State:      %s\n",
		old.Account, old.StateDir, old.Name, old.HubURL, old.HubWords, old.states())

	yes, err := decideReplace(cmd, deps.replace, out, newHub, newWords)
	if err != nil {
		return err
	}
	if !yes {
		_, _ = fmt.Fprintln(out, "Nothing was changed: the existing Worker stays as it is.")
		return nil
	}
	return replaceTransaction(cmd, deps, det, asUser, configureOnly, old, stderr)
}

// flagValue reads the value a join token set as a flag of cmd.
func joinedSetting(cmd *cobra.Command, reg *config.Registry, key string) string {
	s, ok := reg.Lookup(key)
	if !ok {
		return ""
	}
	if f := cmd.Flags().Lookup(s.FlagName()); f != nil {
		return f.Value.String()
	}
	return ""
}

// decideReplace answers the question from the flags or, without one, on the
// terminal. Without either it refuses and names the flags.
func decideReplace(cmd *cobra.Command, d replaceDeps, out io.Writer, newHub, newWords string) (bool, error) {
	switch {
	case boolFlag(cmd, "replace"):
		return true, nil
	case boolFlag(cmd, "keep"):
		return false, nil
	}
	tty, err := d.openTTY()
	if err != nil {
		return false, errors.New("a Worker is already set up here and there is no terminal to ask on; nothing was changed. " +
			"Run the command again with --replace to replace it with the new Hub, or --keep to leave it as it is")
	}
	defer func() { _ = tty.Close() }()
	if _, err := fmt.Fprintf(tty, "Replace it with %s (Hub key: %s)? [y/N] ", newHub, newWords); err != nil {
		return false, err
	}
	line, err := readTTYLine(tty)
	if err != nil {
		return false, nil // no answer is No
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true, nil
	}
	return false, nil
}

// readTTYLine reads up to the first newline, one byte at a time.
func readTTYLine(r io.Reader) (string, error) {
	var buf []byte
	one := make([]byte, 1)
	for len(buf) < 256 {
		n, err := r.Read(one)
		if n == 1 {
			if one[0] == '\n' {
				return string(buf), nil
			}
			buf = append(buf, one[0])
		}
		if err != nil {
			if len(buf) > 0 && errors.Is(err, io.EOF) {
				return string(buf), nil
			}
			return "", err
		}
	}
	return string(buf), nil
}

// replaceTransaction stops the old Worker, keeps a copy of its profile,
// clears the trust settings, joins, and either commits or restores.
func replaceTransaction(cmd *cobra.Command, deps workerUpDeps, det service.Detection, asUser string, configureOnly bool, old existingWorker, stderr io.Writer) (err error) {
	ctx := cmdContext(cmd)
	out := cmd.OutOrStdout()
	backup := old.StateDir + replaceBackupSuffix
	if _, err := os.Lstat(backup); err == nil {
		return fmt.Errorf("%s exists: it is the copy of an earlier Worker profile that a replacement did not finish with. "+
			"Nothing was changed. Check it, then move it back over %s or delete it, and run the command again", backup, old.StateDir)
	}
	wasLive := old.Live
	if wasLive {
		if err := deps.replace.stop(ctx, out); err != nil {
			return fmt.Errorf("the old Worker could not be stopped, nothing was changed: %w", err)
		}
		if deps.replace.isLive(old.StateDir) {
			return errors.New("the old Worker still holds its state after it was asked to stop; nothing was changed. Stop it with: minion worker down")
		}
	}
	restart := func() {
		if wasLive {
			if serr := deps.replace.start(ctx, out); serr != nil {
				_, _ = fmt.Fprintf(stderr, "The old Worker could not be started again (%v). Start it with: minion worker start\n", serr)
			}
		}
	}
	if err := copyTree(old.StateDir, backup); err != nil {
		_ = os.RemoveAll(backup)
		restart()
		return fmt.Errorf("could not keep a copy of the old profile, nothing was changed: %w", err)
	}
	rollback := func(cause error) error {
		if rerr := restoreTree(backup, old.StateDir); rerr != nil {
			return fmt.Errorf("%w; the old profile could NOT be restored (%v): its copy is in %s, move it back over %s", cause, rerr, backup, old.StateDir)
		}
		restart()
		_, _ = fmt.Fprintf(stderr, "The join failed. The old Worker profile was restored%s.\n", map[bool]string{true: " and started again", false: ""}[wasLive])
		return fmt.Errorf("%w; the old Worker was kept", cause)
	}
	if err := resetForJoin(ctx, old.StateDir); err != nil {
		return rollback(fmt.Errorf("clearing the old profile failed: %w", err))
	}
	_, _ = fmt.Fprintln(out, "Replacing the Worker: the old profile is kept until the new Worker is live.")
	deps.requireLive = true
	if err := installWorkerService(cmd, deps, det, asUser, configureOnly); err != nil {
		return rollback(err)
	}
	if err := os.RemoveAll(backup); err != nil {
		_, _ = fmt.Fprintf(stderr, "The new Worker is live. The old profile copy %s could not be deleted (%v); delete it yourself.\n", backup, err)
	}
	_, _ = fmt.Fprintln(out, "The Worker was replaced.")
	return nil
}

// resetForJoin clears the profile for a new Hub: the pairing, the share
// choice, the Worker key and the Hub settings of the config file (what
// "worker reset" does), and also the hub_fingerprint pin and the pause marker,
// which "worker reset" keeps.
func resetForJoin(ctx context.Context, stateDir string) error {
	lock, err := agent.LockLifecycle(stateDir)
	if err != nil {
		return err
	}
	defer func() { _ = lock.Close() }()
	st, err := store.OpenContext(ctx, stateDir)
	if err != nil {
		return fmt.Errorf("open the Worker state in %s: %w", stateDir, err)
	}
	defer func() { _ = st.Close() }()
	cfgPath := filepath.Join(stateDir, workerConfigName)
	if _, err := agent.ResetWorker(ctx, agent.ResetOptions{StateDir: stateDir, ConfigPath: cfgPath, Store: st}); err != nil {
		return err
	}
	cfg, err := config.LoadFile(cfgPath)
	if err != nil {
		return err
	}
	if _, ok, _ := cfg.Lookup(config.KeyHubFingerprint); ok {
		cfg.Delete(config.KeyHubFingerprint)
		if err := cfg.Save(); err != nil {
			return fmt.Errorf("write the config file: %w", err)
		}
	}
	if err := os.Remove(filepath.Join(stateDir, agent.PauseFile)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove the pause marker: %w", err)
	}
	return nil
}

// copyTree copies the regular files and directories of src to the new
// directory dst (mode 0700, files keep their mode). Sockets and other special
// files are skipped; a link is recreated as a link.
func copyTree(src, dst string) error {
	if err := os.Mkdir(dst, 0o700); err != nil {
		return err
	}
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil || rel == "." {
			return err
		}
		to := filepath.Join(dst, rel)
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case d.IsDir():
			return os.Mkdir(to, 0o700)
		case info.Mode()&fs.ModeSymlink != 0:
			l, err := os.Readlink(p)
			if err != nil {
				return err
			}
			return os.Symlink(l, to)
		case info.Mode().IsRegular():
			return copyFile(p, to, info.Mode().Perm())
		}
		return nil
	})
}

func copyFile(src, dst string, perm fs.FileMode) (err error) {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := out.Close(); cerr != nil && err == nil {
			err = cerr
		}
	}()
	_, err = io.Copy(out, in)
	return err
}

// restoreTree puts backup back as dir.
func restoreTree(backup, dir string) error {
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	return os.Rename(backup, dir)
}
