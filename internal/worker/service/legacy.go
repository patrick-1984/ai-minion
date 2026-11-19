package service

import "strings"

// LegacyRemovalCommand removes the LocalSystem service of releases up to
// 0.13.1, in an elevated PowerShell. It names only Windows tools, never a
// minion.exe: an elevated prompt must not run a file the user's programs can
// change.
const LegacyRemovalCommand = "in a PowerShell started as administrator run: Stop-Service " + WindowsServiceName + "; sc.exe delete " + WindowsServiceName

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
