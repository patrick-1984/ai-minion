package cli

import (
	"context"
	"strings"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/config"
)

// Before pairing, the line names the Hub and, when set, the words of the
// expected key.
func TestHubLineBeforePairing(t *testing.T) {
	dir := t.TempDir()
	env := map[string]string{"MINION_STATE_DIR": dir, "MINION_SERVER": "https://hub.example.com",
		"MINION_HUB_FINGERPRINT": strings.Repeat("a", 52)}
	res := &config.Resolver{Role: config.ScopeWorker, LookupEnv: func(k string) (string, bool) { v, ok := env[k]; return v, ok }}
	line := hubIdentityLine(context.Background(), res)
	if !strings.HasPrefix(line, "Hub: https://hub.example.com · not paired yet · expected key: ") {
		t.Fatalf("line = %q", line)
	}
	delete(env, "MINION_HUB_FINGERPRINT")
	if line := hubIdentityLine(context.Background(), res); line != "Hub: https://hub.example.com · not paired yet" {
		t.Fatalf("line = %q", line)
	}
}
