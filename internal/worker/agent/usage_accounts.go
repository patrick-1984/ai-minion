package agent

import (
	"bufio"
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/patrick-1984/ai-minion/internal/ops"
)

type nativeAuthStatus struct{ mode, status, provider string }

func (n nativeAuthStatus) Error() string { return n.status }

type nativeFailure struct {
	status     string
	retryAfter int64
}

func (e nativeFailure) Error() string { return e.status }

type nativeAccount struct {
	Plan             string `json:"planType"`
	SubscriptionType string `json:"subscriptionType"`
	APIKeySource     string `json:"apiKeySource"`
	APIProvider      string `json:"apiProvider"`
	Endpoint         string `json:"baseUrl"`
	AuthMethod       string `json:"authMethod"` // Claude Code
	Type             string `json:"type"`       // Codex
	AccountID        string `json:"accountId"`
	Email            string `json:"email"`
	OrgID            string `json:"orgId"`
}

type nativeWindow struct {
	Percent *float64 `json:"usedPercent"`
	Minutes *int64   `json:"windowDurationMins"`
	Reset   *int64   `json:"resetsAt"`
}
type nativeLimits struct {
	ID        string        `json:"limitId"`
	Primary   *nativeWindow `json:"primary"`
	Secondary *nativeWindow `json:"secondary"`
}
type codexLimits struct {
	Legacy *nativeLimits           `json:"rateLimits"`
	ByID   map[string]nativeLimits `json:"rateLimitsByLimitId"`
}

// A native login label is the full account address (it is not a secret; only
// the Worker owner and Hub administrators see it), with the domain in lower
// case. A digest suffix tells two workspaces of the same login apart. If the
// field is not a simple address, the account keeps its opaque label.
func nativeLoginLabel(provider, email, digest string) string {
	email = strings.TrimSpace(email)
	user, domain, ok := strings.Cut(email, "@")
	fallback := provider + " profile " + digest[:10]
	if !ok || user == "" || len(user) > 64 || len(email) > maxUsageLabel || len(domain) > 253 || len(domain) < 3 || !strings.Contains(domain, ".") {
		return fallback
	}
	for _, r := range domain {
		if r > 127 || !apiKeyChar(byte(r)) && r != '.' || r == '_' {
			return fallback
		}
	}
	for _, r := range user {
		if r <= ' ' || r == '@' || r == '/' || r == 127 {
			return fallback
		}
	}
	label := user + "@" + strings.ToLower(domain)
	// The digest suffix tells two workspaces of a login apart; it never
	// pushes the label past the wire bound and the address is never cut.
	if suffix := " / " + digest[:6]; len(label)+len(suffix) <= maxUsageLabel {
		label += suffix
	}
	return label
}

func subscriptionAccount(provider string, n nativeAccount, at int64) (ops.UsageAccount, bool) {
	provider = ops.UsageProvider(provider)
	id := n.AccountID
	if id == "" && n.Email != "" {
		id = strings.ToLower(strings.TrimSpace(n.Email))
	}
	if id == "" {
		return ops.UsageAccount{}, false
	}
	digest := ops.UsageID("subscription", provider, id, n.OrgID)
	plan := n.Plan
	if plan == "" {
		plan = n.SubscriptionType
	}
	return ops.UsageAccount{Plan: plan, TimeSource: "worker_measured", ID: digest, Provider: provider, Label: nativeLoginLabel(provider, n.Email, digest), AuthMode: "subscription", Source: "native-account", ObservedAt: at}, true
}

// APIUsageAccount fingerprints a credential explicitly identified by the local
// producer as the credential used for the given endpoint. Neither the URL path
// nor an entire key is returned. The prefix is a display hint only.
func APIUsageAccount(provider, endpoint, key string, domainSecret []byte, at int64) (ops.UsageAccount, error) {
	provider = ops.UsageProvider(provider)
	u, err := url.Parse(endpoint)
	if len(domainSecret) < 32 || err != nil || u.Hostname() == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || key == "" || len(key) > 4096 || len(provider) > 64 || provider == "" || strings.IndexFunc(key, unicode.IsControl) >= 0 {
		return ops.UsageAccount{}, errors.New("invalid API credential metadata")
	}
	host := strings.ToLower(u.Hostname())
	if addr, err := netip.ParseAddr(u.Hostname()); err == nil {
		host = addr.String()
	}
	port := u.Port()
	if u.Scheme == "https" && port == "443" || u.Scheme == "http" && port == "80" {
		port = ""
	}
	switch {
	case port != "":
		u.Host = net.JoinHostPort(host, port)
	case strings.Contains(host, ":"):
		u.Host = "[" + host + "]"
	default:
		u.Host = host
	}
	target := strings.TrimRight(u.String(), "/")
	h := hmac.New(sha256.New, domainSecret)
	h.Write([]byte(ops.UsageID("api-key", provider, target, key)))
	digest := hex.EncodeToString(h.Sum(nil))
	label := "Credential / " + digest[:10]
	// Keep at least 24 characters of a long, printable ASCII credential
	// hidden; suppress the complete prefix for short or non-ASCII keys.
	if len(key) >= 34 && strings.IndexFunc(key, func(c rune) bool { return c <= 32 || c > 126 }) < 0 {
		label = key[:10] + "... / " + digest[:10]
	}
	origin := (&url.URL{Scheme: u.Scheme, Host: u.Host}).String()
	if strings.Contains(strings.ToLower(origin), strings.ToLower(key)) {
		origin = ""
	}
	return ops.UsageAccount{EndpointOrigin: origin, KeyHint: label, ID: digest, Provider: provider, Label: label, AuthMode: "api", Source: "native-api-identity", ObservedAt: at}, nil
}

// NativeAPIUsageAccount uses only keys that fit a known, high-entropy
// provider format. A public Hub-specific salt prevents cross-Hub correlation.
// Weak or unknown formats require the optional privately shared domain seed.
func NativeAPIUsageAccount(provider, endpoint, key, hubPublic string, at int64) (ops.UsageAccount, error) {
	provider = ops.UsageProvider(provider)
	if len(hubPublic) < 32 {
		return ops.UsageAccount{}, errors.New("missing Hub scope")
	}
	validFormat := false
	switch provider {
	case "Anthropic":
		validFormat = strings.HasPrefix(key, "sk-ant-api03-") && len(key) >= 80
	case "OpenAI":
		validFormat = strings.HasPrefix(key, "sk-proj-") && len(key) >= 80
	case "OpenRouter":
		validFormat = strings.HasPrefix(key, "sk-or-v1-") && len(key) == 73
	case "DeepSeek":
		validFormat = strings.HasPrefix(key, "sk-") && len(key) == 35
	}
	seen := map[byte]bool{}
	for i := 3; i < len(key); i++ {
		b := key[i]
		if !apiKeyChar(b) {
			validFormat = false
		}
		seen[b] = true
	}
	if !validFormat || len(seen) < 12 {
		return ops.UsageAccount{}, errors.New("credential needs a private fingerprint seed")
	}
	scope := sha256.Sum256([]byte("minion-public-usage-domain-v1:" + hubPublic))
	return APIUsageAccount(provider, endpoint, key, scope[:], at)
}

func parseCodexWindows(l codexLimits) []ops.UsageWindow {
	limits := l.ByID
	if len(limits) == 0 && l.Legacy != nil {
		key := l.Legacy.ID
		if key == "" {
			key = "codex"
		}
		limits = map[string]nativeLimits{key: *l.Legacy}
	}
	keys := make([]string, 0, len(limits))
	for k := range limits {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var out []ops.UsageWindow
	for _, k := range keys {
		row := limits[k]
		for i, w := range []*nativeWindow{row.Primary, row.Secondary} {
			if w == nil {
				continue
			}
			scope := k + "." + []string{"primary", "secondary"}[i]
			normalized := ops.UsageWindow{Scope: scope, Period: "rolling", UsedPercent: w.Percent}
			if w.Minutes != nil && *w.Minutes > 0 && *w.Minutes <= 5259600 {
				v := *w.Minutes * 60000
				normalized.DurationMS = &v
			}
			if w.Reset != nil && *w.Reset > 0 && *w.Reset < 253402300800 {
				v := *w.Reset * 1000
				normalized.ResetsAt = &v
			}
			out = append(out, normalized)
		}
	}
	return out
}

type codexStatusPipe struct {
	scanner *bufio.Scanner
	input   *json.Encoder
}

func (p codexStatusPipe) request(id int, method string, params any, result any) error {
	request := map[string]any{"method": method, "id": id}
	if params != nil {
		request["params"] = params
	}
	if err := p.input.Encode(request); err != nil {
		return err
	}
	for range 64 {
		if !p.scanner.Scan() {
			return errors.New("native status unavailable")
		}
		var msg struct {
			ID     *int            `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  json.RawMessage `json:"error"`
		}
		if err := json.Unmarshal(p.scanner.Bytes(), &msg); err != nil {
			return errors.New("invalid native status")
		}
		if msg.ID == nil || *msg.ID != id {
			continue
		}
		if len(msg.Error) > 0 || len(msg.Result) == 0 {
			var failure struct {
				Code int `json:"code"`
				Data struct {
					RetryAfterSeconds int64 `json:"retryAfterSeconds"`
					Status            int   `json:"status"`
				} `json:"data"`
			}
			_ = json.Unmarshal(msg.Error, &failure)
			if failure.Code == 429 || failure.Data.Status == 429 || failure.Data.RetryAfterSeconds > 0 {
				return nativeFailure{"rate_limited", max(0, min(31536000, failure.Data.RetryAfterSeconds))}
			}
			return nativeFailure{"unavailable", 0}
		}
		if result != nil {
			return json.Unmarshal(msg.Result, result)
		}
		return nil
	}
	return errors.New("native response limit reached")
}

func nativeCommand(ctx context.Context, name string, args ...string) (*exec.Cmd, error) {
	path, err := exec.LookPath(name)
	if err != nil || !filepath.IsAbs(path) {
		return nil, errors.New("harness unavailable")
	}
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Dir = filepath.Dir(path)
	cmd.Stderr = io.Discard
	cmd.WaitDelay = 250 * time.Millisecond
	return cmd, nil
}

// codexAccount uses an independent native stdio connection. It never opens,
// lists or resumes a thread and does not send the harness a prompt.
func codexAccount(ctx context.Context, expected string) (ops.UsageAccount, error) {
	cmd, err := nativeCommand(ctx, "codex", "app-server")
	if err != nil {
		return ops.UsageAccount{}, err
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return ops.UsageAccount{}, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return ops.UsageAccount{}, err
	}
	if err = cmd.Start(); err != nil {
		return ops.UsageAccount{}, err
	}
	defer func() { _ = stdin.Close(); _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	scan := bufio.NewScanner(io.LimitReader(stdout, 512<<10))
	scan.Buffer(make([]byte, 4096), 128<<10)
	p := codexStatusPipe{scan, json.NewEncoder(stdin)}
	return readCodexAccount(p, expected, time.Now())
}

func readCodexAccount(p codexStatusPipe, expected string, now time.Time) (ops.UsageAccount, error) {
	var err error
	if err = p.request(1, "initialize", map[string]any{"clientInfo": map[string]string{"name": "minion_usage", "version": "1"}}, nil); err != nil {
		return ops.UsageAccount{}, err
	}
	if err = p.input.Encode(map[string]string{"method": "initialized"}); err != nil {
		return ops.UsageAccount{}, err
	}
	var who struct {
		Account *nativeAccount `json:"account"`
	}
	if err = p.request(2, "account/read", map[string]bool{"refreshToken": false}, &who); err != nil {
		return ops.UsageAccount{}, err
	}
	if who.Account == nil {
		return ops.UsageAccount{}, nativeAuthStatus{"unknown", "signed_out", "OpenAI"}
	}
	if who.Account.Type != "chatgpt" {
		mode := "unknown"
		if who.Account.Type == "apiKey" || who.Account.Type == "amazonBedrock" {
			mode = "api"
		}
		return ops.UsageAccount{}, nativeAuthStatus{mode, "configured", map[string]string{"amazonBedrock": "Amazon Bedrock", "apiKey": "OpenAI"}[who.Account.Type]}
	}
	a, ok := subscriptionAccount("OpenAI", *who.Account, now.UnixMilli())
	if !ok {
		return a, errors.New("account identity unavailable")
	}
	a.Source = "codex-account"
	if expected != "" {
		if a.ID != expected {
			return ops.UsageAccount{}, nativeFailure{"account_changed", 0}
		}
		var limits codexLimits
		if err = p.request(3, "account/rateLimits/read", nil, &limits); err != nil {
			return ops.UsageAccount{}, err
		}
		a.Windows = parseCodexWindows(limits)
		a.Source = "codex-app-server"
	}
	return a, nil
}

func discoverUsageAccounts(ctx context.Context, inventory []ops.UsageHarness, hubPublic string) []ops.UsageAccount {
	var accounts []ops.UsageAccount
	for i := range inventory {
		h := &inventory[i]
		if h.Status != "installed" || !slices.Contains([]string{"claude", "codex", "opencode"}, h.Name) {
			continue
		}
		h.AuthMode = "unknown"
		h.AccountStatus = "unavailable"
		child, cancel := context.WithTimeout(ctx, 5*time.Second)
		switch h.Name {
		case "codex":
			h.Providers = []string{"OpenAI"}
			if a, err := codexAccount(child, ""); err == nil {
				accounts = append(accounts, a)
				h.AuthMode = "subscription"
				h.AccountStatus = "configured"
			} else {
				var status nativeAuthStatus
				if errors.As(err, &status) {
					h.AuthMode, h.AccountStatus = status.mode, status.status
					if status.provider != "" {
						h.Providers = []string{status.provider}
					}
				}
			}
		case "opencode":
			cmd, err := nativeCommand(child, "opencode", "auth", "list", "--format", "json")
			if err == nil {
				var raw usageOutput
				cmd.Stdout = &raw
				if cmd.Run() == nil {
					h.AuthMode, h.AccountStatus, h.Providers = openCodeAuth(raw)
				}
			}
		case "claude":
			h.Providers = []string{"Anthropic"}
			if n, ok := readClaudeAuth(child, ""); ok {
				switch n.AuthMethod {
				case "claude.ai":
					h.AuthMode = "subscription"
					h.AccountStatus = "configured"
					if a, ok := subscriptionAccount("Anthropic", n, time.Now().UnixMilli()); ok {
						accounts = append(accounts, a)
					}
				case "api_key", "api_key_helper":
					h.AuthMode = "api"
					h.AccountStatus = "configured"
					if a, ok := claudeNativeKey(n, hubPublic); ok {
						accounts = append(accounts, a)
					}
				case "third_party", "oauth_token":
					h.AccountStatus = "configured"
				case "none":
					h.AccountStatus = "signed_out"
				}
			}
		}
		cancel()
	}
	return accounts
}

// openCodeAuth recognizes built-in provider IDs from a native connection
// listing. It ignores raw integration URLs, credential IDs, labels and values.
// A generic OAuth connection alone does not prove subscription entitlement.
func openCodeAuth(raw []byte) (string, string, []string) {
	var integrations []struct {
		ID          string `json:"id"`
		Connections []struct {
			Type   string `json:"type"`
			Method string `json:"method"`
		} `json:"connections"`
	}
	if err := json.Unmarshal(raw, &integrations); err != nil {
		return "unknown", "unavailable", nil
	}
	known := map[string]string{"deepseek": "DeepSeek", "openrouter": "OpenRouter", "anthropic": "Anthropic", "openai": "OpenAI", "google": "Google", "opencode-go": "OpenCode Go", "opencode": "OpenCode Zen"}
	modes := map[string]bool{}
	providers := []string{}
	for _, i := range integrations {
		name := known[i.ID]
		if name == "" || len(i.Connections) == 0 {
			continue
		}
		providers = append(providers, name)
		for _, c := range i.Connections {
			if c.Method == "key" || (c.Type == "env" && i.ID == "deepseek") {
				modes["api"] = true
			} else {
				modes["unknown"] = true
			}
		}
	}
	slices.Sort(providers)
	providers = slices.Compact(providers)
	if len(providers) == 0 {
		return "unknown", "unavailable", nil
	}
	mode := "unknown"
	if len(modes) > 1 {
		mode = "mixed"
	} else if modes["api"] {
		mode = "api"
	}
	return mode, "configured", providers
}

func apiKeyChar(b byte) bool {
	return b >= 'A' && b <= 'Z' || b >= 'a' && b <= 'z' || b >= '0' && b <= '9' || b == '_' || b == '-'
}

func readClaudeAuth(ctx context.Context, profileDir string) (nativeAccount, bool) {
	var n nativeAccount
	cmd, err := nativeCommand(ctx, "claude", "auth", "status")
	if err != nil {
		return n, false
	}
	if profileDir != "" {
		if !filepath.IsAbs(profileDir) {
			return n, false
		}
		cmd.Env = append(os.Environ(), "CLAUDE_CONFIG_DIR="+profileDir)
	}
	var raw usageOutput
	cmd.Stdout = &raw
	err = cmd.Run()
	var exit *exec.ExitError
	if err != nil && (!errors.As(err, &exit) || exit.ExitCode() != 1) {
		return n, false
	}
	ok := decodeClaudeStatus(raw, &n)
	return n, ok
}

func decodeClaudeStatus(raw []byte, n *nativeAccount) bool {
	return json.NewDecoder(bytes.NewReader(raw)).Decode(n) == nil
}

func claudeNativeKey(n nativeAccount, hubPublic string) (ops.UsageAccount, bool) {
	if hubPublic == "" || n.AuthMethod != "api_key" || n.APIKeySource != "ANTHROPIC_API_KEY" {
		return ops.UsageAccount{}, false
	}
	endpoint := n.Endpoint
	if endpoint == "" {
		endpoint = os.Getenv("ANTHROPIC_BASE_URL")
	}
	if endpoint == "" && n.APIProvider == "firstParty" {
		endpoint = "https://api.anthropic.com"
	}
	if endpoint == "" {
		return ops.UsageAccount{}, false
	}
	a, err := NativeAPIUsageAccount("Anthropic", endpoint, os.Getenv("ANTHROPIC_API_KEY"), hubPublic, time.Now().UnixMilli())
	return a, err == nil
}

// CurrentClaudeSubscription resolves the native login while the local owner
// is configuring this specific profile. It is never called per turn or hook.
func CurrentClaudeSubscription(ctx context.Context, profileDir string) (ops.UsageAccount, bool) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	n, ok := readClaudeAuth(ctx, profileDir)
	if !ok || n.AuthMethod != "claude.ai" {
		return ops.UsageAccount{}, false
	}
	return subscriptionAccount("Anthropic", n, time.Now().UnixMilli())
}
