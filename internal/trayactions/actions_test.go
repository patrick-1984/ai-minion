package trayactions

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// Acceptance 1: the table ids are exactly the required ids, each once.
func TestTableCoversRequired(t *testing.T) {
	seen := map[string]bool{}
	for _, a := range All {
		if seen[a.ID] {
			t.Errorf("id %q is in the table twice", a.ID)
		}
		seen[a.ID] = true
		if !slices.Contains(Required, a.ID) {
			t.Errorf("id %q is in the table but not in required.go", a.ID)
		}
	}
	req := map[string]bool{}
	for _, id := range Required {
		if req[id] {
			t.Errorf("id %q is in required.go twice", id)
		}
		req[id] = true
		if !seen[id] {
			t.Errorf("required id %q is not in the table", id)
		}
	}
}

// Acceptance 3: every id has an effect of the known kinds and an
// observation, and every other field is set and valid.
func TestEveryIDHasEffect(t *testing.T) {
	for _, a := range All {
		if a.Effect == "" || !slices.Contains(Effects, a.Effect) {
			t.Errorf("%s: effect %q", a.ID, a.Effect)
		}
		if strings.TrimSpace(a.Observe) == "" {
			t.Errorf("%s: no Observe", a.ID)
		}
		if a.Menu == "" || len(a.Argv) == 0 {
			t.Errorf("%s: menu %q, argv %v", a.ID, a.Menu, a.Argv)
		}
		if a.Program != Minion && a.Program != MinionTray {
			t.Errorf("%s: program %q", a.ID, a.Program)
		}
		if a.Stdin != StdinNone && a.Stdin != StdinToken {
			t.Errorf("%s: stdin %q", a.ID, a.Stdin)
		}
		if a.Confirm != ConfirmNone && a.Confirm != ConfirmSessionLoss && a.Confirm != ConfirmLeave {
			t.Errorf("%s: confirm %q", a.ID, a.Confirm)
		}
		if a.Elevate != ElevateNone && ElevatedArgv(a.Elevate) == nil {
			t.Errorf("%s: elevate %q", a.ID, a.Elevate)
		}
		// Every placeholder is known, also in the optional groups.
		for _, argv := range append([][]string{a.Argv}, a.Optional...) {
			values := map[string][]string{}
			for name := range Placeholders {
				values[name] = []string{"x"}
			}
			if _, err := Resolve(argv, values); err != nil {
				t.Errorf("%s: %v", a.ID, err)
			}
		}
	}
}

// The elevated argv are a closed set of two (contract E1/E2), only start may
// use E1 and only stop E2, and a secret never travels in argv.
func TestElevationAndSecrets(t *testing.T) {
	if got := strings.Join(ElevatedArgv(ElevateE1), " "); got != "worker start --json" {
		t.Errorf("E1 = %q", got)
	}
	if got := strings.Join(ElevatedArgv(ElevateE2), " "); got != "worker stop --scm-only --accept-unknown-loss --json" {
		t.Errorf("E2 = %q", got)
	}
	if ElevatedArgv(ElevateNone) != nil || ElevatedArgv("E3") != nil {
		t.Error("an argv outside E1/E2")
	}
	for _, a := range All {
		switch a.Elevate {
		case ElevateE1:
			if a.ID != "start" {
				t.Errorf("%s uses E1", a.ID)
			}
		case ElevateE2:
			if a.ID != "stop" {
				t.Errorf("%s uses E2", a.ID)
			}
		}
		for _, arg := range a.Argv {
			if strings.Contains(arg, "token") && arg != "--token-file" || arg == "{code}" {
				t.Errorf("%s: a secret in argv: %v", a.ID, a.Argv)
			}
		}
	}
	join, _ := ByID("hub.join")
	if join.Stdin != StdinToken || !slices.Equal(join.Argv[:4], []string{"worker", "join", "--token-file", "-"}) {
		t.Errorf("hub.join = %+v", join)
	}
}

// Acceptance 8: the ids of the new CLI equivalents map onto their commands.
func TestRequiredCommandMapping(t *testing.T) {
	want := map[string]string{
		"copy.hubkey":     "worker copy hub-key",
		"copy.workerkey":  "worker copy worker-key",
		"about.docs":      "docs tray",
		"setup":           "worker up",
		"hub.cancelpair":  "worker pair cancel",
		"hub.join.resume": "worker join --resume",
	}
	for id, prefix := range want {
		a, ok := ByID(id)
		if !ok {
			t.Errorf("%s missing", id)
			continue
		}
		if a.Program != Minion || !strings.HasPrefix(strings.Join(a.Argv, " "), prefix) {
			t.Errorf("%s = %s %v, want minion %s", id, a.Program, a.Argv, prefix)
		}
	}
	if a, _ := ByID("setup"); a.Effect != EffectTerminal {
		t.Errorf("setup effect = %q", a.Effect)
	}
}

func TestResolve(t *testing.T) {
	got, err := Resolve([]string{"share", "set", "select", "{sessions}", "--state-dir", "{state_dir}"},
		map[string][]string{"sessions": nil, "state_dir": {"/s d"}})
	if err != nil || strings.Join(got, "|") != "share|set|select|--state-dir|/s d" {
		t.Fatalf("Resolve = %q, %v", got, err)
	}
	for _, bad := range [][]string{{"{nope}"}, {"{state_dir}"}, {"x{state_dir}"}} {
		if _, err := Resolve(bad, map[string][]string{"nope": {"1"}}); err == nil {
			t.Errorf("Resolve(%v) passed", bad)
		}
	}
	if _, err := Resolve([]string{"{n}"}, map[string][]string{"n": {"1", "2"}}); err == nil {
		t.Error("two values for {n}")
	}
}

// Acceptance 7: the package imports only the standard library, also in its
// tests.
func TestImportsOnlyStandardLibrary(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("glob: %v %v", files, err)
	}
	fset := token.NewFileSet()
	for _, f := range files {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		af, err := parser.ParseFile(fset, f, src, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range af.Imports {
			p, _ := strconv.Unquote(imp.Path.Value)
			if first, _, _ := strings.Cut(p, "/"); strings.Contains(first, ".") || p == "C" {
				t.Errorf("%s imports %s (standard library only)", f, p)
			}
		}
	}
}

func TestSplice(t *testing.T) {
	doc := "# T\n\n" + BeginMarker + "\nold\n" + EndMarker + "\ntail\n"
	out, ok := Splice(doc)
	if !ok || !strings.Contains(out, ParityMarkdown()) || strings.Contains(out, "\nold\n") || !strings.HasSuffix(out, EndMarker+"\ntail\n") {
		t.Fatalf("Splice = %q, %t", out, ok)
	}
	if again, _ := Splice(out); again != out {
		t.Error("Splice is not idempotent")
	}
	if _, ok := Splice("no markers"); ok {
		t.Error("Splice without markers")
	}
	for _, a := range All {
		if !strings.Contains(ParityMarkdown(), "`"+a.ID+"`") {
			t.Errorf("%s is not in the parity table", a.ID)
		}
	}
}
