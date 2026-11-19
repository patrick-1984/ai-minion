package service

import "fmt"

// The Windows Worker runs as the user who installs it (security fix S1): it
// is started at logon from the user's Run key (see autostart.go), never as a
// Windows service. Releases up to 0.13.1 installed it as the LocalSystem
// service MinionWorker, which ran a binary from the user's profile as
// SYSTEM; this package only finds and removes such a service now (see
// legacy.go), and the service entry point "worker service-run" only tells
// one to end.

// pipeUserSIDEnv is identity.PipeUserSIDEnv and localapi.PipeUserSIDEnv; this
// package imports neither for a string, and a test keeps the three equal.
const pipeUserSIDEnv = "MINION_PIPE_USER_SID"

// accessDenied is [isAccessDenied]; tests replace it to exercise the hint on
// every platform.
var accessDenied = isAccessDenied

// windowsInstallError adds the usual cause to an operation on the legacy
// service that Windows refused: it needs an administrator. The cause is
// found by error number, not by text, because the message is localized.
func windowsInstallError(op string, err error) error {
	err = fmt.Errorf("%s the service: %w", op, err)
	if accessDenied(err) {
		err = fmt.Errorf("%w; this needs an administrator: %s", err, LegacyRemovalCommand)
	}
	return err
}
