package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/trayactions"
)

// sampleValues gives every placeholder a plausible value.
func sampleValues(t *testing.T) map[string][]string {
	return map[string][]string{
		"state_dir": {filepath.Join(t.TempDir(), "state dir")},
		"n":         {"2"},
		"session":   {"main"},
		"sessions":  {"main", "build"},
		"revision":  {"7"},
		"share":     {"none"},
		"url":       {"https://hub.example:8443"},
		"hub_fp":    {"apple-river-stone-lamp-oak-mint"},
		"attempt":   {"a1b2"},
		"fp":        {"ABCDEFGHIJKLMNOPQRSTUVWXYZABCDEFGHIJKLMNOPQRSTUVWXYZ"},
		"epoch":     {"3"},
	}
}

// parseArgv resolves argv against a fresh "minion" command tree: it must name
// a runnable command, every flag must exist and take its value, and the
// positional arguments must be accepted. It returns the command's path and
// whether that command has a flag.
func parseArgv(argv []string) (path string, hasFlag func(string) bool, err error) {
	root := NewRoot()
	cmd, rest, err := root.Find(argv)
	if err != nil {
		return "", nil, err
	}
	if cmd == root || !cmd.Runnable() {
		return "", nil, fmt.Errorf("%q names no runnable command", argv)
	}
	if err := cmd.ParseFlags(rest); err != nil {
		return "", nil, err
	}
	if err := cmd.ValidateArgs(cmd.Flags().Args()); err != nil {
		return "", nil, fmt.Errorf("arguments: %w", err)
	}
	return cmd.CommandPath(), func(name string) bool { return cmd.Flags().Lookup(name) != nil }, nil
}

// Acceptance 2: every argv of Program "minion", with its placeholders
// substituted, parses against the real command tree: the base argv, each
// optional group, the session-loss retries and the elevated argv E1/E2.
// Every command that takes --state-dir gets the tray's profile (C7) and every
// command with --json is run with it (except "worker up" in a terminal).
func TestMinionArgvResolve(t *testing.T) {
	values := sampleValues(t)
	checked := 0
	for _, a := range trayactions.All {
		if a.Program != trayactions.Minion {
			continue
		}
		variants := [][]string{a.Argv}
		for _, o := range a.Optional {
			variants = append(variants, append(slices.Clone(a.Argv), o...))
		}
		if a.Confirm.SessionLoss() {
			variants = append(variants,
				append(slices.Clone(a.Argv), trayactions.AcceptLossArgv...),
				append(slices.Clone(a.Argv), trayactions.AcceptUnknownLossArgv...))
		}
		for i, v := range variants {
			argv, err := trayactions.Resolve(v, values)
			if err != nil {
				t.Errorf("%s: %v", a.ID, err)
				continue
			}
			path, has, err := parseArgv(argv)
			if err != nil {
				t.Errorf("%s: %q: %v", a.ID, argv, err)
				continue
			}
			checked++
			if i > 0 {
				continue
			}
			if has("state-dir") && !containsPair(a.Argv, "--state-dir", "{state_dir}") {
				t.Errorf("%s: %s takes --state-dir, but the argv does not pass {state_dir} (C7)", a.ID, path)
			}
			if has("json") && a.Effect != trayactions.EffectTerminal && !slices.Contains(a.Argv, "--json") {
				t.Errorf("%s: %s takes --json, but the argv does not pass it", a.ID, path)
			}
		}
	}
	for _, e := range []trayactions.Elevate{trayactions.ElevateE1, trayactions.ElevateE2} {
		if _, _, err := parseArgv(trayactions.ElevatedArgv(e)); err != nil {
			t.Errorf("%s: %v", e, err)
		}
		checked++
	}
	if checked < 40 {
		t.Fatalf("only %d argv checked", checked)
	}
}

func containsPair(argv []string, a, b string) bool {
	for i := 0; i+1 < len(argv); i++ {
		if argv[i] == a && argv[i+1] == b {
			return true
		}
	}
	return false
}

// A broken argv fails the parse (the test above can fail).
func TestMinionArgvResolveCatchesBadArgv(t *testing.T) {
	for _, argv := range [][]string{
		{"worker", "pause", "--for", "30m", "--no-such-flag"},
		{"worker", "no-such-command"},
		{"worker", "copy"},
	} {
		if _, _, err := parseArgv(argv); err == nil {
			t.Errorf("%q passed", argv)
		}
	}
}

// Acceptance 4: "minion tray-actions --json" prints the compiled table.
func TestTrayActionsJSON(t *testing.T) {
	var out, errOut bytes.Buffer
	code := executeRoot(context.Background(), NewRoot(), []string{"tray-actions", "--json"}, &out, &errOut)
	if code != 0 || strings.Count(out.String(), "\n") != 1 {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, out.String(), errOut.String())
	}
	var got struct {
		OK           bool                             `json:"ok"`
		Actions      []trayactions.Action             `json:"actions"`
		Required     []string                         `json:"required"`
		Elevated     map[trayactions.Elevate][]string `json:"elevated"`
		Retry        map[string][]string              `json:"session_loss_retry"`
		Placeholders map[string]string                `json:"placeholders"`
	}
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !got.OK || !reflect.DeepEqual(got.Actions, trayactions.All) || !slices.Equal(got.Required, trayactions.Required) {
		t.Fatalf("tray-actions --json differs from the table: %s", out.String())
	}
	if !slices.Equal(got.Elevated["E1"], trayactions.ElevatedArgv(trayactions.ElevateE1)) ||
		!slices.Equal(got.Elevated["E2"], trayactions.ElevatedArgv(trayactions.ElevateE2)) || len(got.Elevated) != 2 {
		t.Fatalf("elevated = %v", got.Elevated)
	}
	if !slices.Equal(got.Retry["sessions_would_end"], trayactions.AcceptLossArgv) || !reflect.DeepEqual(got.Placeholders, trayactions.Placeholders) {
		t.Fatalf("retry %v, placeholders %v", got.Retry, got.Placeholders)
	}
	// Without --json: the parity table; and the command is hidden.
	out.Reset()
	if code := executeRoot(context.Background(), NewRoot(), []string{"tray-actions"}, &out, &errOut); code != 0 || out.String() != trayactions.ParityMarkdown() {
		t.Fatalf("text = %d %q", code, out.String())
	}
	cmd, _, _ := NewRoot().Find([]string{"tray-actions"})
	if !cmd.Hidden {
		t.Error("tray-actions is not hidden")
	}
}
