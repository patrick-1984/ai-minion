//go:build windows

package agent

import (
	"testing"

	"github.com/patrick-1984/ai-minion/internal/config"
)

// An invalid session_host stops the start; it never falls back to auto.
func TestSessionHostSpecRefusesAnInvalidValue(t *testing.T) {
	env := map[string]string{"MINION_SESSION_HOST": "conptty"}
	res := &config.Resolver{Registry: config.Default, Role: config.ScopeWorker,
		LookupEnv: func(k string) (string, bool) { v, ok := env[k]; return v, ok }}
	if spec, _, err := sessionHostSpec(Settings{Resolver: res}); err == nil {
		t.Fatalf("spec = %q, want an error", spec)
	}
	env["MINION_SESSION_HOST"] = "conpty"
	spec, info, err := sessionHostSpec(Settings{Resolver: res})
	if err != nil || spec != "conpty" {
		t.Fatalf("conpty: %q %v", spec, err)
	}
	if info.Kind != "conpty" || !info.Hidden || info.Reason == "" {
		t.Fatalf("conpty info = %+v, want hidden ConPTY with a reason", info)
	}
}
