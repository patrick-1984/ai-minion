package agent

import (
	"bufio"
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/ops"
)

func TestUsageNativeReadOnlyCodexRPC(t *testing.T) {
	now := time.Now()
	email := "example@example.invalid"
	a, _ := subscriptionAccount("OpenAI", nativeAccount{Email: email}, now.UnixMilli())
	var sent bytes.Buffer
	reply := strings.Join([]string{
		`{"id":1,"result":{}}`,
		`{"id":2,"result":{"account":{"type":"chatgpt","email":"example@example.invalid","privateField":"private fixture"}}}`,
		`{"id":3,"result":{"rateLimits":{"limitId":"duplicate","primary":{"usedPercent":91}},"rateLimitsByLimitId":{"codex":{"primary":{"usedPercent":0,"windowDurationMins":300,"resetsAt":1780000000},"secondary":null}}}}`,
	}, "\n")
	out, err := readCodexAccount(codexStatusPipe{bufio.NewScanner(strings.NewReader(reply)), json.NewEncoder(&sent)}, a.ID, now)
	if err != nil {
		t.Fatal(err)
	}
	if out.ID != a.ID || len(out.Windows) != 1 || out.Windows[0].UsedPercent == nil || *out.Windows[0].UsedPercent != 0 {
		t.Fatalf("native mapping: %+v", out)
	}
	var methods []string
	s := bufio.NewScanner(&sent)
	for s.Scan() {
		var m struct {
			Method string         `json:"method"`
			Params map[string]any `json:"params"`
		}
		if err := json.Unmarshal(s.Bytes(), &m); err != nil {
			t.Fatal(err)
		}
		methods = append(methods, m.Method)
		if m.Method == "account/read" && m.Params["refreshToken"] != false {
			t.Fatal("forced token refresh")
		}
	}
	if strings.Join(methods, ",") != "initialize,initialized,account/read,account/rateLimits/read" {
		t.Fatal(methods)
	}
	b, _ := json.Marshal(out)
	if bytes.Contains(b, []byte("private fixture")) || !strings.HasPrefix(out.Label, email+" / ") {
		t.Fatalf("native private data escaped or the label is not the full address: %q", out.Label)
	}
	sent.Reset()
	if _, err = readCodexAccount(codexStatusPipe{bufio.NewScanner(strings.NewReader(reply)), json.NewEncoder(&sent)}, ops.UsageID("other"), now); err == nil || strings.Contains(sent.String(), "rateLimits/read") {
		t.Fatal("account switch queried the wrong account")
	}
}

func TestUsageAPICredentialIdentity(t *testing.T) {
	secret := bytes.Repeat([]byte{9}, 32)
	key1 := "sameprefix" + strings.Repeat("x", 32)
	key2 := "sameprefix" + strings.Repeat("y", 32)
	a, err := APIUsageAccount("DeepSeek", "https://api.example/v1/", key1, secret, 1)
	if err != nil {
		t.Fatal(err)
	}
	b, err := APIUsageAccount("DeepSeek", "https://API.EXAMPLE/v1", key2, secret, 1)
	if err != nil {
		t.Fatal(err)
	}
	if a.ID == b.ID || !strings.HasPrefix(a.Label, "sameprefix...") || !strings.HasPrefix(b.Label, "sameprefix...") {
		t.Fatal("prefix collided with identity")
	}
	c, _ := APIUsageAccount("DeepSeek", "https://API.EXAMPLE/v1", key1, secret, 1)
	if c.ID != a.ID {
		t.Fatal("endpoint canonicalization")
	}
	data, _ := json.Marshal(a)
	if bytes.Contains(data, []byte(key1)) {
		t.Fatal("credential disclosed")
	}
	d, _ := APIUsageAccount("DeepSeek", "https://api.example/v1", "z7short", secret, 1)
	if strings.Contains(d.Label, "z7short") {
		t.Fatal("short credential exposed")
	}
	if _, err = APIUsageAccount("DeepSeek", "https://u:pw@api.example/v1", key1, secret, 1); err == nil {
		t.Fatal("accepted embedded credentials")
	}
}

func TestUsageDomainFingerprintAndStoredProjection(t *testing.T) {
	// Fixed, synthetic provider-shaped values exercise the nonsecret hint
	// without reading the machine's environment or a real credential file.
	key := "sk-ant-api03-" + strings.Repeat("0123456789abcdefghijklmnopqrstuv", 3)
	scope := strings.Repeat("a", 64)
	a, err := NativeAPIUsageAccount("Anthropic", "https://api.example/v1", key, scope, 1)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := NativeAPIUsageAccount("Anthropic", "https://other.example/v1", key, scope, 1)
	c, _ := NativeAPIUsageAccount("Anthropic", "https://api.example/v1", key, strings.Repeat("b", 64), 1)
	if a.ID == c.ID || a.ID == b.ID {
		t.Fatal("different Hub or endpoint aliased")
	}
	if _, err := NativeAPIUsageAccount("Anthropic", "https://api.example", "short", scope, 1); err == nil {
		t.Fatal("fingerprinted a weak credential with public scope")
	}
	if mode, status, providers := openCodeAuth([]byte(`[{"id":"deepseek","connections":[{"type":"env","name":"DEEPSEEK_API_KEY"}]},{"id":"https://example.invalid/private","connections":[{"type":"credential","value":"private"}]}]`)); mode != "api" || status != "configured" || len(providers) != 1 || providers[0] != "DeepSeek" {
		t.Fatalf("unsafe/incomplete native inventory: %s %s %v", mode, status, providers)
	}
}

func TestUsageCanonicalProviderAndEndpointIdentity(t *testing.T) {
	seed := bytes.Repeat([]byte{0x14}, 32)
	key := "provider-fixture-key-for-deduplication"
	id := func(provider, endpoint string) string {
		t.Helper()
		a, err := APIUsageAccount(provider, endpoint, key, seed, 1)
		if err != nil {
			t.Fatal(err)
		}
		return a.ID
	}
	if id(" OpenAI ", "https://API.EXAMPLE:443/v1/") != id("open ai", "https://api.example/v1") {
		t.Fatal("provider alias/default TLS port split identity")
	}
	if id("DeepSeek", "http://[::1]:80/v1") != id("deepseek-ai", "http://[::1]/v1") {
		t.Fatal("IPv6/default HTTP port split identity")
	}
	base := id("OpenAI", "https://api.example/v1")
	if id("Anthropic", "https://api.example/v1") == base || id("OpenAI", "https://api.example/V1") == base || id("OpenAI", "https://api.example:8443/v1") == base {
		t.Fatal("distinct provider, path or port merged")
	}
	if id("teamA", "https://api.example/v1") == id("teama", "https://api.example/v1") {
		t.Fatal("distinct custom namespaces merged")
	}
}

func TestOpenRouterOriginAndHintsForDistinctCredentials(t *testing.T) {
	scope := strings.Repeat("a", 64)
	ids := map[string]bool{}
	for _, ending := range []string{"a", "b", "c", "d", "e"} {
		key := "sk-or-v1-" + strings.Repeat("0123456789abcdef", 3) + "0123456789abcde" + ending
		a, err := NativeAPIUsageAccount("openrouter.ai", "https://OPENROUTER.AI:443/private-proxy-path", key, scope, 3)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal(a)
		if a.Provider != "OpenRouter" || a.EndpointOrigin != "" || !strings.HasPrefix(a.KeyHint, key[:10]+"... / ") || ids[a.ID] || bytes.Contains(b, []byte(key)) || bytes.Contains(b, []byte("private-proxy-path")) {
			t.Fatalf("unsafe or ambiguous API metadata: %s", a.ID)
		}
		ids[a.ID] = true
	}
	short, err := APIUsageAccount("test", "https://Z7SHORT.example.invalid/private", "z7short", []byte(scope), 1)
	if err != nil || short.EndpointOrigin != "" || strings.Contains(strings.ToLower(short.KeyHint), "z7short") {
		t.Fatal("credential in hostname was disclosed")
	}
}

func TestNativeLoginLabelIsTheFullAddress(t *testing.T) {
	digest := ops.UsageID("subscription", "openai", "x")
	for _, c := range []struct{ email, want string }{
		{"patrick@Formula5.com", "patrick@formula5.com / " + digest[:6]},
		{" a.b+c@example.org ", "a.b+c@example.org / " + digest[:6]},
		{"cac797@formula5.com", "cac797@formula5.com / " + digest[:6]},
		{"not an address", "openai profile " + digest[:10]},
		{"a b@example.org", "openai profile " + digest[:10]},
		{"", "openai profile " + digest[:10]},
	} {
		if got := nativeLoginLabel("openai", c.email, digest); got != c.want {
			t.Errorf("%q: got %q, want %q", c.email, got, c.want)
		}
	}
}

func TestNativeLoginLabelLongAddressStaysWithinTheWireBound(t *testing.T) {
	digest := ops.UsageID("subscription", "anthropic", "x")
	user := strings.Repeat("u", 64)
	long := user + "@" + strings.Repeat("d", 60) + "." + strings.Repeat("e", 60) + ".example.org" // 64+1+~135
	// Older Hubs accept 96 bytes: a longer login gets the opaque label, never a cut.
	if got := nativeLoginLabel("anthropic", long, digest); got != "anthropic profile "+digest[:10] {
		t.Fatalf("label %q (%d bytes)", got, len(got))
	}
	short := "user@example.com"
	if got := nativeLoginLabel("anthropic", short, digest); !strings.HasPrefix(got, short) || len(got) > maxUsageLabel {
		t.Fatalf("label %q", got)
	}
	// An address of exactly 96 bytes is kept whole, without the suffix.
	exact := strings.Repeat("u", 64) + "@" + strings.Repeat("d", 27) + ".com"
	if len(exact) != 96 {
		t.Fatalf("fixture %d", len(exact))
	}
	if got := nativeLoginLabel("anthropic", exact, digest); got != exact {
		t.Fatalf("label %q", got)
	}
	if got := nativeLoginLabel("anthropic", exact+"x", digest); got != "anthropic profile "+digest[:10] {
		t.Fatalf("got %q", got)
	}
}

func TestAccessAccountsAreExplicitAggregates(t *testing.T) {
	a, _ := accessAccount("Anthropic", sessionAccess{Mode: "api", Kind: accessAPIKey}, 1, "")
	b, _ := accessAccount("Anthropic", sessionAccess{Mode: "api", Kind: accessCustomEndpoint}, 1, "")
	if a.ID == b.ID || a.Label != "API keys (not told apart)" || b.Label != "Custom endpoint (not told apart)" {
		t.Fatalf("%+v %+v", a, b)
	}
}

func TestMergeUsageAccountsKeepsSessionAPIAccounts(t *testing.T) {
	scan := []ops.UsageAccount{{ID: "sub", Label: "a@b.example"}}
	session := []ops.UsageAccount{{ID: "sub", Label: "dup"}, {ID: "gw", Label: "API keys at gw.example (not told apart)"}}
	got := mergeUsageAccounts(scan, session)
	if len(got) != 2 || got[0].Label != "a@b.example" || got[1].ID != "gw" {
		t.Fatalf("%+v", got)
	}
}
