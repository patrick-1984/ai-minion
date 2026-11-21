//go:build !windows

package config

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// managedExecutable resolves an explicit tmux path before root can execute it.
// The resolved file and every ancestor must be writable only by root.
func managedExecutable(path string) (string, error) {
	if os.Geteuid() != 0 {
		return path, nil
	}
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("resolve the privileged tmux binary: %w", err)
	}
	for p := real; ; p = filepath.Dir(p) {
		info, err := os.Stat(p)
		if err != nil {
			return "", err
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || stat.Uid != 0 || info.Mode().Perm()&0o022 != 0 ||
			(p == real && !info.Mode().IsRegular()) || (p != real && !info.IsDir()) {
			return "", fmt.Errorf("root requires a tmux binary and directory chain writable only by root: %s", p)
		}
		if p == "/" {
			return real, nil
		}
	}
}
