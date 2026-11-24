package agent

import (
	"bytes"
	"encoding/json"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sort"
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

// claudeSrc locates the settings of a Claude process beyond the profile: its
// --settings argument (a file path or an inline JSON text) and its working
// directory (the project whose .claude/settings*.json apply).
type claudeSrc struct {
	Settings string
	Cwd      string
}

// processAccess returns the access-relevant environment and optional Claude
// settings path of a verified process. Tests replace it.
var processAccess = readProcessAccess

// managedSettingsFiles lists the machine-wide managed settings file and its
// drop-ins, in the order Claude Code merges them.
func managedSettingsFiles() []string {
	file := ClaudeManagedSettingsFile
	drop, _ := filepath.Glob(filepath.Join(filepath.Dir(file), "managed-settings.d", "*.json"))
	sort.Strings(drop)
	return append([]string{file}, drop...)
}

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

// readProcessAccess reads one same-user process within a deadline and reduces
// its environment to the allowlisted entries. The UID, environment and settings
// argument are read through the same open process directory.
func readProcessAccess(pid int, claude bool) ([]string, claudeSrc, bool) {
	if pid <= 0 {
		return nil, claudeSrc{}, false
	}
	type result struct {
		env []string
		src claudeSrc
		ok  bool
	}
	done := make(chan result, 1)
	go func() {
		raw, src, ok := readProcAccess(pid, claude)
		if !ok {
			done <- result{}
			return
		}
		done <- result{filterProcessEnv(raw), src, true}
	}()
	select {
	case r := <-done:
		return r.env, r.src, r.ok
	case <-time.After(processEnvDeadline):
		return nil, claudeSrc{}, false
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
	env, src, ok := processAccess(pid, harness == "claude")
	if !ok {
		return sessionAccess{Mode: "unknown"}
	}
	switch harness {
	case "claude":
		return claudeAccess(env, src)
	case "codex":
		return codexAccess(env)
	}
	return sessionAccess{Mode: "unknown"}
}

// claudeAPIConfig reports whether the environment env and the settings of the
// profile in dir configure an API key, a token or a key helper (key), and a
// base URL other than the vendor's (custom). Sources, lowest to highest
// precedence: user profile, the project's .claude settings, the inline
// --settings, managed settings; later fields override earlier ones. Only the
// classification leaves this function.
func claudeAPIConfig(env []string, dir string, src claudeSrc) (key, custom bool) {
	type claudeSettings struct {
		Env struct {
			BaseURL   string        `json:"ANTHROPIC_BASE_URL"`
			APIKey    secretPresent `json:"ANTHROPIC_API_KEY"`
			AuthToken secretPresent `json:"ANTHROPIC_AUTH_TOKEN"`
		} `json:"env"`
		APIKeyHelper secretPresent `json:"apiKeyHelper"`
	}
	var files []string
	if dir != "" {
		files = append(files, filepath.Join(dir, "settings.json"), filepath.Join(dir, "settings.local.json"))
	}
	for _, root := range claudeProjectDirs(src.Cwd) {
		files = append(files, filepath.Join(root, ".claude", "settings.json"), filepath.Join(root, ".claude", "settings.local.json"))
	}
	var settings claudeSettings
	merge := func(raw []byte) {
		next := settings
		if json.Unmarshal(raw, &next) == nil {
			settings = next
		}
	}
	read := func(file string) {
		if raw, ok := readProfileFile(file); ok {
			merge(raw)
		}
	}
	for _, file := range files {
		read(file)
	}
	if inline := strings.TrimSpace(src.Settings); strings.HasPrefix(inline, "{") {
		merge(bytes.TrimSpace([]byte(inline)))
	} else if inline != "" {
		read(inline)
	}
	for _, file := range managedSettingsFiles() {
		read(file)
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

// claudeProjectDirs lists the directories whose .claude settings apply to a
// Claude process started in cwd, lowest precedence first: the project root
// (the nearest ancestor with a .git entry, else the nearest with a .claude
// directory, as Claude Code finds it) and cwd itself when it is a
// subdirectory. A launch from a subdirectory thus still reads the
// repository's settings.
func claudeProjectDirs(cwd string) []string {
	if !filepath.IsAbs(cwd) {
		return nil
	}
	root := ""
	for dir := filepath.Clean(cwd); ; {
		if _, err := os.Lstat(filepath.Join(dir, ".git")); err == nil {
			root = dir
			break
		}
		if root == "" {
			if info, err := os.Stat(filepath.Join(dir, ".claude")); err == nil && info.IsDir() {
				root = dir
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	if root == "" || root == filepath.Clean(cwd) {
		return []string{filepath.Clean(cwd)}
	}
	return []string{root, filepath.Clean(cwd)}
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

// ClaudeExporterAPIConfig classifies the Claude session that spawned an
// exporter. It uses the inherited environment, profile settings and, when
// visible on a same-user Claude ancestor, the session's --settings file.
func ClaudeExporterAPIConfig(lookup func(string) (string, bool), profile string) (key, custom bool) {
	var env []string
	for _, name := range []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN"} {
		if v, ok := lookup(name); ok && v != "" {
			env = append(env, name+"=1")
		}
	}
	if v, ok := lookup("ANTHROPIC_BASE_URL"); ok && v != "" {
		env = append(env, "ANTHROPIC_BASE_URL="+endpointKind(v, "api.anthropic.com"))
	}
	return claudeAPIConfig(env, profile, parentClaudeSrc())
}

func claudeAccess(env []string, src claudeSrc) sessionAccess {
	dir := claudeProfileDir(env)
	key, custom := claudeAPIConfig(env, dir, src)
	switch {
	case custom:
		return sessionAccess{Mode: "api", Kind: accessCustomEndpoint}
	case key:
		return sessionAccess{Mode: "api", Kind: accessAPIKey}
	}
	if dir != "" {
		if info, err := os.Stat(filepath.Join(dir, ".credentials.json")); err == nil && info.Mode().IsRegular() {
			return sessionAccess{Mode: "subscription"}
		}
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
