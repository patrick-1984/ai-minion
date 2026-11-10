package mcp

import (
	"io"
	"log/slog"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/ops"
)

func nopLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func mustTool(t *testing.T, name string) tool {
	t.Helper()
	op, ok := ops.Lookup(name)
	if !ok {
		t.Fatalf("op %s not in the catalog", name)
	}
	tools, err := buildTools([]ops.Op{op}, true)
	if err != nil || len(tools) != 1 {
		t.Fatalf("buildTools: %v", err)
	}
	return tools[0]
}
