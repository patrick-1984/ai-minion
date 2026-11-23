package agent

import (
	"encoding/json"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// sessionAccess is how a harness session authenticates, read from the
// environment of its own process and from the harness settings files. Only
// the access kind is kept: no key, token, URL or host is ever read into a
// value that leaves this file.
type sessionAccess struct {
	Mode string // "subscription", "api" or "unknown"
	// Kind refines "api": accessAPIKey (the vendor's endpoint with a key or
	// token) or accessCustomEndpoint (any other base URL).
	Kind string
}

const (
	accessAPIKey         = "api_key"
	accessCustomEndpoint = "custom_endpoint"
)

// processEnv returns the access-relevant environment of a process, or false
// when it is not readable or not trustworthy (another user, another
// platform, vanished pid, a read that stalls). Tests replace it.
var processEnv = readProcessEnv

// Environment names the Worker looks at. Only the profile directory keeps
// its value; the endpoint and a secret keep nothing but the fact that they are
// set (the entry becomes NAME=1, or NAME=custom for an endpoint that is not
// the vendor's own).
var (
	envValueNames    = []string{"CLAUDE_CONFIG_DIR", "CODEX_HOME"}
	envPresenceNames = []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "OPENAI_API_KEY"}
	envEndpointNames = map[string]string{"ANTHROPIC_BASE_URL": "api.anthropic.com", "OPENAI_BASE_URL": "api.openai.com"}
)

const (
	processEnvLimit    = 1 << 20
	processEnvDeadline = 500 * time.Millisecond
)

// readProcessEnv reads the environment of one process of the same user,
// within a deadline, and reduces it to the allowlisted entries. The user check
// and the read use the same open process directory (see readProcEnviron), so a
// pid that is reused cannot be read as another user's process.
func readProcessEnv(pid int) ([]string, bool) {
	if pid <= 0 {
		return nil, false
	}
	type result struct {
		env []string
		ok  bool
	}
	done := make(chan result, 1)
	go func() {
		raw, ok := readProcEnviron(pid)
		if !ok {
			done <- result{}
			return
		}
		done <- result{filterProcessEnv(raw), true}
	}()
	select {
	case r := <-done:
		return r.env, r.ok
	case <-time.After(processEnvDeadline):
		return nil, false
	}
}

// filterProcessEnv keeps the allowlisted entries of a NUL-separated
// environment; secrets are reduced to NAME=1 and every other entry is dropped.
func filterProcessEnv(raw []byte) []string {
	var out []string
	for _, kv := range strings.Split(string(raw), "\x00") {
		name, value, ok := strings.Cut(kv, "=")
		switch {
		case !ok || value == "":
		case slices.Contains(envValueNames, name):
			out = append(out, kv)
		case envEndpointNames[name] != "":
			out = append(out, name+"="+endpointKind(value, envEndpointNames[name]))
		case slices.Contains(envPresenceNames, name):
			out = append(out, name+"=1")
		}
	}
	return out
}

// readProfileFile reads a small settings file of an agent profile: it must be
// a regular file (a FIFO or device would block or lie) and is read within a
// size bound without ever waiting for a writer.
func readProfileFile(path string) ([]byte, bool) {
	if info, err := os.Stat(path); err != nil || !info.Mode().IsRegular() {
		return nil, false
	}
	f, err := openNonBlocking(path)
	if err != nil {
		return nil, false
	}
	defer func() { _ = f.Close() }()
	if info, err := f.Stat(); err != nil || !info.Mode().IsRegular() {
		return nil, false // replaced by a pipe between the checks
	}
	raw, err := io.ReadAll(io.LimitReader(f, 1<<20))
	return raw, err == nil
}

// secretPresent records that a JSON value is a non-empty string without
// keeping it.
type secretPresent bool

func (p *secretPresent) UnmarshalJSON(b []byte) error {
	*p = len(b) > 2 && b[0] == '"'
	return nil
}

// envLookup returns the value of name in a KEY=VALUE list.
func envLookup(env []string, name string) string {
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, name+"="); ok {
			return v
		}
	}
	return ""
}

// endpointKind reduces a base URL to "vendor" (the vendor's own host) or
// "custom" (anything else, including a value that cannot be parsed). The host
// itself is never kept.
func endpointKind(raw string, vendorHost string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err == nil && u.Host != "" && strings.EqualFold(u.Hostname(), vendorHost) && (u.Scheme == "https" || u.Scheme == "http") {
		return "vendor"
	}
	return "custom"
}

// detectSessionAccess classifies the session of harness running as pid.
// pid is the verified agent process; without it (pid 0, or a process that is
// not readable as the same user) the access is unknown: the profile files are
// never taken as the observed access of a process that could not be read.
func detectSessionAccess(harness string, pid int) sessionAccess {
	env, ok := processEnv(pid)
	if !ok {
		return sessionAccess{Mode: "unknown"}
	}
	switch harness {
	case "claude":
		return claudeAccess(env)
	case "codex":
		return codexAccess(env)
	}
	return sessionAccess{Mode: "unknown"}
}

// claudeAPIConfig reports whether the environment env and the settings of the
// profile in dir configure an API key, a token or a key helper (key), and a
// base URL other than the vendor's (custom). Only the facts are kept.
func claudeAPIConfig(env []string, dir string) (key, custom bool) {
	var settings struct {
		Env struct {
			BaseURL   string        `json:"ANTHROPIC_BASE_URL"`
			APIKey    secretPresent `json:"ANTHROPIC_API_KEY"`
			AuthToken secretPresent `json:"ANTHROPIC_AUTH_TOKEN"`
		} `json:"env"`
		APIKeyHelper secretPresent `json:"apiKeyHelper"`
	}
	if raw, ok := readProfileFile(filepath.Join(dir, "settings.json")); ok {
		_ = json.Unmarshal(raw, &settings)
	}
	// The process environment wins over the settings env block.
	base := envLookup(env, "ANTHROPIC_BASE_URL")
	if base == "" && settings.Env.BaseURL != "" {
		base = endpointKind(settings.Env.BaseURL, "api.anthropic.com")
	}
	key = envLookup(env, "ANTHROPIC_API_KEY") != "" || envLookup(env, "ANTHROPIC_AUTH_TOKEN") != "" ||
		bool(settings.Env.APIKey) || bool(settings.Env.AuthToken) || bool(settings.APIKeyHelper)
	return key, base == "custom"
}

func claudeProfileDir(env []string) string {
	dir := envLookup(env, "CLAUDE_CONFIG_DIR")
	if dir == "" || !filepath.IsAbs(dir) {
		dir = ""
		if home, err := os.UserHomeDir(); err == nil {
			dir = filepath.Join(home, ".claude")
		}
	}
	return dir
}

// ClaudeAPIAccess reports whether the Claude Code process whose environment
// lookup answers uses an API key, a token or a custom endpoint rather than the
// profile's login. profile is the profile directory its settings are read from.
// It reads only the caller's own environment.
func ClaudeAPIAccess(lookup func(string) (string, bool), profile string) bool {
	var env []string
	for _, name := range []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN"} {
		if v, ok := lookup(name); ok && v != "" {
			env = append(env, name+"=1")
		}
	}
	if v, ok := lookup("ANTHROPIC_BASE_URL"); ok && v != "" {
		env = append(env, "ANTHROPIC_BASE_URL="+endpointKind(v, "api.anthropic.com"))
	}
	key, custom := claudeAPIConfig(env, profile)
	return key || custom
}

func claudeAccess(env []string) sessionAccess {
	dir := claudeProfileDir(env)
	key, custom := claudeAPIConfig(env, dir)
	switch {
	case custom:
		return sessionAccess{Mode: "api", Kind: accessCustomEndpoint}
	case key:
		return sessionAccess{Mode: "api", Kind: accessAPIKey}
	}
	if info, err := os.Stat(filepath.Join(dir, ".credentials.json")); err == nil && info.Mode().IsRegular() {
		return sessionAccess{Mode: "subscription"}
	}
	return sessionAccess{Mode: "unknown"}
}

func codexAccess(env []string) sessionAccess {
	switch {
	case envLookup(env, "OPENAI_BASE_URL") == "custom":
		return sessionAccess{Mode: "api", Kind: accessCustomEndpoint}
	case envLookup(env, "OPENAI_API_KEY") != "":
		return sessionAccess{Mode: "api", Kind: accessAPIKey}
	}
	home := envLookup(env, "CODEX_HOME")
	if home == "" || !filepath.IsAbs(home) {
		home = ""
		if h, err := os.UserHomeDir(); err == nil {
			home = filepath.Join(h, ".codex")
		}
	}
	if raw, ok := readProfileFile(filepath.Join(home, "auth.json")); ok {
		var auth struct {
			Mode string `json:"auth_mode"`
		}
		if json.Unmarshal(raw, &auth) == nil && strings.EqualFold(auth.Mode, "chatgpt") {
			return sessionAccess{Mode: "subscription"}
		}
	}
	return sessionAccess{Mode: "unknown"}
}
