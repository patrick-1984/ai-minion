//go:build !unix

package agent

import "os"

// There is no /proc and no UID check here: the process environment is not
// read, so session access stays unknown.
func sameUserProcess(int) bool { return false }

func openNonBlocking(path string) (*os.File, error) { return os.Open(path) }
