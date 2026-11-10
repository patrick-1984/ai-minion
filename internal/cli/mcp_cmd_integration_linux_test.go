//go:build linux

package cli

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/testutil/tmuxtest"
	"github.com/patrick-1984/ai-minion/internal/worker/agent"
)

// TestMCPSessionsListAgainstARealLocalWorker is the integration test of Story
// 2.10: a real Worker (store, tmux host, executor, local control socket) on a
// private tmux server, and "minion mcp" with its default Solo backend finding
// the Worker's socket the way a harness would.
func TestMCPSessionsListAgainstARealLocalWorker(t *testing.T) {
	srv := tmuxtest.New(t)
	srv.NewSession("adopted", "cat")

	// The Worker listens below XDG_RUNTIME_DIR; the directory is short because
	// unix socket paths are limited, and private because the client refuses a
	// socket whose directory is open to others.
	runDir, err := os.MkdirTemp("", "mn")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(runDir) })
	if err := os.Chmod(runDir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_RUNTIME_DIR", runDir)

	deps, p := coreDeps(t, map[string]string{})
	if err := agent.RegisterLocalAPI(deps.registry); err != nil {
		t.Fatal(err)
	}
	cancel, wait := startWorker(t, deps, p, "--tmux-socket", srv.Socket(), "--share-existing", "all", "--name", "solo")
	if _, err := os.Stat(filepath.Join(runDir, "minion", "worker.sock")); err != nil {
		t.Fatalf("the Worker did not open its control socket: %v", err)
	}

	// The production Solo wiring: the default backend and trust policy of
	// `minion mcp`.
	mdeps, _ := testMCPDeps(map[string]string{}, nil, nil)
	mdeps.backend = defaultMCPDeps(io.Discard).backend
	got, raw := rpc(t, mdeps, nil,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"sessions_list","arguments":{}}}`)

	var res struct {
		IsError           bool `json:"isError"`
		StructuredContent struct {
			Sessions []struct {
				Name string `json:"name"`
			} `json:"sessions"`
		} `json:"structuredContent"`
	}
	if err := json.Unmarshal(got[2], &res); err != nil {
		t.Fatalf("tools/call = %s (%v)", got[2], err)
	}
	if res.IsError || len(res.StructuredContent.Sessions) != 1 || res.StructuredContent.Sessions[0].Name != "adopted" {
		t.Fatalf("sessions_list through MCP = %s\nstdout:\n%s", got[2], raw)
	}

	cancel()
	if err := wait(); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(raw) == "" {
		t.Fatal("no protocol output")
	}
}
