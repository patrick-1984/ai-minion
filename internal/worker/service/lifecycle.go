package service

import (
	"context"
	"strconv"
	"strings"
)

// LifecycleRequest names the Worker service [Deps.ProbeLifecycle] looks at.
type LifecycleRequest struct {
	// GOOS is the operating system.
	GOOS string
	// User, Home and UID are the account the Worker belongs to; EUID is the
	// effective user of this process (0: root).
	User string
	Home string
	UID  int
	EUID int
	// QueryWindows looks at the Windows service with query rights only; nil
	// means [QueryWindowsService].
	QueryWindows func() (WindowsServiceState, error)
}

// Lifecycle is what [Deps.ProbeLifecycle] found: the service, unit or agent
// that "minion worker up" installed for the user, and whether it runs. It
// never names the binary the service runs (tray C2: the elevation target
// comes from the service registration alone, never from status).
type Lifecycle struct {
	Manager Manager
	Scope   Scope
	Unit    string
	// Installed is true when the service, unit or agent is installed.
	Installed bool
	// Running is true when it runs (or is starting).
	Running bool
	// StartHint is the command that starts it when this process lacks the
	// rights ("sudo systemctl start minion-worker@ann.service"); empty when
	// "minion worker start" can do it.
	StartHint string
}

// ProbeLifecycle looks at the Worker service of req.User without changing
// anything (tray W10b): on Linux the evidence of [Deps.ProbeSystemInstance]
// (a packaged template alone is no installed Worker), on macOS the launchd
// agent's plist, on Windows a query-only look at the service control manager.
// The error is set when the service manager could not be asked; the
// Lifecycle is then the none one.
func (d Deps) ProbeLifecycle(ctx context.Context, req LifecycleRequest) (Lifecycle, error) {
	none := Lifecycle{Manager: ManagerNone}
	switch req.GOOS {
	case "linux":
		if d.Runner == nil {
			return none, nil
		}
		p := d.ProbeSystemInstance(ctx, req.User, req.Home)
		if p.Manager == ManagerNone {
			return none, nil
		}
		l := Lifecycle{Manager: p.Manager, Scope: p.Scope, Unit: p.Unit, Installed: true}
		args := []string{"is-active", p.Unit}
		if p.Scope == ScopeUser {
			args = append([]string{"--user"}, args...)
		}
		out, _ := d.Runner.Run(ctx, "systemctl", args...)
		l.Running = systemdStateRunning(out)
		if p.Scope == ScopeSystem && req.EUID != 0 {
			l.StartHint = "sudo systemctl start " + p.Unit
		}
		return l, nil
	case "darwin":
		if !isFile(PlistPath(req.Home)) {
			return none, nil
		}
		l := Lifecycle{Manager: ManagerLaunchd, Scope: ScopeUser, Unit: LaunchdLabel, Installed: true}
		if d.Runner != nil {
			target := "gui/" + strconv.Itoa(req.UID) + "/" + LaunchdLabel
			if out, err := d.Runner.Run(ctx, "launchctl", "print", target); err == nil {
				l.Running = strings.Contains(out, "state = running")
			}
		}
		return l, nil
	case "windows":
		query := req.QueryWindows
		if query == nil {
			query = QueryWindowsService
		}
		st, err := query()
		if err != nil {
			return none, err
		}
		if !st.Installed {
			return none, nil
		}
		return Lifecycle{Manager: ManagerWindows, Scope: ScopeSystem, Unit: WindowsServiceName, Installed: true, Running: st.Running}, nil
	}
	return none, nil
}
