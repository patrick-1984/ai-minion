// Command openapi writes the OpenAPI 3.1 description of the Hub API, which
// ops.OpenAPI generates from the operation catalog, to a file. It is the same
// document that the Hub serves at GET /api/v1/ops and that the golden test of
// internal/ops pins.
//
// Usage: go run ./scripts/openapi [file]   (default: docs/api/openapi.json)
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/patrick-1984/ai-minion/internal/ops"
)

func main() {
	out := filepath.Join("docs", "api", "openapi.json")
	if len(os.Args) > 1 {
		out = os.Args[1]
	}
	if err := write(out); err != nil {
		fmt.Fprintln(os.Stderr, "openapi:", err)
		os.Exit(1)
	}
}

func write(out string) error {
	doc, err := ops.OpenAPI()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return err
	}
	return os.WriteFile(out, doc, 0o644)
}
