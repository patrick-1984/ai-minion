package service

import "errors"

// ErrRunsAsSystem refuses a Worker that would run as the LocalSystem account
// (security fix S1): the Worker runs a binary from a user's profile and must
// never have the rights of SYSTEM. Every entry point that starts a Worker
// (run, launch, up) checks the process token for it.
var ErrRunsAsSystem = errors.New("refusing to run the Worker as LocalSystem (SYSTEM): the Worker runs as a normal user; run it from your own account")

// RefuseSystem returns [ErrRunsAsSystem] when isSystem is true.
func RefuseSystem(isSystem bool) error {
	if isSystem {
		return ErrRunsAsSystem
	}
	return nil
}
