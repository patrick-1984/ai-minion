package agent

import (
	"encoding/json"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

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

// processEnv returns the environment of a process, or false when it is not
// readable (other user, another platform, vanished pid). Tests replace it.
var processEnv = func(pid int) ([]string, bool) {
	if pid <= 0 {
		return nil, false
	}
	f, err := os.Open("/proc/" + strconv.Itoa(pid) + "/environ")
	if err != nil {
		return nil, false
	}
	defer func() { _ = f.Close() }()
	raw, err := io.ReadAll(io.LimitReader(f, 1<<20))
	if err != nil {
		return nil, false
	}
	return strings.Split(string(raw), "\x00"), true
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
// own host or an unusable value.
func endpointHost(raw string, vendorHosts ...string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return ""
	}
	host := strings.ToLower(u.Host)
	for _, v := range vendorHosts {
		if host == v {
			return ""
		}
	}
	if !ops.UsageEndpointHost(host) {
		return ""
	}
	return host
}

// detectSessionAccess classifies the session of harness running as pid.
func detectSessionAccess(harness string, pid int) sessionAccess {
	env, _ := processEnv(pid)
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
	if dir == "" {
		if home, err := os.UserHomeDir(); err == nil {
			dir = filepath.Join(home, ".claude")
		}
	}
	// The settings env block and apiKeyHelper count as configuration of the
	// session; the process environment wins over them.
	var settings struct {
		Env          map[string]any `json:"env"`
		APIKeyHelper string         `json:"apiKeyHelper"`
	}
	if f, err := os.Open(filepath.Join(dir, "settings.json")); err == nil {
		_ = json.NewDecoder(io.LimitReader(f, 1<<20)).Decode(&settings)
		_ = f.Close()
	}
	get := func(name string) string {
		if v := envLookup(env, name); v != "" {
			return v
		}
		v, _ := settings.Env[name].(string)
		return v
	}
	base := endpointHost(get("ANTHROPIC_BASE_URL"), "api.anthropic.com")
	key := get("ANTHROPIC_API_KEY") != "" || get("ANTHROPIC_AUTH_TOKEN") != "" || settings.APIKeyHelper != ""
	switch {
	case key || base != "":
		return sessionAccess{Mode: "api", Endpoint: base}
	}
	if _, err := os.Stat(filepath.Join(dir, ".credentials.json")); err == nil {
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
	if home == "" {
		if h, err := os.UserHomeDir(); err == nil {
			home = filepath.Join(h, ".codex")
		}
	}
	if f, err := os.Open(filepath.Join(home, "auth.json")); err == nil {
		defer func() { _ = f.Close() }()
		var auth struct {
			Mode string `json:"auth_mode"`
		}
		if json.NewDecoder(io.LimitReader(f, 1<<20)).Decode(&auth) == nil && strings.EqualFold(auth.Mode, "chatgpt") {
			return sessionAccess{Mode: "subscription"}
		}
	}
	return sessionAccess{Mode: "unknown"}
}
