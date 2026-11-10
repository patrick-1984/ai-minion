package service

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// TemplateUnitName is the file name of the systemd template unit that Story
// 9.1 ships and that `minion worker up` installs when no package did.
const TemplateUnitName = "minion-worker@.service"

// UserUnitName is the file name of the per-user unit.
const UserUnitName = "minion-worker.service"

// DefaultBinary is the path the deb and rpm packages install the binary to;
// the shipped unit names it in ExecStart.
const DefaultBinary = "/usr/bin/minion"

// SystemUnitFor returns the unit name for one Unix user:
// minion-worker@<user>.service.
func SystemUnitFor(user string) string { return "minion-worker@" + user + ".service" }

// systemUnitTemplate is the system template unit. It must stay identical to
// deploy/systemd/minion-worker@.service, which Story 9.1 owns (A-6): a unit
// test compares the two for the default binary path. ExecStart is the only
// line that depends on the install.
const systemUnitTemplate = `# Minion Worker, one instance per Unix user: minion-worker@<user>.service
# runs "minion worker run" as <user>, so that the Worker sees the tmux
# sessions of that user (AD-24, A-6). This file is created and owned by
# Story 9.1, shipped in the deb and rpm packages as
# /usr/lib/systemd/system/minion-worker@.service and validated by Story 4.11
# ("minion worker up" installs exactly this content). Do not edit it anywhere
# else.
[Unit]
Description=Minion Worker for %i
Documentation=https://github.com/patrick-1984/ai-minion
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=%i
WorkingDirectory=~
ExecStart={{BINARY}} worker run
Restart=always
RestartSec=5
# Stopping or restarting the Worker must not end the tmux sessions of the user:
# only the Worker process is signalled, a tmux server stays alive.
KillMode=process

[Install]
WantedBy=multi-user.target
`

// userUnitTemplate is the per-user unit: the same service without the User=
// line (the user manager already runs as that user) and wanted by the
// default target of the user manager.
const userUnitTemplate = `# Minion Worker as a systemd user service, installed by "minion worker up".
# It runs "minion worker run" as the user that owns this unit; it starts at
# boot only when lingering is enabled for the user (loginctl enable-linger).
[Unit]
Description=Minion Worker
Documentation=https://github.com/patrick-1984/ai-minion

[Service]
Type=simple
WorkingDirectory=~
ExecStart={{BINARY}} worker run
{{STATE_ENV}}Restart=always
RestartSec=5
# Stopping or restarting the Worker must not end the tmux sessions of the user:
# only the Worker process is signalled, a tmux server stays alive.
KillMode=process

[Install]
WantedBy=default.target
`

// SystemUnit returns the content of the system template unit for the binary
// at the absolute path binary.
func SystemUnit(binary string) (string, error) {
	if err := validateBinary(binary); err != nil {
		return "", err
	}
	return strings.Replace(systemUnitTemplate, "{{BINARY}}", binary, 1), nil
}

// UserUnit returns the content of the per-user unit for the binary at binary.
// A non-empty stateDir is pinned with Environment=MINION_STATE_DIR so that the
// service reads the state directory "worker up" wrote, whatever the
// environment of the user manager holds (a shell's XDG_STATE_HOME is not part
// of it).
func UserUnit(binary, stateDir string) (string, error) {
	if err := validateBinary(binary); err != nil {
		return "", err
	}
	env, err := stateEnvLine(stateDir)
	if err != nil {
		return "", err
	}
	u := strings.Replace(userUnitTemplate, "{{BINARY}}", binary, 1)
	return strings.Replace(u, "{{STATE_ENV}}", env, 1), nil
}

// stateEnvName is the environment variable of the state directory setting.
const stateEnvName = "MINION_STATE_DIR"

// stateEnvLine returns the unit line that sets the state directory, or ""
// for an empty stateDir.
func stateEnvLine(stateDir string) (string, error) {
	if stateDir == "" {
		return "", nil
	}
	if err := validateStateDir(stateDir); err != nil {
		return "", err
	}
	return "Environment=" + systemdQuote(stateEnvName+"="+stateDir) + "\n", nil
}

// validateStateDir accepts an absolute path without control characters.
func validateStateDir(dir string) error {
	if !strings.HasPrefix(dir, "/") {
		return fmt.Errorf("the state directory %q is not an absolute path", dir)
	}
	for _, r := range dir {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("the state directory %q holds a control character", dir)
		}
	}
	return nil
}

// systemdQuote quotes s for an Environment= assignment: double quotes, with
// backslash and quote escaped and % doubled (specifier expansion).
func systemdQuote(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "%", "%%")
	return `"` + r.Replace(s) + `"`
}

// SystemDropIn returns the content of the drop-in of one system service
// instance, or "" when the instance needs none. The packaged template is the
// same for every user; what depends on the install (the binary when it is not
// DefaultBinary, a pinned state directory) lives in the instance's drop-in, so
// installing for one user never changes the service of another.
func SystemDropIn(binary, stateDir string) (string, error) {
	if err := validateBinary(binary); err != nil {
		return "", err
	}
	env, err := stateEnvLine(stateDir)
	if err != nil {
		return "", err
	}
	if binary == DefaultBinary && env == "" {
		return "", nil
	}
	var b strings.Builder
	b.WriteString("# Settings of this instance, written by \"minion worker up\".\n[Service]\n")
	if binary != DefaultBinary {
		b.WriteString("ExecStart=\nExecStart=" + binary + " worker run\n")
	}
	b.WriteString(env)
	return b.String(), nil
}

// dropInName is the file name of the instance drop-in.
const dropInName = "10-minion-worker-up.conf"

// binaryRE allows the characters that need no quoting in a unit or a plist.
var binaryRE = regexp.MustCompile(`^/[A-Za-z0-9/._+@,:=-]+$`)

func validateBinary(binary string) error {
	if !binaryRE.MatchString(binary) {
		return fmt.Errorf("the binary path %q is not an absolute path of plain characters (letters, digits and /._+@,:=-); copy minion to a place like /usr/local/bin and run it from there", binary)
	}
	return nil
}

// userRE restricts the account names that become part of a unit instance name,
// so that no escaping is needed.
var userRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.-]{0,63}$`)

func validateUser(name string) error {
	if !userRE.MatchString(name) {
		return fmt.Errorf("the user name %q cannot be used in a service name (letters, digits and _.- only)", name)
	}
	return nil
}

// LingerError is returned when the systemd user service is installed but
// lingering is not enabled for the user: the service would stop when the user
// logs out and would not start at boot. Command is what to run.
type LingerError struct {
	User    string
	Command string
}

func (e *LingerError) Error() string {
	return fmt.Sprintf("lingering is not enabled for %s, so the Worker would stop at logout and not start at boot; run this command once, then run minion worker up again: %s", e.User, e.Command)
}

func (d Deps) installSystemd(ctx context.Context, req Request) (Result, error) {
	if req.Scope == ScopeUser {
		return d.installSystemdUser(ctx, req)
	}
	return d.installSystemdSystem(ctx, req)
}

func (d Deps) installSystemdSystem(ctx context.Context, req Request) (Result, error) {
	if err := validateUser(req.Target.User); err != nil {
		return Result{}, err
	}
	// The template is the same for every user (the packaged one); the binary
	// and a pinned state directory go into the drop-in of this instance.
	unit, err := SystemUnit(DefaultBinary)
	if err != nil {
		return Result{}, err
	}
	pin := ""
	if req.PinStateDir {
		pin = req.StateDir
	}
	dropIn, err := SystemDropIn(req.Binary, pin)
	if err != nil {
		return Result{}, err
	}
	name := SystemUnitFor(req.Target.User)
	path := filepath.Join(d.systemUnitDir(), TemplateUnitName)

	// A package already installed this exact unit: leave it alone. A unit in
	// the system directory takes precedence over the packaged one, so a stale
	// copy that an earlier "worker up" wrote there must not stay.
	installedBy := d.identicalUnit(unit)
	if installedBy == "" {
		if err := writeIfChanged(path, unit, 0o755, 0o644); err != nil {
			return Result{}, fmt.Errorf("install the unit: %w", err)
		}
		installedBy = path
	} else if data, err := os.ReadFile(path); err == nil && string(data) != unit {
		if err := os.Remove(path); err != nil {
			return Result{}, fmt.Errorf("remove the stale unit %s that overrides the packaged one: %w", path, err)
		}
	}
	dropInPath := filepath.Join(d.systemUnitDir(), name+".d", dropInName)
	if dropIn == "" {
		if err := os.Remove(dropInPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return Result{}, fmt.Errorf("remove the stale drop-in %s: %w", dropInPath, err)
		}
		_ = os.Remove(filepath.Dir(dropInPath)) // only when empty
	} else if err := writeIfChanged(dropInPath, dropIn, 0o755, 0o644); err != nil {
		return Result{}, fmt.Errorf("install the drop-in of %s: %w", name, err)
	}
	for _, step := range [][]string{
		{"daemon-reload"},
		{"enable", name},
		// restart starts a stopped service and restarts a running one, so
		// that running "worker up" again applies the new settings.
		{"restart", name},
	} {
		if err := d.systemctl(ctx, false, step...); err != nil {
			return Result{}, err
		}
	}
	if err := d.waitActive(ctx, false, name); err != nil {
		return Result{}, err
	}
	return Result{Manager: ManagerSystemd, Scope: ScopeSystem, Unit: name, Path: installedBy}, nil
}

func (d Deps) installSystemdUser(ctx context.Context, req Request) (Result, error) {
	if err := validateUser(req.Target.User); err != nil {
		return Result{}, err
	}
	if d.getenv("XDG_RUNTIME_DIR") == "" {
		return Result{}, errors.New("no systemd user session found (XDG_RUNTIME_DIR is not set); log in at the machine or over ssh and run this again, or run it with sudo for a system service")
	}
	pin := ""
	if req.PinStateDir {
		pin = req.StateDir
	}
	unit, err := UserUnit(req.Binary, pin)
	if err != nil {
		return Result{}, err
	}
	path := filepath.Join(req.Target.Home, ".config", "systemd", "user", UserUnitName)
	if err := writeIfChanged(path, unit, 0o755, 0o644); err != nil {
		return Result{}, fmt.Errorf("install the user unit: %w", err)
	}
	for _, step := range [][]string{{"daemon-reload"}, {"enable", UserUnitName}, {"restart", UserUnitName}} {
		if err := d.systemctl(ctx, true, step...); err != nil {
			return Result{}, err
		}
	}
	if err := d.waitActive(ctx, true, UserUnitName); err != nil {
		return Result{}, err
	}
	res := Result{Manager: ManagerSystemd, Scope: ScopeUser, Unit: UserUnitName, Path: path}
	return res, d.ensureLinger(ctx, req.Target.User)
}

// ensureLinger makes sure the user's services run without a login session. It
// attempts to enable lingering, checks the result and, when that did not work
// (usually a missing privilege), returns a [LingerError] with the exact
// command to run.
func (d Deps) ensureLinger(ctx context.Context, user string) error {
	if d.lingerEnabled(ctx, user) {
		return nil
	}
	_, _ = d.Runner.Run(ctx, "loginctl", "enable-linger", user)
	if d.lingerEnabled(ctx, user) {
		return nil
	}
	return &LingerError{User: user, Command: "sudo loginctl enable-linger " + user}
}

func (d Deps) lingerEnabled(ctx context.Context, user string) bool {
	out, err := d.Runner.Run(ctx, "loginctl", "show-user", user, "--property=Linger")
	if err != nil {
		return false
	}
	return strings.TrimSpace(out) == "Linger=yes"
}

func (d Deps) systemctl(ctx context.Context, userManager bool, args ...string) error {
	full := args
	if userManager {
		full = append([]string{"--user"}, args...)
	}
	if out, err := d.Runner.Run(ctx, "systemctl", full...); err != nil {
		return fmt.Errorf("systemctl %s failed: %w%s", strings.Join(full, " "), err, tail(out))
	}
	return nil
}

// waitActive polls until the unit is active and still active after a short
// settle time (a Worker that crashes at once restarts every five seconds and
// would show "active" for a moment). It fails at once when the unit failed.
func (d Deps) waitActive(ctx context.Context, userManager bool, unit string) error {
	polls := d.activePolls()
	args := []string{"is-active", unit}
	if userManager {
		args = append([]string{"--user"}, args...)
	}
	state := ""
	for i := 0; i < polls; i++ {
		out, _ := d.Runner.Run(ctx, "systemctl", args...)
		state = strings.TrimSpace(out)
		switch state {
		case "active":
			d.sleep(ctx, d.settle())
			again, _ := d.Runner.Run(ctx, "systemctl", args...)
			if strings.TrimSpace(again) == "active" {
				return nil
			}
			state = strings.TrimSpace(again)
		case "failed":
			return fmt.Errorf("the service %s failed to start; see: %s", unit, journalHint(userManager, unit))
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		d.sleep(ctx, d.pollInterval())
	}
	if state == "" {
		state = "unknown"
	}
	return fmt.Errorf("the service %s is not active (state: %s); see: %s", unit, state, journalHint(userManager, unit))
}

func journalHint(userManager bool, unit string) string {
	if userManager {
		return "journalctl --user -u " + unit + " -n 50"
	}
	return "journalctl -u " + unit + " -n 50"
}

// identicalUnit returns the path of a packaged template unit whose content is
// exactly unit, or "".
func (d Deps) identicalUnit(unit string) string {
	for _, dir := range d.packagedUnitDirs() {
		p := filepath.Join(dir, TemplateUnitName)
		if data, err := os.ReadFile(p); err == nil && string(data) == unit {
			return p
		}
	}
	return ""
}

// writeIfChanged writes content to path (creating the directory) unless the
// file already holds exactly that.
func writeIfChanged(path, content string, dirMode, fileMode fs.FileMode) error {
	if data, err := os.ReadFile(path); err == nil && bytes.Equal(data, []byte(content)) {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), dirMode); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".minion-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.WriteString(content); err != nil {
		_ = tmp.Close()
		_ = os.Remove(name)
		return err
	}
	if err := tmp.Chmod(fileMode); err != nil {
		_ = tmp.Close()
		_ = os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)
		return err
	}
	if err := os.Rename(name, path); err != nil {
		_ = os.Remove(name)
		return err
	}
	return nil
}

// tail formats the end of a command's output for an error message.
func tail(out string) string {
	out = strings.TrimSpace(out)
	if out == "" {
		return ""
	}
	if len(out) > 400 {
		out = out[len(out)-400:]
	}
	return ": " + out
}
