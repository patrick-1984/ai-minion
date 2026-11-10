package ops

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// The tests decode the wire form, so they state the contract of the fields
// (EPICS A-22) and not the Go types behind them.

func wireArgs(t *testing.T, v string, into any) error {
	t.Helper()
	if err := json.Unmarshal([]byte(v), into); err != nil {
		t.Fatal(err)
	}
	return into.(Validator).Validate()
}

const wireHead = `"request_id":"0191a2b3-c4d5-7e6f-8a9b-0c1d2e3f4a5b","target":{"worker_id":"","session_name":"work"}`

func TestSendTextValidateRefusesInputControls(t *testing.T) {
	for _, text := range []string{`a\rb`, `a\r\nb`, `a\u001b[201~b`, `a\u0000b`, `a\u0003b`, `a\u007fb`, `a\u0085b`, `a\u009bb`} {
		err := wireArgs(t, `{`+wireHead+`,"text":"`+text+`"}`, &SendTextArgs{})
		if !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("%s: err = %v, want invalid_argument", text, err)
		}
	}
	for _, text := range []string{`a\tb`, `one\ntwo\n`, `plain`, "\u65e5\u672c\u8a9e \u2713 \U0001F642"} {
		if err := wireArgs(t, `{`+wireHead+`,"text":"`+text+`"}`, &SendTextArgs{}); err != nil {
			t.Errorf("%s: err = %v, want accepted", text, err)
		}
	}
}

func TestSendTextSubmitFieldIsOptionalAndDefaultsToFalse(t *testing.T) {
	var a SendTextArgs
	if err := wireArgs(t, `{`+wireHead+`,"text":"x"}`, &a); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(a)
	if strings.Contains(string(b), `"submit":true`) {
		t.Fatalf("submit defaults to true: %s", b)
	}
	if err := wireArgs(t, `{`+wireHead+`,"text":"x","submit":true}`, &a); err != nil {
		t.Fatal(err)
	}
	b, _ = json.Marshal(a)
	if !strings.Contains(string(b), `"submit":true`) {
		t.Fatalf("submit was lost: %s", b)
	}
}

const goodDigest = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestAnswerValidateNeedsTheReviewedBinding(t *testing.T) {
	good := `"expect":{"pane_id":"%3","pid":4242,"command":"claude","session_created":1794308400,"dialog_digest":"` + goodDigest + `"}`
	if err := wireArgs(t, `{`+wireHead+`,"choice":"1",`+good+`}`, &AnswerArgs{}); err != nil {
		t.Fatalf("a complete binding: %v", err)
	}
	for name, body := range map[string]string{
		"missing":       `"choice":"1"`,
		"empty":         `"choice":"1","expect":{}`,
		"no pane":       `"choice":"1","expect":{"pid":4242,"command":"claude","session_created":1794308400,"dialog_digest":"` + goodDigest + `"}`,
		"no pid":        `"choice":"1","expect":{"pane_id":"%3","command":"claude","session_created":1794308400,"dialog_digest":"` + goodDigest + `"}`,
		"no command":    `"choice":"1","expect":{"pane_id":"%3","pid":4242,"session_created":1794308400,"dialog_digest":"` + goodDigest + `"}`,
		"no session":    `"choice":"1","expect":{"pane_id":"%3","pid":4242,"command":"claude","dialog_digest":"` + goodDigest + `"}`,
		"no digest":     `"choice":"1","expect":{"pane_id":"%3","pid":4242,"command":"claude","session_created":1794308400}`,
		"short digest":  `"choice":"1","expect":{"pane_id":"%3","pid":4242,"command":"claude","session_created":1794308400,"dialog_digest":"abcd"}`,
		"upper digest":  `"choice":"1","expect":{"pane_id":"%3","pid":4242,"command":"claude","session_created":1794308400,"dialog_digest":"` + strings.ToUpper(goodDigest) + `"}`,
		"negative pid":  `"choice":"1","expect":{"pane_id":"%3","pid":-1,"command":"claude","session_created":1794308400,"dialog_digest":"` + goodDigest + `"}`,
		"control bytes": `"choice":"1","expect":{"pane_id":"%3","pid":4242,"command":"cl\u001baude","session_created":1794308400,"dialog_digest":"` + goodDigest + `"}`,
	} {
		err := wireArgs(t, `{`+wireHead+`,`+body+`}`, &AnswerArgs{})
		if !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("%s: err = %v, want invalid_argument", name, err)
		}
	}
}
