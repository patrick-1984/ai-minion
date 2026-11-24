//go:build !unix

package agent

import "os"

// There is no /proc and no UID check here: the process environment is not
// read, so session access stays unknown.
func readProcAccess(int, bool) ([]byte, claudeSrc, bool) { return nil, claudeSrc{}, false }

func parentClaudeSrc() claudeSrc {
	cwd, _ := os.Getwd()
	return claudeSrc{Cwd: cwd}
}

func openNonBlocking(path string) (*os.File, error) { return os.Open(path) }
