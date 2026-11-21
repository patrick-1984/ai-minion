//go:build !unix

package cli

// ownedByCurrentUser: replacing is refused on these systems before this is asked.
func ownedByCurrentUser(string) bool { return false }
