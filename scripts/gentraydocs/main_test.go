package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/trayactions"
)

// Acceptance 5: the parity section of docs/guides/tray.md equals the
// generator's output (run "go run ./scripts/gentraydocs" after a change of
// internal/trayactions).
func TestParityDocsFresh(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", guidePath))
	if err != nil {
		t.Fatal(err)
	}
	out, ok := trayactions.Splice(string(b))
	if !ok {
		t.Fatalf("%s lacks the generated-parity markers", guidePath)
	}
	if out != string(b) {
		t.Fatalf("%s is stale: run go run ./scripts/gentraydocs", guidePath)
	}
}

func TestGenerate(t *testing.T) {
	p := filepath.Join(t.TempDir(), "tray.md")
	doc := "# Tray\n\n" + trayactions.BeginMarker + "\nstale\n" + trayactions.EndMarker + "\n"
	if err := os.WriteFile(p, []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	if changed, err := generate(p); err != nil || !changed {
		t.Fatalf("first run = %t, %v", changed, err)
	}
	if changed, err := generate(p); err != nil || changed {
		t.Fatalf("second run = %t, %v", changed, err)
	}
	if err := os.WriteFile(p, []byte("no markers\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := generate(p); err == nil {
		t.Fatal("no error without markers")
	}
}
