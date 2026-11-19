//go:build windows

package agent

// workerRunMode is how this Worker runs: as the Windows service (the service
// control manager started it) or in a console.
func workerRunMode() string {
	if underServiceManager() {
		return RunModeWindowsService
	}
	if supervised {
		return RunModeWindowsLogon
	}
	return RunModeForeground
}
