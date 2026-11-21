package agent

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestUsageCredentialProjectionKeepsAuthorityOutOfSafeLabels(t *testing.T) {
	seed := []byte(strings.Repeat("private-fixture-seed-", 2))
	const shortKey = "local-short-token"
	// Custom endpoints may embed a confidential routing value in the authority
	// or path, even without URL userinfo. No such value is a display hint.
	hidden, err := APIUsageAccount("custom", "https://LOCAL-SHORT-TOKEN.example.invalid/service/opaque-route", shortKey, seed, 1)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{shortKey, "example.invalid", "opaque-route", string(seed)} {
		assertUsageCredentialAbsent(t, hidden, value)
	}
	firstKey := "sameprefix" + strings.Repeat("unique-first-", 4)
	secondKey := "sameprefix" + strings.Repeat("unique-other-", 4)
	first, err := APIUsageAccount("custom", "https://endpoint-a.example.invalid/v1/", firstKey, seed, 1)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ endpoint, key string }{
		{"https://endpoint-a.example.invalid/v1", secondKey},
		{"https://endpoint-b.example.invalid/v1", firstKey},
	} {
		other, err := APIUsageAccount("custom", c.endpoint, c.key, seed, 1)
		if err != nil {
			t.Fatal(err)
		}
		if first.ID == other.ID || first.Label == other.Label ||
			!strings.HasPrefix(first.Label, "sameprefix...") || !strings.HasPrefix(other.Label, "sameprefix...") {
			t.Errorf("colliding display prefix concealed a distinct endpoint or key identity: %+v / %+v", first, other)
		}
		assertUsageCredentialAbsent(t, other, c.key)
	}
	for _, endpoint := range []string{
		"https://endpoint-a.example.invalid/v1?", "https://endpoint-a.example.invalid/v1?token=secret",
		"https://endpoint-a.example.invalid/v1#private",
	} {
		if _, err := APIUsageAccount("custom", endpoint, firstKey, seed, 1); err == nil {
			t.Errorf("an endpoint with query or fragment became a third account identity")
		}
	}
}

func assertUsageCredentialAbsent(t *testing.T, data any, sensitive string) {
	t.Helper()
	b, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.ToLower(string(b)), strings.ToLower(sensitive)) {
		t.Fatal("the projected credential metadata exposed a secret or raw endpoint")
	}
}
