package agent

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// hostileText carries a line break, a carriage return, a cursor movement, a
// right-to-left override and a zero-width space.
const hostileText = "eve\nINJECTED\r\x1b[2A‮​"

func assertOneSafeLine(t *testing.T, what, s string) {
	t.Helper()
	if strings.ContainsAny(s, "\n\r\x1b‮​") {
		t.Errorf("%s keeps a control or format character: %q", what, s)
	}
}

func TestWelcomeTextsAreMadeSafe(t *testing.T) {
	body, _ := json.Marshal(map[string]any{
		"type": "welcome", "worker_id": "w-1", "name": hostileText, "owner": hostileText, "kind": hostileText,
	})
	r, err := decodeReply(body)
	if err != nil {
		t.Fatal(err)
	}
	for what, s := range map[string]string{"owner": r.Welcome.Owner, "name": r.Welcome.Name, "kind": r.Welcome.Kind} {
		assertOneSafeLine(t, what, s)
	}
	for _, bad := range []string{"", "w\n1", "w 1", strings.Repeat("a", 65), "w\x1b1"} {
		body, _ := json.Marshal(map[string]any{"type": "welcome", "worker_id": bad})
		if _, err := decodeReply(body); err == nil {
			t.Errorf("worker id %q was accepted", bad)
		}
	}
}

func TestConsoleBoxLinesAreSafe(t *testing.T) {
	var b bytes.Buffer
	writeConsoleBox(&b, "T"+hostileText, "owner: "+hostileText, "")
	for _, l := range strings.Split(strings.TrimRight(b.String(), "\n"), "\n") {
		assertOneSafeLine(t, "box line", l)
	}
	if strings.Count(b.String(), "INJECTED") != 2 {
		t.Fatalf("box:\n%s", b.String())
	}
	if strings.Contains(b.String(), "\nINJECTED") {
		t.Fatalf("a peer text started a line:\n%s", b.String())
	}
}
