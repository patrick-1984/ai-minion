package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"

	ksvc "github.com/kardianos/service"
)

// Runner runs an external program and returns its standard output. When the
// program fails the error is non-nil and the output holds what it printed
// (standard error included), because "systemctl is-active" reports the state
// on stdout and fails for every state but "active".
type Runner interface {
	Run(ctx context.Context, name string, args ...string) (string, error)
}

// ExecRunner runs programs with os/exec.
type ExecRunner struct{}

// Run implements [Runner].
func (ExecRunner) Run(ctx context.Context, name string, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	return string(out), err
}

// Status is the state of a service of the Windows service control manager.
type Status int

const (
	// StatusUnknown is any state that is not one of the others.
	StatusUnknown Status = iota
	// StatusRunning means the service runs.
	StatusRunning
	// StatusStopped means the service is installed but not running.
	StatusStopped
)

// Controller controls one service of the platform service manager that
// kardianos/service wraps. It is the port that keeps the library out of the
// tests.
type Controller interface {
	Install() error
	Start() error
	Status() (Status, error)
	Uninstall() error
}

// WindowsServiceName is the name of the Windows service.
const WindowsServiceName = "MinionWorker"

// WindowsServiceCommand is the hidden subcommand of "minion worker" that the
// Windows service runs: the Worker under the service control manager (see
// [RunUnderSCM]).
const WindowsServiceCommand = "service-run"

// noopProgram satisfies kardianos' Interface for install and control calls;
// the process that kardianos starts is `minion worker service-run`, not this
// one.
type noopProgram struct{}

func (noopProgram) Start(ksvc.Service) error { return nil }
func (noopProgram) Stop(ksvc.Service) error  { return nil }

// KardianosConfig returns the kardianos/service configuration of the Worker:
// it runs `<binary> worker service-run`, the entry point that connects to the
// service control manager.
func KardianosConfig(binary string) *ksvc.Config {
	return &ksvc.Config{
		Name:        WindowsServiceName,
		DisplayName: "Minion Worker",
		Description: "Minion Worker: shares terminal sessions with a Minion Hub.",
		Executable:  binary,
		Arguments:   []string{"worker", WindowsServiceCommand},
	}
}

type kardianosController struct{ svc ksvc.Service }

// NewKardianosController returns the [Controller] for the Worker service of
// the platform service manager.
func NewKardianosController(binary string) (Controller, error) {
	s, err := ksvc.New(noopProgram{}, KardianosConfig(binary))
	if err != nil {
		return nil, err
	}
	return kardianosController{svc: s}, nil
}

func (c kardianosController) Install() error   { return c.svc.Install() }
func (c kardianosController) Start() error     { return c.svc.Start() }
func (c kardianosController) Uninstall() error { return c.svc.Uninstall() }
func (c kardianosController) Status() (Status, error) {
	s, err := c.svc.Status()
	if err != nil {
		return StatusUnknown, err
	}
	switch s {
	case ksvc.StatusRunning:
		return StatusRunning, nil
	case ksvc.StatusStopped:
		return StatusStopped, nil
	}
	return StatusUnknown, nil
}

// Request describes one install.
type Request struct {
	Manager Manager
	Scope   Scope
	// Binary is the absolute path of the minion binary the service runs.
	Binary string
	// Target is the user the Worker runs as.
	Target Target
	// StateDir is the Worker state directory of the target.
	StateDir string
	// PinStateDir writes StateDir into the service (MINION_STATE_DIR) because
	// it is not the directory the service would resolve without help: the
	// service manager does not carry the environment of the shell that ran
	// "worker up".
	PinStateDir bool
}

// Result reports what was installed.
type Result struct {
	Manager Manager
	Scope   Scope
	// Unit is the unit, label or service name.
	Unit string
	// Path is the unit or plist file, empty for the Windows service.
	Path string
	// Note is extra text for the operator, printed after the description;
	// empty for most installs.
	Note string
}

// Describe returns a one-line, human-readable report.
func (r Result) Describe() string {
	what := string(r.Manager) + " " + string(r.Scope) + " service " + r.Unit
	if r.Path != "" {
		what += " (" + r.Path + ")"
	}
	return "Installed " + what + "; it is running and starts at boot or login, and restarts by itself."
}

// describeWithoutLinger reports an install whose lingering is missing: the
// service runs now but does not survive a logout and does not start at boot.
func (r Result) describeWithoutLinger() string {
	what := string(r.Manager) + " " + string(r.Scope) + " service " + r.Unit
	if r.Path != "" {
		what += " (" + r.Path + ")"
	}
	return "Installed " + what + "; it is running now, but it stops when you log out because lingering is not enabled."
}

// Deps are the ports and settings of [Install]. Every field has a default
// (see the accessors); tests replace them.
type Deps struct {
	Runner Runner
	// Chown, when set, gives files created for the target user to that user
	// (needed when the command runs as root). Its presence also means that the
	// directories of the target are opened through [OpenTrustedDir].
	Chown Chowner
	// LinkOwner is the user whose symbolic links may be followed in the path
	// of a directory of the target; the zero value is root (see [DirSpec]).
	LinkOwner int
	// NewController creates the Windows service controller from the binary
	// path alone; when set it wins over NewControllerFromConfig (the
	// settings are then the controller's business). Nil means the path
	// below.
	NewController func(binary string) (Controller, error)
	// NewControllerFromConfig creates the Windows service controller of the
	// configuration [WindowsConfig] built; nil means [NewConfigController].
	NewControllerFromConfig func(cfg *ksvc.Config) (Controller, error)
	// UserSID returns the SID of the user that installs the Windows service;
	// nil means [CurrentUserSID].
	UserSID func() (string, error)
	// Getenv reads the environment (XDG_RUNTIME_DIR); nil means empty.
	Getenv func(string) string
	// Sleep waits; nil means time.Sleep honouring ctx.
	Sleep func(ctx context.Context, d time.Duration)

	// SystemUnitDir is where a system unit is written; empty means
	// /etc/systemd/system.
	SystemUnitDir string
	// PackagedUnitDirs are the directories where a package ships the unit;
	// nil means /usr/lib/systemd/system and /lib/systemd/system.
	PackagedUnitDirs []string
	// ActivePolls and PollInterval bound the wait for the service to become
	// active; Settle is the pause before the second check. Zero values mean
	// 20 polls, 500 ms and 1 s.
	ActivePolls  int
	PollInterval time.Duration
	Settle       time.Duration
}

// DefaultDeps returns the Deps for the real machine.
func DefaultDeps() Deps {
	return Deps{Runner: ExecRunner{}, Chown: OSChowner{}}
}

func (d Deps) getenv(k string) string {
	if d.Getenv == nil {
		return ""
	}
	return d.Getenv(k)
}

func (d Deps) systemUnitDir() string {
	if d.SystemUnitDir != "" {
		return d.SystemUnitDir
	}
	return "/etc/systemd/system"
}

func (d Deps) packagedUnitDirs() []string {
	if d.PackagedUnitDirs != nil {
		return d.PackagedUnitDirs
	}
	return []string{"/usr/lib/systemd/system", "/lib/systemd/system"}
}

func (d Deps) activePolls() int {
	if d.ActivePolls > 0 {
		return d.ActivePolls
	}
	return 20
}

func (d Deps) pollInterval() time.Duration {
	if d.PollInterval > 0 {
		return d.PollInterval
	}
	return 500 * time.Millisecond
}

func (d Deps) settle() time.Duration {
	if d.Settle > 0 {
		return d.Settle
	}
	return time.Second
}

func (d Deps) sleep(ctx context.Context, dur time.Duration) {
	if d.Sleep != nil {
		d.Sleep(ctx, dur)
		return
	}
	t := time.NewTimer(dur)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// Install installs the Worker as a service of the manager in req, enables it
// at boot or login, starts it and returns after the manager reports it
// active. A systemd user install whose lingering cannot be enabled returns the
// [Result] together with a [*LingerError].
func Install(ctx context.Context, d Deps, req Request, out io.Writer) (Result, error) {
	if d.Runner == nil {
		d.Runner = ExecRunner{}
	}
	var (
		res Result
		err error
	)
	switch req.Manager {
	case ManagerSystemd:
		res, err = d.installSystemd(ctx, req)
	case ManagerLaunchd:
		res, err = d.installLaunchd(ctx, req)
	case ManagerWindows:
		res, err = d.installController(ctx, req)
	default:
		return Result{}, fmt.Errorf("no service manager to install into (%q); use --foreground", req.Manager)
	}
	if res.Unit != "" && out != nil {
		switch {
		case err == nil:
			_, _ = io.WriteString(out, res.Describe()+"\n")
			if res.Note != "" {
				_, _ = io.WriteString(out, res.Note+"\n")
			}
		case isLinger(err):
			_, _ = io.WriteString(out, res.describeWithoutLinger()+"\n")
		}
	}
	return res, err
}

func isLinger(err error) bool {
	var le *LingerError
	return errors.As(err, &le)
}

func (d Deps) installController(ctx context.Context, req Request) (Result, error) {
	if err := validateBinaryAny(req.Binary); err != nil {
		return Result{}, err
	}
	c, err := d.windowsController(req)
	if err != nil {
		return Result{}, fmt.Errorf("service manager: %w", err)
	}
	// An existing installation is replaced so that the settings of this run
	// apply; "not installed" is not an error.
	_ = c.Uninstall()
	if err := c.Install(); err != nil {
		return Result{}, windowsInstallError("install", err)
	}
	if err := c.Start(); err != nil {
		return Result{}, windowsInstallError("start", err)
	}
	var state Status
	for i := 0; i < d.activePolls(); i++ {
		if state, err = c.Status(); err == nil && state == StatusRunning {
			return Result{Manager: ManagerWindows, Scope: ScopeSystem, Unit: WindowsServiceName, Note: windowsServiceNote}, nil
		}
		if cerr := ctx.Err(); cerr != nil {
			return Result{}, cerr
		}
		d.sleep(ctx, d.pollInterval())
	}
	return Result{}, errors.New("the service " + WindowsServiceName + " is not running after the start")
}

// windowsController returns the controller of the service to install.
func (d Deps) windowsController(req Request) (Controller, error) {
	if d.NewController != nil {
		return d.NewController(req.Binary)
	}
	userSID := d.UserSID
	if userSID == nil {
		userSID = CurrentUserSID
	}
	sid, err := userSID()
	if err != nil {
		return nil, fmt.Errorf("cannot read your user SID: %w", err)
	}
	cfg := WindowsConfig(req.Binary, req.StateDir, sid)
	if d.NewControllerFromConfig != nil {
		return d.NewControllerFromConfig(cfg)
	}
	return NewConfigController(cfg)
}

// validateBinaryAny accepts any non-empty absolute-looking path: the Windows
// service manager takes the executable as a separate value and quotes it.
func validateBinaryAny(binary string) error {
	if strings.TrimSpace(binary) == "" {
		return errors.New("the binary path is empty")
	}
	return nil
}
