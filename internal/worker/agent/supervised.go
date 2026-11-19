package agent

import "os"

// SupervisedEnv is set by "minion worker launch" (the supervisor of the
// Windows Worker, started at logon from the user's Run key) for the Worker
// it starts. A supervised Worker hands a restart (self-update) to the
// supervisor by exiting with the restart code, as under a service manager.
const SupervisedEnv = "MINION_SUPERVISED"

// supervised is read once and the variable removed, so that the sessions and
// programs the Worker starts do not inherit it.
var supervised = func() bool {
	v := os.Getenv(SupervisedEnv) == "1"
	_ = os.Unsetenv(SupervisedEnv)
	return v
}()

// Supervised reports whether a supervisor started this Worker.
func Supervised() bool { return supervised }

// ExitRestart is the exit code with which a Worker asks its service manager
// or supervisor to start it again (with a new binary after a self-update).
const ExitRestart = exitRestart
