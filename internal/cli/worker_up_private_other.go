//go:build !unix

package cli

// serviceOwnedPrivate falls back to the daemon's own check where an install
// for another account does not happen.
func serviceOwnedPrivate(path string, _ int) ([]byte, error) { return readPrivateSinkFile(path) }
