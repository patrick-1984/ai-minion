package localapi

import (
	"errors"
	"fmt"
	"strings"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/identity"
)

// The Windows control pipe (AD-22). The Worker serves the same op frames on a
// named pipe instead of a unix socket. The pipe name carries the SID of the
// user it belongs to, and the pipe's DACL lets only that user (and SYSTEM, the
// account a Windows service runs as) in. This file holds the parts that need
// no Windows API, so that they are tested on every platform: names, the SDDL
// string and the decision about who may own a pipe a client connects to. The
// Windows API calls are in pipe_windows.go.

const (
	// PipePrefix is the pipe namespace on Windows; every pipe name starts with it.
	PipePrefix = `\\.\pipe\`
	// PipeNamePrefix starts the name of the Worker control pipe; the user's
	// SID follows.
	PipeNamePrefix = PipePrefix + "minion-worker-"

	// PipeUserSIDEnv names the environment variable that sets the SID the pipe
	// belongs to. A Worker that runs as a Windows service runs as SYSTEM, not
	// as the user who installed it, so `minion worker up` writes the installing
	// user's SID into the service environment; everywhere else the SID of the
	// process user is used. The name is defined once, in the kernel package
	// identity, which uses the same pin for the private DACLs of the state.
	PipeUserSIDEnv = identity.PipeUserSIDEnv

	// SIDSystem and SIDAdministrators are the well-known SIDs that may own the
	// pipe of a Worker that runs as a service (see [TrustedPipeOwner]).
	SIDSystem         = identity.SIDSystem
	SIDAdministrators = "S-1-5-32-544"
)

// ValidSID reports whether s is a well-formed SID string.
func ValidSID(s string) bool { return identity.ValidSID(s) }

// PipeName returns the control pipe name for the user with the given SID:
// \\.\pipe\minion-worker-<SID>.
func PipeName(sid string) (string, error) {
	if !ValidSID(sid) {
		return "", fmt.Errorf("localapi: %q is not a valid user SID", sid)
	}
	return PipeNamePrefix + sid, nil
}

// ParsePipeName returns the SID of a control pipe name, or an error when name
// is not exactly \\.\pipe\minion-worker-<SID>.
func ParsePipeName(name string) (string, error) {
	sid, ok := strings.CutPrefix(name, PipeNamePrefix)
	if !ok || !ValidSID(sid) {
		return "", fmt.Errorf("localapi: %q is not a Minion control pipe name (want %s<user SID>)", name, PipeNamePrefix)
	}
	return sid, nil
}

// PipeSDDL returns the security descriptor of the control pipe of the user
// with the given SID, in SDDL form: a protected DACL (it inherits nothing)
// with exactly two entries, full access for that user and for SYSTEM. SYSTEM
// is the service account that runs the Worker as a Windows service; it is
// not another user. Everybody else, including other users, network logons
// and anonymous callers, has no access.
func PipeSDDL(sid string) (string, error) {
	if !ValidSID(sid) {
		return "", fmt.Errorf("localapi: %q is not a valid user SID", sid)
	}
	sddl := "D:P(A;;GA;;;" + sid + ")"
	if sid != SIDSystem {
		sddl += "(A;;GA;;;SY)"
	}
	return sddl, nil
}

// TrustedPipeOwner reports whether a pipe that is owned by owner may be
// talked to by the user with SID self. A client never sends a request to a
// pipe that another user created: pipe names are machine-wide, so another
// user could create the pipe of this user first and read the requests. The
// owner of a Worker's pipe is the user itself (foreground Worker), or SYSTEM
// or the Administrators group (Windows service, whose default owner depends on
// how the service was started).
func TrustedPipeOwner(owner, self string) bool {
	return owner != "" && (owner == self || owner == SIDSystem || owner == SIDAdministrators)
}

// ErrNoUserSID is returned when the SID the control pipe belongs to cannot be
// found.
var ErrNoUserSID = errors.New("localapi: cannot determine the user SID of the control pipe")

// PipePath returns the control pipe name for env: the SID in the environment
// variable [PipeUserSIDEnv] when it is set, else the one that self returns (the
// SID of the current user).
func PipePath(getenv func(string) string, self func() (string, error)) (string, error) {
	if getenv != nil {
		if v := getenv(PipeUserSIDEnv); v != "" {
			if !ValidSID(v) {
				return "", fmt.Errorf("localapi: %s=%q is not a valid SID", PipeUserSIDEnv, v)
			}
			return PipeName(v)
		}
	}
	if self == nil {
		return "", ErrNoUserSID
	}
	sid, err := self()
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrNoUserSID, err)
	}
	return PipeName(sid)
}

// windowsPipePath is [SocketPath] on Windows.
func windowsPipePath(env config.PathEnv) (string, error) {
	return PipePath(env.Getenv, processUserSID)
}

// pipeServedBy reports whether a process that runs as the user with SID self
// can serve the control pipe of the user with SID owner. The pipe DACL lets
// only that user and SYSTEM in, and creating a further pipe instance needs
// that access, so a Worker can only serve its own pipe, or any pipe when it
// runs as SYSTEM (a Windows service pinned to the installing user).
func pipeServedBy(owner, self string) bool {
	return owner == self || self == SIDSystem
}
