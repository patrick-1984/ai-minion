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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/patrick-1984/ai-minion/internal/client"
	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/worker/agent"
	"github.com/patrick-1984/ai-minion/internal/worker/service"
	"github.com/patrick-1984/ai-minion/internal/worker/store"
)

// replaceBackupSuffix names the private copy of the old state directory.
const replaceBackupSuffix = ".replace-backup"

// replaceClearKeys are the config keys that tie a profile to its Hub and that
// the replacement removes besides what "worker reset" removes: the Hub pin, the
// API key file of calls through the old Hub (it must never be sent to the new
// Hub), the accept list of Worker ids of the old Hub and the share lock.
var replaceClearKeys = []string{config.KeyHubFingerprint, config.KeyCallAPIKeyFile, config.KeyPipeAcceptFrom, config.KeyShareLock}

// replaceDeps are the seams of the replace flow that tests replace.
type replaceDeps struct {
	// tty opens the terminal for the question; nil means the real one.
	tty func() (io.ReadWriteCloser, error)
	// stopOld stops the running Worker of this user; startOld starts it again
	// (errNotInstalled when nothing can start it).
	stopOld, startOld func(ctx context.Context, out io.Writer) error
	// live reports whether a Worker holds the state directory.
	live func(stateDir string) bool
	// incumbentDirs returns the state directories of the Worker that runs or
	// is installed for the target, when they are not the default one: the
	// answer of the live Worker and the directory pinned in its service.
	incumbentDirs func(ctx context.Context, home string) []string
	// reload makes the service manager read a restored unit file.
	reload func(ctx context.Context)
	// sleep waits between polls.
	sleep func(time.Duration)
	// owned reports whether the user controls path (nil: the real check).
	owned func(path string) bool
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

func (d replaceDeps) pause(t time.Duration) {
	if d.sleep != nil {
		d.sleep(t)
		return
	}
	time.Sleep(t)
}

// waitState waits until isLive(dir) == want, at most about 15 seconds.
func (d replaceDeps) waitState(dir string, want bool) bool {
	for i := 0; i < 60; i++ {
		if d.isLive(dir) == want {
			return true
		}
		d.pause(250 * time.Millisecond)
	}
	return d.isLive(dir) == want
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
	return runWorkerStart(c, defaultWorkerStartDeps())
}

func (d replaceDeps) reloadUnits(ctx context.Context) {
	if d.reload != nil {
		d.reload(ctx)
		return
	}
	if runtime.GOOS == "linux" {
		_ = exec.CommandContext(ctx, "systemctl", "--user", "daemon-reload").Run()
	}
}

func (d replaceDeps) isOwned(path string) bool {
	if d.owned != nil {
		return d.owned(path)
	}
	return ownedByCurrentUser(path)
}

// defaultIncumbentDirs asks the running Worker for its state directory and
// reads the one pinned in the user's service registration.
func defaultIncumbentDirs(ctx context.Context, home string) []string {
	var out []string
	cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if raw, err := client.Local().Call(cctx, agent.OpLocalStatus, nil); err == nil {
		var reply agent.LiveStatusReply
		if json.Unmarshal(raw, &reply) == nil && reply.StateDir != "" {
			out = append(out, reply.StateDir)
		}
	}
	for _, f := range registrationFiles(home) {
		if b, err := os.ReadFile(f); err == nil {
			if dir := pinnedStateDir(string(b)); dir != "" {
				out = append(out, dir)
			}
		}
	}
	return out
}

// registrationFiles are the files that make a supervisor start the Worker.
func registrationFiles(home string) []string {
	return []string{
		filepath.Join(home, ".config", "systemd", "user", service.UserUnitName),
		service.PlistPath(home),
		filepath.Join(home, ".config", "minion", "worker-login"),
	}
}

var (
	unitStateRE  = regexp.MustCompile(`(?m)^Environment="?MINION_STATE_DIR=([^"\n]+)"?\s*$`)
	plistStateRE = regexp.MustCompile(`<key>MINION_STATE_DIR</key>\s*<string>([^<]+)</string>`)
)

// pinnedStateDir reads the state directory a unit file or plist pins.
func pinnedStateDir(text string) string {
	if m := unitStateRE.FindStringSubmatch(text); m != nil {
		return strings.TrimSpace(m[1])
	}
	if m := plistStateRE.FindStringSubmatch(text); m != nil {
		return strings.TrimSpace(m[1])
	}
	return ""
}

// fileSnap is a registration file as it was before the replacement.
type fileSnap struct {
	path    string
	data    []byte
	perm    fs.FileMode
	existed bool
}

func snapFiles(paths []string) []fileSnap {
	var out []fileSnap
	for _, p := range paths {
		sn := fileSnap{path: p}
		if info, err := os.Stat(p); err == nil && info.Mode().IsRegular() {
			if b, err := os.ReadFile(p); err == nil {
				sn.data, sn.perm, sn.existed = b, info.Mode().Perm(), true
			}
		}
		out = append(out, sn)
	}
	return out
}

func restoreFiles(snaps []fileSnap) error {
	var first error
	for _, sn := range snaps {
		var err error
		if sn.existed {
			err = os.WriteFile(sn.path, sn.data, sn.perm)
		} else if rerr := os.Remove(sn.path); rerr != nil && !errors.Is(rerr, fs.ErrNotExist) {
			err = rerr
		}
		if err != nil && first == nil {
			first = err
		}
	}
	return first
}

// addReplaceFlags adds the flags of the replace flow to "worker up".
func addReplaceFlags(cmd *cobra.Command) {
	f := cmd.Flags()
	f.Bool("replace", false, "With --join: replace an existing Worker of this account without asking")
	f.Bool("keep", false, "With --join: leave an existing Worker of this account unchanged (exit 0) without asking")
	f.Bool("interactive-replace", false, "With --join: show an existing Worker of this account and ask on the terminal whether to replace it (used by install.sh)")
	cmd.MarkFlagsMutuallyExclusive("replace", "keep")
	f.Bool("legacy-join", false, "With --join: keep the old behaviour for a Worker that is set up for another Hub (an error), without the replace question (used by the tray)")
	_ = f.MarkHidden("legacy-join")
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
	// rawURL and rawWords are what is saved, without the placeholders.
	rawURL, rawWords string
}

// sameHub reports whether the saved Hub is the one of the join token.
func (e existingWorker) sameHub(url, words string) bool {
	return e.rawURL != "" && e.rawWords != "" && sameHubURL(e.rawURL, url) && e.rawWords == words
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
	e.rawURL, e.rawWords = e.HubURL, e.HubWords
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
// --interactive-replace (explicit), or a plain join over a Worker that is set
// up for another Hub (then the question is asked too, unless --legacy-join).
// handled is false when the caller continues with the ordinary join.
func runWorkerUpReplace(cmd *cobra.Command, deps workerUpDeps, det service.Detection, asUser string, configureOnly, explicit bool) (handled bool, err error) {
	ctx := cmdContext(cmd)
	out := cmd.OutOrStdout()
	stderr := deps.run.stderr
	if stderr == nil {
		stderr = cmd.ErrOrStderr()
	}
	windows := runtime.GOOS == "windows" || det.Manager == service.ManagerWindows
	if explicit {
		switch {
		case windows:
			return true, errors.New("--replace, --keep and --interactive-replace are not available on Windows yet: the Worker key's private access rules would not survive a copy. Run `minion worker reset` and then the join command")
		case runningAsRoot():
			return true, errors.New("--replace, --keep and --interactive-replace work in user mode: run the command as the user whose Worker it is, without sudo")
		case configureOnly:
			return true, errors.New("--replace, --keep and --interactive-replace do not work with --configure-only: nothing would start the new Worker to prove it is live, so the old one could not be restored")
		}
	} else if windows || runningAsRoot() || configureOnly || boolFlag(cmd, "legacy-join") {
		return false, nil
	}
	target, err := service.ResolveTarget(deps.targetEnv, asUser)
	if err != nil {
		return true, err
	}
	lookupEnv := deps.run.lookupEnv
	if lookupEnv == nil {
		lookupEnv = os.LookupEnv
	}
	reg := deps.registry()
	pe := targetPathEnv(deps, target, lookupEnv)
	stateDir, err := pe.StateDir()
	if err != nil {
		return true, err
	}
	plain := &config.Resolver{Registry: reg, Role: config.ScopeWorker, Flags: changedFlags{cmd: cmd, reg: reg}, LookupEnv: lookupEnv}
	stateRes, err := plain.Resolve(config.KeyStateDir)
	if err != nil {
		return true, err
	}
	explicitState := stateRes.Source != config.SourceDefault
	if explicitState {
		if stateDir, err = filepath.Abs(stateRes.Value); err != nil {
			return true, fmt.Errorf("setting %q: %w", config.KeyStateDir, err)
		}
	}
	// The incumbent may keep its state elsewhere than the default: the live
	// Worker says where, and so does the directory pinned in its service. That
	// directory is the one that is asked about and replaced, never silently
	// switched away from.
	incumbent := deps.replace.incumbentDirs
	if incumbent == nil {
		incumbent = defaultIncumbentDirs
	}
	canon := func(dir string) (string, error) { return canonicalStateDir(dir, deps.replace) }
	if stateDir, err = canon(stateDir); err != nil {
		return true, err
	}
	for _, d := range incumbent(ctx, target.Home) {
		real, err := canon(d)
		if err != nil {
			return true, err
		}
		if real == stateDir {
			continue
		}
		if explicitState {
			return true, fmt.Errorf("a Worker is set up for this account with its state in %s, but the command names %s; nothing was changed. "+
				"Run it again with --state-dir %s to replace that Worker", real, stateDir, real)
		}
		if err := cmd.Flags().Set("state-dir", real); err != nil {
			return true, err
		}
		stateDir, explicitState = real, true
	}

	// One replacement at a time per state directory, from here to the end.
	if _, err := os.Stat(stateDir); err == nil {
		unlock, err := lockConfigFileContext(ctx, filepath.Join(filepath.Dir(stateDir), ".minion-replace.lock"))
		if err != nil {
			return true, err
		}
		defer unlock()
	}
	old, err := inspectExisting(ctx, deps.replace, target.User, stateDir)
	if err != nil {
		return true, err
	}
	newHub := joinedSetting(cmd, reg, config.KeyServer)
	newWords := "(unknown)"
	if m, err := agent.ParseHubExpectation(joinedSetting(cmd, reg, config.KeyHubFingerprint)); err == nil && m.Words() != "" {
		newWords = m.Words()
	}
	if !old.present() || (!explicit && old.sameHub(newHub, newWords)) {
		return false, nil
	}

	_, _ = fmt.Fprintf(out, "A Minion Worker is already set up here:\n  Account:    %s\n  State path: %s\n  Name:       %s\n  Hub:        %s\n  Hub key:    %s\n  State:      %s\n",
		old.Account, old.StateDir, old.Name, old.HubURL, old.HubWords, old.states())

	yes, err := decideReplace(ctx, cmd, deps.replace, out, newHub, newWords)
	if err != nil {
		return true, err
	}
	if !yes {
		_, _ = fmt.Fprintln(out, "Nothing was changed: the existing Worker stays as it is.")
		return true, errReplaceDeclined
	}
	return true, replaceTransaction(cmd, deps, det, asUser, configureOnly, old, target, stderr)
}

// errReplaceDeclined is returned by the flow when the person kept the existing
// Worker: it is a success for the exit status ("worker up" maps it to 0), and
// the installer tells it from a finished join by a marker on standard output.
var errReplaceDeclined = errors.New("replacement declined")

// declinedMarker is the line "worker up" prints after a decline, for install.sh.
const declinedMarker = "minion: join skipped: existing Worker kept"

// canonicalStateDir resolves symlinks of an existing state directory once and
// refuses one the user does not control (a link to a place others can change).
func canonicalStateDir(dir string, d replaceDeps) (string, error) {
	real, err := filepath.EvalSymlinks(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return dir, nil
	}
	if err != nil {
		return "", fmt.Errorf("resolve the state directory %s: %w", dir, err)
	}
	if real != dir && !d.isOwned(real) {
		return "", fmt.Errorf("the state directory %s is a link to %s, which you do not control; nothing was changed", dir, real)
	}
	return real, nil
}

// joinedSetting reads the value a join token set as a flag of cmd.
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
// terminal. Without either it refuses and names the flags. The read ends with
// ctx (Ctrl-C): the terminal is closed and the command stops with status 130.
func decideReplace(ctx context.Context, cmd *cobra.Command, d replaceDeps, out io.Writer, newHub, newWords string) (bool, error) {
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
	type answer struct {
		line string
		err  error
	}
	ch := make(chan answer, 1)
	go func() {
		l, err := readTTYLine(tty)
		ch <- answer{l, err}
	}()
	select {
	case <-ctx.Done():
		_ = tty.Close() // ends the read
		_, _ = fmt.Fprintln(out)
		return false, &exitCodeError{130}
	case a := <-ch:
		if a.err != nil {
			return false, nil // no answer is No
		}
		switch strings.ToLower(strings.TrimSpace(a.line)) {
		case "y", "yes":
			return true, nil
		}
		return false, nil
	}
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

// replaceTransaction stops the old Worker, keeps a copy of its profile, clears
// the trust settings, joins, and either commits or restores.
func replaceTransaction(cmd *cobra.Command, deps workerUpDeps, det service.Detection, asUser string, configureOnly bool, old existingWorker, target service.Target, stderr io.Writer) (err error) {
	ctx := cmdContext(cmd)
	out := cmd.OutOrStdout()
	rd := deps.replace
	backup := old.StateDir + replaceBackupSuffix
	if _, err := os.Lstat(backup); err == nil {
		return fmt.Errorf("%s exists: it is the copy of an earlier Worker profile that a replacement did not finish with. "+
			"Nothing was changed. Check it, then move it back over %s or delete it, and run the command again", backup, old.StateDir)
	}
	wasLive := old.Live
	regSnap := snapFiles(registrationFiles(target.Home))
	if wasLive {
		if err := rd.stop(ctx, out); err != nil {
			return fmt.Errorf("the old Worker could not be stopped, nothing was changed: %w", err)
		}
		if !rd.waitState(old.StateDir, false) {
			return errors.New("the old Worker still holds its state after it was asked to stop; nothing was changed. Stop it with: minion worker down")
		}
	}
	// restart brings the old Worker back the way it was launched and reports
	// only what it verified.
	restart := func() string {
		rd.reloadUnits(ctx)
		if !wasLive {
			return ""
		}
		switch serr := rd.start(ctx, out); {
		case errors.Is(serr, errNotInstalled):
			return "It was running in the foreground, so nothing can start it for you: run `minion worker up --foreground` to start it again."
		case serr != nil:
			return fmt.Sprintf("It could not be started again (%v): run `minion worker start`.", serr)
		}
		if !rd.waitState(old.StateDir, true) {
			return "It did not come back within 15 seconds: check it with `minion worker status`."
		}
		return "The old Worker is running again."
	}
	created, err := copyTree(old.StateDir, backup)
	if err != nil {
		if created { // only a copy this run made is removed
			_ = os.RemoveAll(backup)
		}
		restart()
		return fmt.Errorf("could not keep a copy of the old profile, nothing was changed: %w", err)
	}
	rollback := func(cause error) error {
		// Whatever the failed join started (a unit, a login entry, a launchd
		// agent) stops first, so that nothing serves the new Hub from the
		// directory that is about to be replaced.
		_ = rd.stop(ctx, out)
		if !rd.waitState(old.StateDir, false) {
			return fmt.Errorf("%w; the new Worker could not be stopped, so the old profile was NOT restored: its copy is in %s", cause, backup)
		}
		if rerr := restoreTree(backup, old.StateDir); rerr != nil {
			return fmt.Errorf("%w; the old profile could NOT be restored (%v): its copy is in %s, move it back over %s", cause, rerr, backup, old.StateDir)
		}
		if rerr := restoreFiles(regSnap); rerr != nil {
			_, _ = fmt.Fprintf(stderr, "The old service registration could not be restored (%v); run `minion worker up` for the old Hub.\n", rerr)
		}
		msg := restart()
		_, _ = fmt.Fprintf(stderr, "The join failed and the old Worker profile was restored. %s\n", msg)
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
// "worker reset" does), and also every setting that ties the profile to the old
// Hub (replaceClearKeys), and the pause marker, which "worker reset" keeps.
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
	changed := false
	for _, k := range replaceClearKeys {
		if _, ok, _ := cfg.Lookup(k); ok {
			cfg.Delete(k)
			changed = true
		}
	}
	if changed {
		if err := cfg.Save(); err != nil {
			return fmt.Errorf("write the config file: %w", err)
		}
	}
	if err := os.Remove(filepath.Join(stateDir, agent.PauseFile)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove the pause marker: %w", err)
	}
	return nil
}

// copyTree copies the regular files and directories of src (a real directory,
// not a link) to the new directory dst (created exclusively, mode 0700, files
// keep their mode); created says whether dst was made by this call. Sockets and other special
// files are skipped; a link is recreated as a link.
func copyTree(src, dst string) (created bool, err error) {
	if err := os.Mkdir(dst, 0o700); err != nil {
		return false, err // exists or cannot be made: it is not ours to remove
	}
	return true, filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
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
