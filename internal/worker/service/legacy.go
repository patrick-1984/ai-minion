package service

import (
	"errors"
	"strings"
)

// LegacyRemovalCommand removes the LocalSystem service of releases up to
// 0.13.1, in an elevated PowerShell: disable it first (so a recovery action
// cannot start it again), stop it, check that it stopped, delete it, check
// that the service manager no longer lists it. It names only Windows tools, never a
// minion.exe: an elevated prompt must not run a file the user's programs can
// change.
const LegacyRemovalCommand = "in a PowerShell started as administrator run: " +
	"$ErrorActionPreference='Stop'; Set-Service " + WindowsServiceName + " -StartupType Disabled; " +
	"Stop-Service " + WindowsServiceName + " -Force; " +
	"if ((Get-Service " + WindowsServiceName + ").Status -ne 'Stopped') { throw 'the service still runs' }; " +
	"sc.exe delete " + WindowsServiceName + "; " +
	"if (Get-Service " + WindowsServiceName + " -ErrorAction SilentlyContinue) { 'still listed: close the Services console' } else { 'removed' }"

// IsLegacyService reports whether st is the Worker service of releases up to
// 0.13.1: installed, and running as LocalSystem (or as an account that the
// query could not read, which only an administrator's service hides).
func IsLegacyService(st WindowsServiceState) bool {
	if !st.Installed {
		return false
	}
	a := strings.TrimPrefix(strings.ToLower(st.Account), `.\`)
	return a == "" || a == "localsystem" || a == `nt authority\system`
}

// LegacyServiceWarning is the problem line of "minion worker status" (and the
// note of "worker up") while the LocalSystem service exists; empty otherwise.
func LegacyServiceWarning(st WindowsServiceState) string {
	if !IsLegacyService(st) {
		return ""
	}
	state := "installed"
	if st.Running {
		state = "installed and running"
	}
	return "the Windows service " + WindowsServiceName + " of an older release is " + state +
		": it runs minion as LocalSystem, from a file your programs can change, so it is a way to SYSTEM; " +
		"the Worker now runs as you. Remove the service: " + LegacyRemovalCommand
}

// LegacyQueryFailure is the message when the service control manager cannot
// be asked whether the LocalSystem service exists (access denied on both
// query rights, a failed connection): the state is unknown, so nothing may
// proceed as if the service were absent.
func LegacyQueryFailure(err error) string {
	return "cannot tell whether the Windows service " + WindowsServiceName + " of an older release (LocalSystem) exists: " + err.Error() +
		"; it must be removed before the Worker runs as you. Check and remove it " + LegacyRemovalCommand
}

// ErrLegacyRemovalPending says the service was stopped and marked for
// deletion, but the service control manager still lists it because a program
// (the Services console, for one) has it open; Windows removes it when that
// program closes it. It is not a confirmed removal.
var ErrLegacyRemovalPending = errors.New("the service " + WindowsServiceName + " is stopped and marked for deletion; Windows removes it when the programs that have it open (the Services console) are closed")
