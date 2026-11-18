package cli

import (
	"context"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/config"
)

// An unpaired Worker says so; it never shows a key it has not pinned.
func TestHubLineBeforePairing(t *testing.T) {
	env := map[string]string{"MINION_STATE_DIR": t.TempDir(), "MINION_SERVER": "https://hub.example.com/?token=secret"}
	res := &config.Resolver{Role: config.ScopeWorker, LookupEnv: func(k string) (string, bool) { v, ok := env[k]; return v, ok }}
	if line := hubIdentityLine(context.Background(), res); line != "Hub key: none (this Worker is not paired)" {
		t.Fatalf("line = %q", line)
	}
}
