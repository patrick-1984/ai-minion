// Package service installs and runs the Worker as a systemd, launchd or Windows service and detects foreground mode. It is the only package that imports kardianos/service (AD-1).
//
// Owner: Story 4.11 (`minion worker up`). The package is split by module and
// every effect on the machine goes through a small port, so that the logic is
// unit-testable without root, systemd or launchd:
//
//   - detect.go: [Detect] decides from facts about the machine ([Env]) which
//     service manager is available and whether the install is a system or a
//     user install. No service manager (a container) means foreground mode.
//   - target.go: [ResolveTarget] picks the user the Worker runs as (the sudo
//     user, not root), and the ownership helpers that keep the files `worker
//     up` creates as root readable by that user.
//   - trusted.go: [OpenTrustedDir] opens a directory of that user as root
//     without ever following a link the user could have planted (the whole
//     path is walked through directory handles), and every later change goes
//     through the returned [os.Root]: [ChownTree], [WriteFileIn].
//   - systemd.go: the system unit (identical to deploy/systemd/minion-worker@.service,
//     Story 9.1), the user unit, the linger check and the systemctl steps.
//   - launchd.go: the launchd agent plist and the launchctl steps.
//   - service.go: [Install], the entry point that dispatches on the detected
//     manager, the [Runner] and [Controller] ports, and the kardianos/service
//     wrapper used for the Windows service control manager.
//   - scm.go: [SCM.Run], the service handshake of the Windows service: the
//     process that the service control manager starts runs `minion worker
//     service-run`, which calls it, so that the manager sees the service start
//     and stop.
//   - windows.go: [WindowsConfig], the Windows service as installed by Story
//     8.2 (preview): the state directory and the SID of the installing user in
//     the service environment, automatic start, restart on failure and the
//     preview note; usersid_windows.go reads the SID of the user.
//
// Nothing here resolves settings or runs pre-start hooks: that is the job of
// internal/cli/worker_up.go.
package service
