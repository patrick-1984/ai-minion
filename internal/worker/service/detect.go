package service

import (
	"os"
	"runtime"
)

// Manager is a service manager `minion worker up` can install into.
type Manager string

const (
	// ManagerNone means no service manager: the Worker runs in the foreground
	// (a container, or a host that does not run systemd).
	ManagerNone Manager = "none"
	// ManagerSystemd is systemd on Linux.
	ManagerSystemd Manager = "systemd"
	// ManagerLaunchd is launchd on macOS.
	ManagerLaunchd Manager = "launchd"
	// ManagerWindows is the Windows service control manager (preview).
	ManagerWindows Manager = "windows"
)

// Scope says whether a service belongs to the whole machine or to one user.
type Scope string

const (
	// ScopeSystem is a system service (a system unit, started at boot).
	ScopeSystem Scope = "system"
	// ScopeUser is a per-user service (a systemd user unit or a launchd agent).
	ScopeUser Scope = "user"
)

// Env carries the facts about the machine that [Detect] reads, so that the
// detection matrix can be tested for every OS on any host.
type Env struct {
	// GOOS is the operating system, "linux", "darwin" or "windows".
	GOOS string
	// Getenv reads an environment variable.
	Getenv func(string) string
	// Exists reports whether a file or directory exists.
	Exists func(path string) bool
	// EUID is the effective user ID; 0 means root (sudo). It is 0 on Windows,
	// where the privilege model does not apply.
	EUID int
}

// SystemEnv returns the Env of the running process.
func SystemEnv() Env {
	return Env{
		GOOS:   runtime.GOOS,
		Getenv: os.Getenv,
		Exists: func(path string) bool { _, err := os.Stat(path); return err == nil },
		EUID:   os.Geteuid(),
	}
}

// systemdRunDir exists exactly when the machine was booted with systemd
// (the check of sd_booted(3)).
const systemdRunDir = "/run/systemd/system"

// Detection is the result of [Detect].
type Detection struct {
	Manager Manager
	// Scope is meaningful when Manager is not [ManagerNone].
	Scope Scope
	// Reason says in one line why Manager was chosen; for ManagerNone it
	// explains why the Worker will run in the foreground.
	Reason string
}

// Detect decides which service manager is available. On Linux that is systemd
// when it is running (a system unit when the caller is root, a user unit
// otherwise); a container without systemd, or a host without it, has none. On
// macOS it is launchd (a user agent), on Windows the service control manager.
func Detect(env Env) Detection {
	switch env.GOOS {
	case "darwin":
		return Detection{Manager: ManagerLaunchd, Scope: ScopeUser, Reason: "macOS: launchd agent for the current user"}
	case "windows":
		return Detection{Manager: ManagerWindows, Scope: ScopeSystem, Reason: "Windows: service control manager"}
	case "linux":
		return detectLinux(env)
	default:
		return Detection{Manager: ManagerNone, Reason: "no supported service manager on " + env.GOOS}
	}
}

func detectLinux(env Env) Detection {
	if env.Exists != nil && env.Exists(systemdRunDir) {
		if env.EUID == 0 {
			return Detection{Manager: ManagerSystemd, Scope: ScopeSystem, Reason: "systemd is running and the command runs as root: system unit"}
		}
		return Detection{Manager: ManagerSystemd, Scope: ScopeUser, Reason: "systemd is running and the command runs without root: user unit"}
	}
	if marker := containerMarker(env); marker != "" {
		return Detection{Manager: ManagerNone, Reason: "running in a container (" + marker + ") without a service manager"}
	}
	return Detection{Manager: ManagerNone, Reason: "systemd is not running"}
}

// containerMarker returns what tells that the process runs in a container, or
// "" when nothing does. It is only used to explain the result.
func containerMarker(env Env) string {
	for _, p := range []string{"/.dockerenv", "/run/.containerenv"} {
		if env.Exists != nil && env.Exists(p) {
			return p
		}
	}
	if env.Getenv != nil {
		if v := env.Getenv("container"); v != "" {
			return "container=" + v
		}
		if env.Getenv("KUBERNETES_SERVICE_HOST") != "" {
			return "Kubernetes"
		}
	}
	return ""
}
