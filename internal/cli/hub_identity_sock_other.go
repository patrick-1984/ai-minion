//go:build !unix

package cli

import "errors"

// checkSocketFile: Unix sockets are not available on this system.
func checkSocketFile(string) error {
	return errors.New("the local admin socket is not available on this system")
}
