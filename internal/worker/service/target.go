package service

import (
	"fmt"
	"os"
	"os/user"
	"strconv"
)

// Target is the user the Worker runs as. It sees the tmux sessions of that
// user: by default the person who ran sudo, or root itself on a root login
// (a deliberate choice on hosts whose sessions run as root).
type Target struct {
	User string
	UID  int
	GID  int
	Home string
}

// TargetEnv carries what [ResolveTarget] reads from the machine.
type TargetEnv struct {
	// EUID is the effective user ID of the process.
	EUID int
	// Getenv reads an environment variable (SUDO_USER).
	Getenv func(string) string
	// Lookup returns the account with that name.
	Lookup func(name string) (*user.User, error)
	// Current returns the account of the process.
	Current func() (*user.User, error)
}

// SystemTargetEnv returns the TargetEnv of the running process.
func SystemTargetEnv() TargetEnv {
	return TargetEnv{EUID: os.Geteuid(), Getenv: os.Getenv, Lookup: user.Lookup, Current: user.Current}
}

// ResolveTarget picks the account the Worker runs as. asUser is the explicit
// choice (--as-user). Without it, root uses the account of SUDO_USER; a plain
// root login (no sudo) runs the Worker as root and serves root's tmux
// sessions; any other caller uses their own account.
func ResolveTarget(env TargetEnv, asUser string) (Target, error) {
	getenv := env.Getenv
	if getenv == nil {
		getenv = func(string) string { return "" }
	}
	cur, err := env.Current()
	if err != nil {
		return Target{}, fmt.Errorf("cannot determine the current user: %w", err)
	}
	name := asUser
	switch {
	case name != "":
	case env.EUID == 0 && getenv("SUDO_USER") != "":
		name = getenv("SUDO_USER")
	default:
		name = cur.Username
	}
	u := cur
	if name != cur.Username {
		if env.EUID != 0 {
			return Target{}, fmt.Errorf("--as-user %s needs root; run with sudo, or leave --as-user out to install for yourself", name)
		}
		if u, err = env.Lookup(name); err != nil {
			return Target{}, fmt.Errorf("user %q: %w", name, err)
		}
	}
	return targetFromUser(u)
}

func targetFromUser(u *user.User) (Target, error) {
	uid, err := strconv.Atoi(u.Uid)
	if err != nil {
		// Windows account IDs are SIDs; ownership changes do not apply there.
		uid = -1
	}
	gid, err := strconv.Atoi(u.Gid)
	if err != nil {
		gid = -1
	}
	if u.Username == "" || u.HomeDir == "" {
		return Target{}, fmt.Errorf("account %q has no name or home directory", u.Username)
	}
	return Target{User: u.Username, UID: uid, GID: gid, Home: u.HomeDir}, nil
}
