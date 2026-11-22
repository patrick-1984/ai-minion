//go:build !windows

package presence

import (
	"path/filepath"

	"github.com/patrick-1984/ai-minion/internal/config"
)

type pathEnvT = pe

type pe struct{ state string }

func pathEnv(state string) pe { return pe{state} }

func (p pe) e() config.PathEnv {
	return config.PathEnv{GOOS: "linux", Getenv: func(k string) string {
		if k == "XDG_STATE_HOME" {
			return p.state
		}
		return ""
	}, HomeDir: func() (string, error) { return "/home/x", nil }}
}

// stageOrDir is the directory where files are staged (tests).
func (l Layout) stageOrDir() string {
	if l.Scope == ScopeRoot {
		return filepath.Join(l.PublicRoot, stageDirName)
	}
	return l.PrivateDir
}
