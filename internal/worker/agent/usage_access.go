package agent

import (
	"encoding/json"
	"io"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/patrick-1984/ai-minion/internal/ops"
)

// sessionAccess is how a harness session authenticates, read from the
// environment of its own process and from the harness settings files. Only
// the kind and the endpoint host are kept: no key, token or URL path is ever
// read into a value that leaves this file.
type sessionAccess struct {
	Mode     string // "subscription", "api" or "unknown"
	Endpoint string // host of a custom endpoint, empty for the vendor's own
}

// processEnv returns the access-relevant environment of a process, or false
// when it is not readable or not trustworthy (another user, another
// platform, vanished pid, a read that stalls). Tests replace it.
var processEnv = readProcessEnv

// Environment names the Worker looks at. Only the endpoint and the profile
// directory keep their value; a secret keeps nothing but the fact that it is
// set (the entry becomes NAME=1).
var (
	envValueNames    = []string{"ANTHROPIC_BASE_URL", "OPENAI_BASE_URL", "CLAUDE_CONFIG_DIR", "CODEX_HOME"}
	envPresenceNames = []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "OPENAI_API_KEY"}
)

const (
	processEnvLimit    = 1 << 20
	processEnvDeadline = 500 * time.Millisecond
)

// readProcessEnv reads /proc/<pid>/environ of a process of the same user,
// within a deadline, and reduces it to the allowlisted entries.
func readProcessEnv(pid int) ([]string, bool) {
	if pid <= 0 || !sameUserProcess(pid) {
		return nil, false
	}
	type result struct {
		env []string
		ok  bool
	}
	done := make(chan result, 1)
	go func() {
		f, err := openNonBlocking("/proc/" + strconv.Itoa(pid) + "/environ")
		if err != nil {
			done <- result{}
			return
		}
		defer func() { _ = f.Close() }()
		raw, err := io.ReadAll(io.LimitReader(f, processEnvLimit))
		if err != nil {
			done <- result{}
			return
		}
		// The process may have ended and its pid been reused while reading.
		if !sameUserProcess(pid) {
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

// endpointHost returns the host[:port] of a base URL, or "" for the vendor's
// own host or an unusable value. It never returns anything that could carry a
// credential: a URL with user info, a host that is not a plain DNS name or IP
// address, or a name that looks like a key or token.
func endpointHost(raw string, vendorHosts ...string) string {
	raw = strings.TrimSpace(raw)
	if !strings.Contains(raw, "://") || strings.ContainsAny(raw, "@ \t\\") {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return ""
	}
	host := strings.ToLower(u.Host)
	for _, v := range vendorHosts {
		if host == v {
			return ""
		}
	}
	name := strings.ToLower(u.Hostname())
	if port := u.Port(); port != "" {
		if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
			return ""
		}
	}
	if len(host) > 253 || !plainHostName(name) || !ops.UsageEndpointHost(host) {
		return ""
	}
	return host
}

// plainHostName accepts an IP address or a DNS name of valid labels that
// does not look like a credential (a key prefix or an overlong label).
func plainHostName(name string) bool {
	if _, err := netip.ParseAddr(name); err == nil {
		return true
	}
	if name == "" || len(name) > 253 {
		return false
	}
	for _, label := range strings.Split(name, ".") {
		if label == "" || len(label) > 40 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, p := range []string{"sk-", "sk_", "pk-", "key-", "ghp_", "bearer", "token"} {
			if strings.HasPrefix(label, p) {
				return false
			}
		}
		for i := 0; i < len(label); i++ {
			c := label[i]
			if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
				return false
			}
		}
	}
	return true
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

func claudeAccess(env []string) sessionAccess {
	dir := envLookup(env, "CLAUDE_CONFIG_DIR")
	if dir == "" || !filepath.IsAbs(dir) {
		dir = ""
		if home, err := os.UserHomeDir(); err == nil {
			dir = filepath.Join(home, ".claude")
		}
	}
	// The settings env block and apiKeyHelper count as configuration of the
	// session; the process environment wins over them. Only the endpoint is
	// kept; keys are checked for presence and dropped.
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
	baseRaw := envLookup(env, "ANTHROPIC_BASE_URL")
	if baseRaw == "" {
		baseRaw = settings.Env.BaseURL
	}
	base := endpointHost(baseRaw, "api.anthropic.com")
	key := envLookup(env, "ANTHROPIC_API_KEY") != "" || envLookup(env, "ANTHROPIC_AUTH_TOKEN") != "" ||
		bool(settings.Env.APIKey) || bool(settings.Env.AuthToken) || bool(settings.APIKeyHelper)
	if key || base != "" {
		return sessionAccess{Mode: "api", Endpoint: base}
	}
	if info, err := os.Stat(filepath.Join(dir, ".credentials.json")); err == nil && info.Mode().IsRegular() {
		return sessionAccess{Mode: "subscription"}
	}
	return sessionAccess{Mode: "unknown"}
}

func codexAccess(env []string) sessionAccess {
	base := endpointHost(envLookup(env, "OPENAI_BASE_URL"), "api.openai.com")
	if envLookup(env, "OPENAI_API_KEY") != "" || base != "" {
		return sessionAccess{Mode: "api", Endpoint: base}
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
