package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// ErrNotInstalled means no Worker service, unit or agent is installed for the
// user: "minion worker up" installs one. "minion worker start" never does.
var ErrNotInstalled = errors.New("no Worker service is installed for this user; set it up with: minion worker up")

// NeedsAdminError is a start the service manager refused for lack of rights;
// Hint is the command that has them.
type NeedsAdminError struct {
	Hint string
	Err  error
}

func (e *NeedsAdminError) Error() string {
	msg := "starting the Worker needs administrator rights"
	if e.Err != nil {
		msg += " (" + e.Err.Error() + ")"
	}
	return msg + "; run: " + e.Hint
}

func (e *NeedsAdminError) Unwrap() error { return e.Err }

// StartRequest names the Worker service to start.
type StartRequest struct {
	// GOOS is the operating system.
	GOOS string
	// User, Home and UID are the account the Worker belongs to; EUID is the
	// effective user of this process (0: root).
	User string
	Home string
	UID  int
	EUID int
	// Windows returns the controller of the Windows service.
	Windows func() (Controller, error)
}

// StartResult is what [Deps.StartInstalled] started.
type StartResult struct {
	Manager Manager `json:"manager"`
	Scope   Scope   `json:"scope,omitempty"`
	Unit    string  `json:"unit,omitempty"`
	// AlreadyRunning is true when it ran before (nothing was started).
	AlreadyRunning bool `json:"already_running"`
}

// StartInstalled starts the Worker service, unit or agent that "minion worker
// up" installed (tray W1b). It never installs, writes or reconfigures
// anything: an absent service is [ErrNotInstalled], a refusal for lack of
// rights a [*NeedsAdminError] with the command that has them.
func (d Deps) StartInstalled(ctx context.Context, req StartRequest) (StartResult, error) {
	switch req.GOOS {
	case "linux":
		return d.startSystemd(ctx, req)
	case "darwin":
		return d.startLaunchd(ctx, req)
	case "windows":
		return startWindows(req)
	}
	return StartResult{Manager: ManagerNone}, ErrNotInstalled
}

func (d Deps) startSystemd(ctx context.Context, req StartRequest) (StartResult, error) {
	p := d.ProbeSystemInstance(ctx, req.User, req.Home)
	res := StartResult{Manager: p.Manager, Scope: p.Scope, Unit: p.Unit}
	switch {
	case p.Manager == ManagerNone:
		return res, ErrNotInstalled
	case p.Scope == ScopeSystem && req.EUID != 0:
		return res, &NeedsAdminError{Hint: "sudo systemctl start " + p.Unit}
	}
	user := p.Scope == ScopeUser
	args := []string{"is-active", p.Unit}
	if user {
		args = append([]string{"--user"}, args...)
	}
	if out, _ := d.Runner.Run(ctx, "systemctl", args...); systemdStateRunning(out) {
		res.AlreadyRunning = true
		return res, nil
	}
	if err := d.systemctl(ctx, user, "start", p.Unit); err != nil {
		if p.Scope == ScopeSystem && strings.Contains(err.Error(), "Access denied") {
			return res, &NeedsAdminError{Hint: "sudo systemctl start " + p.Unit, Err: err}
		}
		return res, err
	}
	return res, nil
}

func (d Deps) startLaunchd(ctx context.Context, req StartRequest) (StartResult, error) {
	res := StartResult{Manager: ManagerLaunchd, Scope: ScopeUser, Unit: LaunchdLabel}
	path := PlistPath(req.Home)
	if !isFile(path) {
		return StartResult{Manager: ManagerNone}, ErrNotInstalled
	}
	domain := "gui/" + strconv.Itoa(req.UID)
	target := domain + "/" + LaunchdLabel
	if out, err := d.Runner.Run(ctx, "launchctl", "print", target); err == nil {
		if strings.Contains(out, "state = running") {
			res.AlreadyRunning = true
			return res, nil
		}
	} else if out, err := d.Runner.Run(ctx, "launchctl", "bootstrap", domain, path); err != nil {
		return res, fmt.Errorf("launchctl bootstrap failed: %w%s", err, tail(out))
	}
	if out, err := d.Runner.Run(ctx, "launchctl", "kickstart", target); err != nil {
		return res, fmt.Errorf("launchctl kickstart failed: %w%s", err, tail(out))
	}
	return res, nil
}

func startWindows(req StartRequest) (StartResult, error) {
	res := StartResult{Manager: ManagerWindows, Scope: ScopeSystem, Unit: WindowsServiceName}
	if req.Windows == nil {
		return res, ErrNotInstalled
	}
	c, err := req.Windows()
	if err != nil {
		return res, err
	}
	st, err := c.Status()
	switch {
	case errors.Is(err, ErrNotInstalled):
		return StartResult{Manager: ManagerNone}, ErrNotInstalled
	case errors.Is(err, os.ErrPermission):
		return res, &NeedsAdminError{Hint: "Start-Service " + WindowsServiceName + " (in PowerShell opened with Run as administrator)", Err: err}
	case err != nil:
		return res, err
	case st == StatusRunning:
		res.AlreadyRunning = true
		return res, nil
	}
	if err := c.Start(); err != nil {
		if errors.Is(err, os.ErrPermission) {
			return res, &NeedsAdminError{Hint: "Start-Service " + WindowsServiceName + " (in PowerShell opened with Run as administrator)", Err: err}
		}
		return res, err
	}
	return res, nil
}
