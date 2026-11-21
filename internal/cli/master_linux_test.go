//go:build linux && !android

package cli

import (
	"encoding/json"
	"slices"
	"testing"
)

func TestMasterClaudeLaunchAddsOnlyItsInlineServer(t *testing.T) {
	want := []string{"/test/claude", "-p", "--input-format", "stream-json"}
	argv, err := masterClaudeArgs(want[0], "/test/minion", "/test/master.sock", want[1:])
	if err != nil {
		t.Fatal(err)
	}
	if len(argv) != len(want)+2 || !slices.Equal(argv[:len(want)], want) || argv[len(want)] != "--mcp-config" {
		t.Fatalf("the launcher changed existing arguments: %v", argv)
	}
	var cfg struct {
		Servers map[string]struct {
			Type    string
			Command string
			Args    []string
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(argv[len(argv)-1]), &cfg); err != nil {
		t.Fatal(err)
	}
	server := cfg.Servers["minion_master"]
	if len(cfg.Servers) != 1 || server.Type != "stdio" || server.Command != "/test/minion" ||
		!slices.Equal(server.Args, []string{"master", "mcp", "/test/master.sock"}) {
		t.Fatalf("unexpected launch-scoped config: %+v", cfg)
	}
}
