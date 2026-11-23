//go:build !unix

package agent

import "os"

// There is no /proc and no UID check here: the process environment is not
// read, so session access stays unknown.
func readProcAccess(int, bool) ([]byte, string, bool) { return nil, "", false }

func parentClaudeSettings() string { return "" }

func openNonBlocking(path string) (*os.File, error) { return os.Open(path) }
