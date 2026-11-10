package identity

import "regexp"

// PipeUserSIDEnv names the environment variable that pins the user the Worker
// state belongs to when the Worker runs as a Windows service. A service runs as
// LocalSystem (SYSTEM), not as the user who ran `minion worker up`, so the
// install writes the SID of that user into the service environment. The value
// is defined once, here, because the kernel package identity cannot import the
// packages that read it: internal/worker/localapi (the control pipe of that
// user), internal/worker/service (writes it into the service) and
// internal/worker/store (the state directory). Tests in those packages keep
// their copies equal.
const PipeUserSIDEnv = "MINION_PIPE_USER_SID"

// SIDSystem is the SID of the LocalSystem account.
const SIDSystem = "S-1-5-18"

// sidRE matches the string form of a SID: S-1-<authority>-<sub-authorities>.
// A SID has at most 15 sub-authorities; the check also keeps every character
// that could change the meaning of a pipe name or an SDDL string out.
var sidRE = regexp.MustCompile(`^S-1-[0-9]{1,15}(-[0-9]{1,10}){1,15}$`)

// ValidSID reports whether s is a well-formed SID string.
func ValidSID(s string) bool { return sidRE.MatchString(s) }

// PrivateUserSID returns the SID of the user that the private files and
// directories of the Worker belong to (the Windows DACLs of the state directory
// and of every key and pin file). It is the SID of the process user, except
// that a process that runs as SYSTEM (the Windows service) with a valid pinned
// user SID (the value of [PipeUserSIDEnv]) serves that user: without this the
// service would protect the state of the installing user against the installing
// user, who could then no longer read, reset or run the Worker in the
// foreground. The pin is ignored for every other process user, so a user cannot
// give a file to another account by setting an environment variable.
//
// It has no Windows API in it, so the choice is tested on every platform.
func PrivateUserSID(processSID, pinned string) string {
	if processSID == SIDSystem && pinned != "" && ValidSID(pinned) {
		return pinned
	}
	return processSID
}
