package agent

// Run modes of a running Worker, as `minion worker status` shows them.
const (
	RunModeForeground     = "foreground console"
	RunModeWindowsService = "Windows service"
	RunModeSystemd        = "systemd service"
	RunModeLaunchd        = "launchd service"
)

// unixRunMode tells how a Worker on Linux or macOS runs from its environment:
// systemd sets INVOCATION_ID for every unit it starts, launchd sets
// XPC_SERVICE_NAME (to "0" in a plain terminal).
func unixRunMode(getenv func(string) string) string {
	if getenv("INVOCATION_ID") != "" {
		return RunModeSystemd
	}
	if x := getenv("XPC_SERVICE_NAME"); x != "" && x != "0" {
		return RunModeLaunchd
	}
	return RunModeForeground
}
