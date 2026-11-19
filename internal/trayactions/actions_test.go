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

// Session names are operands, never options: every argv that carries a
// session name has every flag before a literal "--" and the names after it
// (a tmux session may be called "--json").
func TestSessionNamesAfterDoubleDash(t *testing.T) {
	n := 0
	for _, a := range All {
		for i, arg := range a.Argv {
			if arg != "{session}" && arg != "{sessions}" {
				continue
			}
			n++
			if i == 0 || a.Argv[i-1] != "--" || i != len(a.Argv)-1 {
				t.Errorf("%s: %s must be the last argument, right after --: %v", a.ID, arg, a.Argv)
			}
			if slices.Index(a.Argv, "--") != i-1 {
				t.Errorf("%s: more than one -- in %v", a.ID, a.Argv)
			}
		}
	}
	if n != 3 {
		t.Errorf("%d argv carry session names, want share.select, share.add and share.remove", n)
	}
}

// The names of Selected and the revision come from the same share list
// response (stale names under a fresh revision would share again a session
// the owner just removed).
func TestSelectedNamesAndRevisionShareASource(t *testing.T) {
	for _, name := range []string{"sessions", "revision"} {
		if !strings.Contains(Placeholders[name], "share list") {
			t.Errorf("{%s} = %q: it must say it comes from the share list response", name, Placeholders[name])
		}
	}
	if strings.Contains(Placeholders["sessions"], "share.sessions") || !strings.Contains(Placeholders["sessions"], "SAME") {
		t.Errorf("{sessions} = %q", Placeholders["sessions"])
	}
}

// A confirmed session loss never replays what the first run saved: the retry
// of a join is the resume (no token), the retry of a pairing is the restart.
func TestRetryDoesNotReplay(t *testing.T) {
	for id, to := range Retry {
		a, ok := ByID(id)
		b, ok2 := ByID(to)
		if !ok || !ok2 {
			t.Fatalf("retry %s -> %s names an unknown action", id, to)
		}
		if !a.Confirm.SessionLoss() || !b.Confirm.SessionLoss() {
			t.Errorf("%s -> %s: both need the session-loss confirmation", id, to)
		}
		if b.Stdin != StdinNone || len(b.Optional) != 0 {
			t.Errorf("%s needs no stdin and no sharing choice: %+v", to, b)
		}
		for _, arg := range b.Argv {
			if arg == "--share-existing" || arg == "--token-file" {
				t.Errorf("%s replays %s", to, arg)
			}
		}
	}
	for _, id := range []string{"hub.join", "hub.pair"} {
		if Retry[id] == "" {
			t.Errorf("%s has no retry", id)
		}
	}
}

// The block commands, start and stop all ask for JSON and bind the profile.
func TestProfileBoundAndJSON(t *testing.T) {
	for _, id := range []string{"start", "stop", "restart", "block.on", "block.off"} {
		a, _ := ByID(id)
		if !slices.Contains(a.Argv, "--json") || !slices.Contains(a.Argv, "--state-dir") {
			t.Errorf("%s: %v lacks --json or --state-dir", id, a.Argv)
		}
	}
}
