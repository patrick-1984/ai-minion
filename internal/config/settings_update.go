package config

import (
	"os"
	"path/filepath"
)

// Names of the settings of "minion update" (AD-24, R66), for callers that
// look them up by key.
const (
	KeyChannel        = "channel"
	KeyUpdateAPIURL   = "update_api_url"
	KeyUpdateCacheDir = "update_cache_dir"
)

func init() { declareUpdate(Default) }

// declareUpdate declares the settings of the updater. They are Worker-scope
// settings: "minion update" runs on the machine whose binary is replaced, and
// the Hub never reads them.
func declareUpdate(r *Registry) {
	r.Declare(Setting{
		Key:     KeyChannel,
		Scope:   ScopeWorker,
		Help:    "Version selector for minion update: vX.Y.Z (exact), X.Y or X (newest matching release) or latest",
		Default: "latest",
	})
	r.Declare(Setting{
		Key:     KeyUpdateAPIURL,
		Scope:   ScopeWorker,
		Help:    "GitHub API root that minion update lists releases from (https; http only on loopback)",
		Default: "https://api.github.com",
	})
	r.Declare(Setting{
		Key:         KeyUpdateCacheDir,
		Scope:       ScopeWorker,
		Help:        "Directory where minion update keeps verified releases for offline use",
		DefaultFunc: defaultUpdateCacheDir,
	})
}

// defaultUpdateCacheDir is the per-user cache directory plus "minion/update":
// XDG_CACHE_HOME or ~/.cache on Linux, ~/Library/Caches on macOS and
// %LocalAppData% on Windows (what [os.UserCacheDir] returns).
func defaultUpdateCacheDir() (string, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, appDir, "update"), nil
}
