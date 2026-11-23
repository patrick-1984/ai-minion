//go:build unix

package agent

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/version"
)

// An API key that only the Worker's own environment carries does not turn the
// profile's subscription into API access.
func TestClaudeProfileProbeIgnoresTheWorkersAmbientKey(t *testing.T) {
	bin := t.TempDir()
	script := "#!/bin/sh\nif [ -n \"$ANTHROPIC_API_KEY\" ]; then echo '{\"authMethod\":\"api_key\"}'; else echo '{\"authMethod\":\"claude.ai\",\"email\":\"a@b.example\"}'; fi\n"
	if err := os.WriteFile(filepath.Join(bin, "claude"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("ANTHROPIC_API_KEY", "worker-only")
	a, ok := CurrentClaudeSubscription(context.Background(), t.TempDir())
	if !ok || a.AuthMode != "subscription" {
		t.Fatalf("got %+v ok=%t", a, ok)
	}
}

// The answer is written to the Hub before the Worker restarts.
func TestWorkerUpdateRestartWaitsForTheReply(t *testing.T) {
	withVersion(t, "v0.9.0")
	inst := &fakeInstaller{res: version.Result{Tag: "v0.9.1"}}
	u, restarted := newTestUpdate(t, inst)
	sent := make(chan struct{})
	ctx := context.WithValue(context.Background(), replySentKey{}, (<-chan struct{})(sent))
	if _, err := u.handleUpdate(ctx, updateRequest("worker_update", map[string]any{"worker_id": "w-self", "version": "v0.9.1"})); err != nil {
		t.Fatal(err)
	}
	select {
	case <-restarted:
		t.Fatal("restarted before the reply was written")
	case <-time.After(2500 * time.Millisecond):
	}
	close(sent)
	select {
	case <-restarted:
	case <-time.After(5 * time.Second):
		t.Fatal("no restart after the reply")
	}
}
