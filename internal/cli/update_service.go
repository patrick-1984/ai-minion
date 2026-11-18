package cli

import (
	"fmt"

	"github.com/patrick-1984/ai-minion/internal/worker/service"
)

// serviceUpdateGuard decides what "minion update" does on a machine where the
// Worker's Windows service is installed (security fix S1). The service runs
// its own copy of the binary in %ProgramFiles%\Minion, which only
// administrators and SYSTEM can change:
//
//   - updating that copy from a prompt that is not elevated cannot work, so
//     it is refused (exit 21) with the command to run in an elevated prompt;
//   - updating that copy elevated works; the service must be restarted;
//   - updating another copy (the one the installer put in %LOCALAPPDATA%)
//     works but does not change what the service runs, which the note says.
//
// refuse is the message of a refusal, note the line printed after a
// successful update; both are empty when no service is installed.
func serviceUpdateGuard(st service.WindowsServiceState, exe string, elevated bool) (refuse, note string) {
	if !st.Installed || st.BinaryPath == "" {
		return "", ""
	}
	svc := st.BinaryPath
	if service.SameWindowsPath(exe, svc) {
		if !elevated {
			return fmt.Sprintf("the Windows service %s runs %s, which only administrators can change. "+
				"Update it from the Hub (the Worker's update button, as an administrator), or run in an elevated prompt: \"%s\" update",
				service.WindowsServiceName, svc, svc), ""
		}
		return "", fmt.Sprintf("The Windows service %s runs this binary; restart it to use the new version: Restart-Service %s",
			service.WindowsServiceName, service.WindowsServiceName)
	}
	return "", fmt.Sprintf("The Windows service %s runs its own copy, %s, which this update did not change. "+
		"Update it from the Hub (the Worker's update button, as an administrator), or run in an elevated prompt: \"%s\" update, then Restart-Service %s",
		service.WindowsServiceName, svc, svc, service.WindowsServiceName)
}
