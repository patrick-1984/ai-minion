package cli

import (
	"fmt"

	"github.com/patrick-1984/ai-minion/internal/worker/service"
)

// serviceUpdateGuard decides what "minion update" does on Windows (security
// fix S1). protected is %ProgramFiles%\Minion\minion.exe, the binary a
// Windows service installed by this version runs, which only administrators
// and SYSTEM can change; it is known without asking the service manager.
//
//   - Updating the protected binary from a prompt that is not elevated cannot
//     work, whatever the service manager says: it is refused (exit 21) with
//     the command to run in an elevated prompt, before anything is contacted.
//   - Updating it elevated works; the service must be restarted.
//   - A service that still runs another file (installed by release 0.13.1 or
//     older, typically the user-writable copy) is not one to elevate: the
//     note says how to move it instead.
//   - Updating another copy works but does not change what a protected
//     service runs, which the note says.
//
// refuse is the message of a refusal, note the line printed after a
// successful update; both are empty when there is nothing to say.
func serviceUpdateGuard(st service.WindowsServiceState, exe, protected string, elevated bool) (refuse, note string) {
	isProtected := protected != "" && service.SameWindowsPath(exe, protected)
	if isProtected && !elevated {
		return fmt.Sprintf("%s is the binary of the Windows service %s, which only administrators can change. "+
			"Update it from the Hub (the Worker's update button, as an administrator), or run in an elevated prompt: \"%s\" update",
			protected, service.WindowsServiceName, protected), ""
	}
	if !st.Installed || st.BinaryPath == "" {
		if isProtected {
			return "", restartNote()
		}
		return "", ""
	}
	svc := st.BinaryPath
	switch {
	case protected == "" || !service.SameWindowsPath(svc, protected):
		return "", legacyServiceNote(svc)
	case isProtected:
		return "", restartNote()
	}
	return "", fmt.Sprintf("The Windows service %s runs its own copy, %s, which this update did not change. "+
		"Update it from the Hub (the Worker's update button, as an administrator), or run in an elevated prompt: \"%s\" update, then Restart-Service %s",
		service.WindowsServiceName, svc, svc, service.WindowsServiceName)
}

func restartNote() string {
	return fmt.Sprintf("The Windows service %s runs this binary; restart it to use the new version: Restart-Service %s",
		service.WindowsServiceName, service.WindowsServiceName)
}

// legacyServiceNote is for a service that still runs a binary outside
// %ProgramFiles%\Minion. Elevating that binary is what must not happen; the
// way out is the install.ps1 bootstrap, which never runs a user-writable file.
func legacyServiceNote(svc string) string {
	return fmt.Sprintf("The Windows service %s still runs %s, a file outside %%ProgramFiles%%\\Minion that non-administrators may be able to change; do not run it elevated. "+
		"Move the service to the verified release: in an elevated PowerShell run %s",
		service.WindowsServiceName, svc, service.BootstrapCommand())
}
