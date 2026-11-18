package service

import (
	"context"
	"os"
	"path/filepath"
	"strings"
)

// InstanceProbe is what [Deps.ProbeSystemInstance] found: the systemd unit
// of one user's Worker, if any. Manager is [ManagerNone] when neither a system
// instance nor a user unit is installed; Scope then names the scope a
// "worker up" without root would install (user).
type InstanceProbe struct {
	Manager Manager
	Scope   Scope
	// Unit is the unit name ("minion-worker@<user>.service" or
	// "minion-worker.service"), empty for ManagerNone.
	Unit string
	// Warning is set when both a system instance and a user unit exist: the
	// system instance wins and the user unit should be removed.
	Warning string
	// Evidence says in one line what proved the installed unit.
	Evidence string
}

// systemdStateInstalled reports whether a "systemctl is-enabled" answer means
// that the unit is installed for start (enabled at boot or at runtime).
func systemdStateInstalled(out string) bool {
	switch strings.TrimSpace(out) {
	case "enabled", "enabled-runtime", "linked", "linked-runtime":
		return true
	}
	return false
}

// systemdStateRunning reports whether a "systemctl is-active" answer means
// that the unit runs or is about to.
func systemdStateRunning(out string) bool {
	switch strings.TrimSpace(out) {
	case "active", "activating", "reloading", "refreshing":
		return true
	}
	return false
}

// ProbeSystemInstance finds the systemd unit of user's Worker on Linux (tray
// ticket W1a). The evidence of a system instance is the instance
// minion-worker@<user>.service being enabled or active, or the instance
// drop-in directory that "worker up" writes under the system unit directory.
// The template unit minion-worker@.service alone, which the deb and rpm
// packages ship for every machine, is no evidence. The evidence of a user
// unit is minion-worker.service being enabled or active in the user manager,
// or the unit file in home's ~/.config/systemd/user. When both exist the
// system instance wins, with a warning. home may be empty (no user unit file
// check).
func (d Deps) ProbeSystemInstance(ctx context.Context, user, home string) InstanceProbe {
	none := InstanceProbe{Manager: ManagerNone, Scope: ScopeUser}
	if validateUser(user) != nil || d.Runner == nil {
		return none
	}
	sysUnit := SystemUnitFor(user)
	var sysEvidence, userEvidence string
	if out, _ := d.Runner.Run(ctx, "systemctl", "is-active", sysUnit); systemdStateRunning(out) {
		sysEvidence = sysUnit + " is " + strings.TrimSpace(out)
	} else if out, _ := d.Runner.Run(ctx, "systemctl", "is-enabled", sysUnit); systemdStateInstalled(out) {
		sysEvidence = sysUnit + " is " + strings.TrimSpace(out)
	} else if dir := filepath.Join(d.systemUnitDir(), sysUnit+".d"); isDir(dir) {
		sysEvidence = "the drop-in directory " + dir + " exists"
	}
	if out, _ := d.Runner.Run(ctx, "systemctl", "--user", "is-active", UserUnitName); systemdStateRunning(out) {
		userEvidence = "the user unit " + UserUnitName + " is " + strings.TrimSpace(out)
	} else if out, _ := d.Runner.Run(ctx, "systemctl", "--user", "is-enabled", UserUnitName); systemdStateInstalled(out) {
		userEvidence = "the user unit " + UserUnitName + " is " + strings.TrimSpace(out)
	} else if home != "" {
		if p := filepath.Join(home, ".config", "systemd", "user", UserUnitName); isFile(p) {
			userEvidence = "the user unit file " + p + " exists"
		}
	}
	switch {
	case sysEvidence != "" && userEvidence != "":
		return InstanceProbe{
			Manager: ManagerSystemd, Scope: ScopeSystem, Unit: sysUnit, Evidence: sysEvidence,
			Warning: "both the system service " + sysUnit + " and a user unit " + UserUnitName + " are installed (" + userEvidence +
				"); the system service is used. Remove the user unit: systemctl --user disable --now " + UserUnitName,
		}
	case sysEvidence != "":
		return InstanceProbe{Manager: ManagerSystemd, Scope: ScopeSystem, Unit: sysUnit, Evidence: sysEvidence}
	case userEvidence != "":
		return InstanceProbe{Manager: ManagerSystemd, Scope: ScopeUser, Unit: UserUnitName, Evidence: userEvidence}
	}
	return none
}

func isDir(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

func isFile(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.Mode().IsRegular()
}
