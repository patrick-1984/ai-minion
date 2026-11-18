package service

import "strings"

// WindowsServiceState is what a query-only look at the service control
// manager tells about the Worker's Windows service (v0.10.0). It changes
// nothing in the SCM.
type WindowsServiceState struct {
	// Installed is true when the service exists.
	Installed bool
	// Running is true when it runs (or is starting).
	Running bool
	// AutoStart is true when it starts by itself at boot.
	AutoStart bool
	// Account is the account it runs as ("LocalSystem"); empty when unknown.
	Account string
	// BinaryPath is the executable the service starts (from its ImagePath);
	// empty when unknown.
	BinaryPath string
}

// ForegroundServiceWarning is the warning a foreground Worker prints when the
// Worker's Windows service is also installed and running or set to start at
// boot: that service serves only hidden ConPTY sessions and keeps a Worker
// connected after the console closes. It is empty when there is nothing to
// say. The commands name the service in the SCM, so they cannot reach the
// foreground Worker (`minion worker down` could: with the service stopped it
// stops the Worker of this console): a running service is stopped and then
// deleted, a stopped one that starts at boot is deleted, or set to start only
// on demand.
func ForegroundServiceWarning(st WindowsServiceState) string {
	if !st.Installed || (!st.Running && !st.AutoStart) {
		return ""
	}
	account := st.Account
	if account == "" || strings.EqualFold(account, "LocalSystem") || strings.EqualFold(account, `.\LocalSystem`) {
		account = "LocalSystem"
	}
	var state, fix string
	switch {
	case st.Running && st.AutoStart:
		state = "installed, running and set to start at boot"
	case st.Running:
		state = "installed and running"
	default:
		state = "installed and set to start at boot"
	}
	if st.Running {
		fix = "Stop and remove it, in an elevated PowerShell: sc.exe stop " + WindowsServiceName + ", then sc.exe delete " + WindowsServiceName
	} else {
		fix = "Remove it, in an elevated PowerShell: sc.exe delete " + WindowsServiceName +
			" (or keep it from starting at boot: sc.exe config " + WindowsServiceName + " start= demand)"
	}
	return "Warning: a Minion Worker Windows service (" + WindowsServiceName + ", runs as " + account + ") is also " + state +
		": it serves only hidden ConPTY sessions and keeps a Worker connected after you close this console. " + fix
}
