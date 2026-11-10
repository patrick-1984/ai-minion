package service

import (
	"fmt"

	ksvc "github.com/kardianos/service"
)

// The Windows service (Story 8.2, R98, preview). The service control manager
// starts `minion worker service-run` (see [SCM.Run]) as LocalSystem, which is
// what an install without a password gives. That has two consequences that
// this file handles, and neither depends on the Windows API, so both are
// tested on every platform:
//
//   - LocalSystem has its own profile, so the state directory the service
//     resolves is not the one `minion worker up` saved the settings to. The
//     install therefore always writes the state directory into the service
//     environment (MINION_STATE_DIR); that is how the service has "the same
//     settings".
//   - The control pipe of the Worker belongs to a user (AD-22), not to
//     LocalSystem. The install writes the SID of the installing user into the
//     service environment (MINION_PIPE_USER_SID), so the Worker serves the
//     pipe of the user who runs the CLI. The pipe ACL admits that user and
//     SYSTEM only.
//   - The state of the Worker belongs to the installing user, not to SYSTEM:
//     the same environment variable makes the SYSTEM service grant that user
//     (and SYSTEM) its state directory and key files, so the user can still run
//     `minion worker status`, `worker reset` and `worker up --foreground`
//     (identity.PrivateUserSID).

// pipeUserSIDEnv is identity.PipeUserSIDEnv and localapi.PipeUserSIDEnv; this
// package imports neither for a string, and a test keeps the three equal.
const pipeUserSIDEnv = "MINION_PIPE_USER_SID"

// failureResetPeriodSeconds is how long the service must run without failing
// before the service control manager forgets earlier failures: one day.
// kardianos/service passes the value to SetRecoveryActions unchanged, and the
// Windows API takes seconds.
const failureResetPeriodSeconds = 24 * 60 * 60

// windowsServiceNote tells the operator what a Windows service of the preview
// is, in one paragraph that `minion worker up` prints after the install.
const windowsServiceNote = "Windows preview: the service runs as the LocalSystem account, so the terminal sessions it creates run as that account, not as you. " +
	"To run sessions as yourself, stop the service and run \"minion worker up --foreground\" in your own terminal. " +
	"The CLI reaches the service over the named pipe of the user that ran this command."

// WindowsConfig returns the kardianos/service configuration to install: the
// entry point of [KardianosConfig], the environment that makes the service
// read the saved settings and serve the user's control pipe, automatic start
// at boot, and a restart 5 seconds after a failure (the counterpart of
// Restart=always). An empty stateDir or userSID leaves that variable out.
func WindowsConfig(binary, stateDir, userSID string) *ksvc.Config {
	cfg := KardianosConfig(binary)
	env := map[string]string{}
	if stateDir != "" {
		env[stateEnvName] = stateDir
	}
	if userSID != "" {
		env[pipeUserSIDEnv] = userSID
	}
	if len(env) > 0 {
		cfg.EnvVars = env
	}
	cfg.Option = ksvc.KeyValue{
		"StartType":              "automatic",
		"OnFailure":              "restart",
		"OnFailureDelayDuration": "5s",
		"OnFailureResetPeriod":   failureResetPeriodSeconds, // seconds, not days
	}
	return cfg
}

// NewConfigController returns the [Controller] for the service described by
// cfg.
func NewConfigController(cfg *ksvc.Config) (Controller, error) {
	s, err := ksvc.New(noopProgram{}, cfg)
	if err != nil {
		return nil, err
	}
	return kardianosController{svc: s}, nil
}

// accessDenied is [isAccessDenied]; tests replace it to exercise the hint on
// every platform.
var accessDenied = isAccessDenied

// windowsInstallError adds the usual cause to an install that Windows refused:
// creating a service needs an administrator. The cause is found by error
// number, not by text, because the message is localized.
func windowsInstallError(op string, err error) error {
	err = fmt.Errorf("%s the service: %w", op, err)
	if accessDenied(err) {
		err = fmt.Errorf("%w; installing a Windows service needs an administrator: run \"minion worker up\" in a terminal started as administrator, or use --foreground", err)
	}
	return err
}
