package agentdetect

import (
	"os"
	"strings"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/ops"
)

func screenOf(t *testing.T, lines ...string) ops.Screen {
	t.Helper()
	return ops.Screen{Width: 80, Height: len(lines), Lines: lines}
}

func dialogOf(kind string, choices ...Choice) Result {
	return Result{Kind: kind, State: ops.PaneStateDialog, Choices: choices}
}

var yesNo = []Choice{{Label: "Yes", Keys: []string{"1"}}, {Label: "No", Keys: []string{"2"}}}

func TestDialogDigestIsOnlyForDialogs(t *testing.T) {
	scr := screenOf(t, "Do you want to proceed?", "1. Yes")
	for _, st := range []string{ops.PaneStateStable, ops.PaneStateBusy, ops.PaneStateUnknown} {
		if d := DialogDigest(Result{Kind: "agent:claude", State: st}, scr); d != "" {
			t.Errorf("state %s has the digest %q", st, d)
		}
	}
	d := DialogDigest(dialogOf("agent:claude", yesNo...), scr)
	if !ops.ValidDialogDigest(d) {
		t.Fatalf("digest %q is not 64 lowercase hex characters", d)
	}
	if d != DialogDigest(dialogOf("agent:claude", yesNo...), scr) {
		t.Fatal("the digest is not deterministic")
	}
}

// What the user reads is part of the digest.
func TestDialogDigestChangesWithTheMeaning(t *testing.T) {
	base := screenOf(t, "Bash command", "  rm -rf build/", "Do you want to proceed?", "❯ 1. Yes", "  2. No")
	d := DialogDigest(dialogOf("agent:claude", yesNo...), base)
	for name, other := range map[string]struct {
		r   Result
		scr ops.Screen
	}{
		"another command":   {dialogOf("agent:claude", yesNo...), screenOf(t, "Bash command", "  rm -rf /", "Do you want to proceed?", "❯ 1. Yes", "  2. No")},
		"another agent":     {dialogOf("agent:codex", yesNo...), base},
		"another label":     {dialogOf("agent:claude", Choice{Label: "Yes, always", Keys: []string{"1"}}, yesNo[1]), base},
		"other keys":        {dialogOf("agent:claude", Choice{Label: "Yes", Keys: []string{"2"}}, Choice{Label: "No", Keys: []string{"1"}}), base},
		"an option more":    {dialogOf("agent:claude", append(append([]Choice{}, yesNo...), Choice{Label: "Maybe", Keys: []string{"3"}})...), base},
		"a line below":      {dialogOf("agent:claude", yesNo...), screenOf(t, "Bash command", "  rm -rf build/", "Do you want to proceed?", "❯ 1. Yes", "  2. No", "typed text")},
		"an extra file row": {dialogOf("agent:claude", yesNo...), screenOf(t, "Bash command", "  rm -rf build/ other/", "Do you want to proceed?", "❯ 1. Yes", "  2. No")},
	} {
		if got := DialogDigest(other.r, other.scr); got == d {
			t.Errorf("%s: the digest did not change", name)
		}
	}
}

// Only the selection marker of a row that is provably an option does not change
// the digest: it moves with the arrow keys and the option does not.
func TestDialogDigestIgnoresOnlyTheSelectionMarkerOfAnOption(t *testing.T) {
	base := screenOf(t, "Bash command", "  rm -rf build/", "Do you want to proceed?", "❯ 1. Yes", "  2. No", "esc to cancel")
	d := DialogDigest(dialogOf("agent:claude", yesNo...), base)
	for name, scr := range map[string]ops.Screen{
		"the selection moved": screenOf(t, "Bash command", "  rm -rf build/", "Do you want to proceed?", "  1. Yes", "❯ 2. No", "esc to cancel"),
		"other marker":        screenOf(t, "Bash command", "  rm -rf build/", "Do you want to proceed?", "› 1. Yes", "  2. No", "esc to cancel"),
	} {
		if got := DialogDigest(dialogOf("agent:claude", yesNo...), scr); got != d {
			t.Errorf("%s: the digest changed", name)
		}
	}
	// A marker in front of a row that is not an option of the dialog is content.
	notOption := screenOf(t, "Bash command", "❯ 1. Maybe", "Do you want to proceed?", "❯ 1. Yes", "  2. No", "esc to cancel")
	plain := screenOf(t, "Bash command", "  1. Maybe", "Do you want to proceed?", "❯ 1. Yes", "  2. No", "esc to cancel")
	if DialogDigest(dialogOf("agent:claude", yesNo...), notOption) == DialogDigest(dialogOf("agent:claude", yesNo...), plain) {
		t.Error("a marker in front of a row that is no option was normalised away")
	}
}

// Everything else is content: a conservative binding costs a new review, a loose
// one approves a command that was not read. Each pair below is two different
// screens (the second of each is what a stale approval must not cover).
func TestDialogDigestKeepsEveryMeaningfulCharacterOfTheScreen(t *testing.T) {
	r := dialogOf("agent:claude", yesNo...)
	tail := []string{"Do you want to proceed?", "❯ 1. Yes", "  2. No", "Esc to cancel"}
	with := func(rows ...string) ops.Screen { return screenOf(t, append(rows, tail...)...) }
	for name, pair := range map[string][2]ops.Screen{
		"a command that contains a status-shaped text": {
			with("Bash command", "  printf '1s esc'; echo old"), with("Bash command", "  printf '1s esc'; echo new")},
		"a blank line in a here-document": {
			with("Bash command", "  cat <<'EOF'", "alpha", "", "beta", "EOF"), with("Bash command", "  cat <<'EOF'", "alpha", "beta", "EOF")},
		"a space after a shell continuation": {
			with("Bash command", "  printf '%s' foo\\", "bar"), with("Bash command", "  printf '%s' foo\\ ", "bar")},
		"trailing white space":  {with("Bash command", "  echo a"), with("Bash command", "  echo a ")},
		"leading white space":   {with("Bash command", "  echo a"), with("Bash command", "    echo a")},
		"a blank row":           {with("Bash command", "  echo a"), with("Bash command", "", "  echo a")},
		"a spinner-looking row": {with("Bash command", "  echo a"), with("Bash command", "  echo a", "✻ Thinking… (14s · esc to interrupt)")},
		"a tab":                 {with("Bash command", "  echo a b"), with("Bash command", "  echo a\tb")},
	} {
		if DialogDigest(r, pair[0]) == DialogDigest(r, pair[1]) {
			t.Errorf("%s: two different screens have the same digest", name)
		}
	}
}

// The real dialog of each agent is a dialog and has a digest; two different
// dialogs differ.
func TestDialogDigestOfTheRealFixtures(t *testing.T) {
	seen := map[string]string{}
	for _, agent := range []string{"claude", "codex", "aider"} {
		b, err := os.ReadFile("testdata/" + agent + "_dialog.txt")
		if err != nil {
			t.Fatal(err)
		}
		scr := screenOf(t, strings.Split(strings.TrimRight(string(b), "\n"), "\n")...)
		r := Classify(t.Context(), Input{Command: agent, Screen: scr})
		if r.State != ops.PaneStateDialog {
			t.Fatalf("%s: state = %s", agent, r.State)
		}
		d := DialogDigest(r, scr)
		if !ops.ValidDialogDigest(d) {
			t.Fatalf("%s: digest %q", agent, d)
		}
		if other, dup := seen[d]; dup {
			t.Errorf("%s and %s share a digest", agent, other)
		}
		seen[d] = agent
	}
}

// The digest covers every visible row, not the classifier's window of the last
// 25 non-blank rows: a change far above the question is a different dialog, and
// so is a different indentation of a command row.
func TestDialogDigestCoversTheWholeScreen(t *testing.T) {
	rows := func(command, indent string) ops.Screen {
		lines := []string{"Bash command", indent + command}
		for i := 0; i < 30; i++ {
			lines = append(lines, "  filler")
		}
		return screenOf(t, append(lines, "Do you want to proceed?", "❯ 1. Yes", "  2. No")...)
	}
	r := dialogOf("agent:claude", yesNo...)
	d := DialogDigest(r, rows("rm -rf ./build", "  "))
	if got := DialogDigest(r, rows("rm -rf ./source", "  ")); got == d {
		t.Error("a command changed above the last 25 rows has the same digest")
	}
	if got := DialogDigest(r, rows("rm -rf ./build", "      ")); got == d {
		t.Error("the indentation of a row is content and must change the digest")
	}
	if got := DialogDigest(r, rows("rm -rf ./build", "  ")); got != d {
		t.Error("the same screen has another digest")
	}
	// A row that looks like a status line is left out only in that exact shape.
	if a, b := DialogDigest(r, screenOf(t, "Bash command", "  rm -rf ./a", "1. Yes")), DialogDigest(r, screenOf(t, "Bash command", "  rm -rf ./b", "1. Yes")); a == b {
		t.Error("two commands share a digest")
	}
}
