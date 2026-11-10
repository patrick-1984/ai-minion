package cli

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/ops"
)

const reviewedJSON = `{"pane_id":"%3","pid":4242,"command":"claude","session_created":1700000000,"dialog_digest":"` +
	"abababababababababababababababababababababababababababababababab" + `"}`

// TestAnswerNeedsTheReviewedDialog checks the CLI never makes up the review:
// without --expect nothing is sent (the Worker would refuse it anyway), and with
// it the structured document reaches the op as given.
func TestAnswerNeedsTheReviewedDialog(t *testing.T) {
	b := &fakeOpsBackend{call: answerWith(writeResult(ops.OutcomeDelivered, "", false), nil)}
	code, _, errs := runWith(t, testDeps(b), "", "answer", "--target", "s", "--choice", "1")
	if code != ExitUsage || !strings.Contains(errs, "expect") {
		t.Fatalf("answer without --expect: exit %d, stderr %q", code, errs)
	}
	if len(b.calls) != 0 {
		t.Fatalf("a request went out without the reviewed dialog: %+v", b.calls)
	}

	for name, doc := range map[string]string{
		"not json":       `{"pane_id":`,
		"missing digest": `{"pane_id":"%3","pid":4242,"command":"claude","session_created":1700000000}`,
		"bad digest":     `{"pane_id":"%3","pid":4242,"command":"claude","session_created":1700000000,"dialog_digest":"nothex"}`,
	} {
		code, _, errs := runWith(t, testDeps(b), "", "answer", "--target", "s", "--choice", "1", "--expect", doc)
		if code != ExitUsage {
			t.Errorf("%s: exit %d, stderr %q, want a usage error", name, code, errs)
		}
	}
	if len(b.calls) != 0 {
		t.Fatalf("an invalid --expect reached the Worker: %+v", b.calls)
	}

	code, out, errs := runWith(t, testDeps(b), "", "answer", "--target", "s", "--choice", "1", "--expect", reviewedJSON)
	if code != ExitOK {
		t.Fatalf("answer: exit %d, stdout %q, stderr %q", code, out, errs)
	}
	args := decodeArgs(t, b.calls[0].Args)
	exp, _ := args["expect"].(map[string]any)
	if exp["pane_id"] != "%3" || exp["pid"] != float64(4242) || exp["command"] != "claude" ||
		exp["session_created"] != float64(1700000000) || exp["dialog_digest"] == "" || args["choice"] != "1" {
		t.Errorf("the request carries %s", b.calls[0].Args)
	}
	if _, forced := args["force"]; forced {
		t.Error("force was set without the flag")
	}
}

func TestAnswerReadsTheReviewedDialogFromStandardInput(t *testing.T) {
	b := &fakeOpsBackend{call: answerWith(writeResult(ops.OutcomeDelivered, "", false), nil)}
	code, _, errs := runWith(t, testDeps(b), reviewedJSON+"\n", "answer", "--target", "s", "--choice", "yes", "--expect", "-")
	if code != ExitOK {
		t.Fatalf("exit %d: %s", code, errs)
	}
	if exp, _ := decodeArgs(t, b.calls[0].Args)["expect"].(map[string]any); exp["dialog_digest"] == nil {
		t.Errorf("request %s", b.calls[0].Args)
	}
}

// TestAnswerForceDoesNotCarryTheBinding: a refusal for a changed dialog is
// failed_not_sent, and --force is a separate flag that the CLI never adds.
func TestAnswerRefusedForAChangedDialogIsNotRetriedWithForce(t *testing.T) {
	b := &fakeOpsBackend{call: answerWith(writeResult(ops.OutcomeFailedNotSent, "dialog_changed", false), nil)}
	code, out, _ := runWith(t, testDeps(b), "", "answer", "--target", "s", "--choice", "1", "--expect", reviewedJSON)
	if code != ExitFailedNotSent || !strings.Contains(out, "dialog_changed") {
		t.Fatalf("exit %d, stdout %q", code, out)
	}
	if len(b.calls) != 1 {
		t.Fatalf("the CLI sent %d requests, want exactly one", len(b.calls))
	}
}

func TestSendTextSubmitIsOptIn(t *testing.T) {
	b := &fakeOpsBackend{call: answerWith(writeResult(ops.OutcomeDelivered, "", false), nil)}
	if code, _, errs := runWith(t, testDeps(b), "", "send_text", "--target", "s", "--text", "hello"); code != ExitOK {
		t.Fatalf("exit %d: %s", code, errs)
	}
	if _, has := decodeArgs(t, b.calls[0].Args)["submit"]; has {
		t.Errorf("the default send_text carries submit: %s", b.calls[0].Args)
	}
	if code, _, errs := runWith(t, testDeps(b), "", "send_text", "--target", "s", "--text", "hello", "--submit"); code != ExitOK {
		t.Fatalf("exit %d: %s", code, errs)
	}
	if got := decodeArgs(t, b.calls[1].Args)["submit"]; got != true {
		t.Errorf("--submit did not reach the request: %s", b.calls[1].Args)
	}
	// A multi-line text from standard input stays as it is (verbatim, no Enter).
	if code, _, errs := runWith(t, testDeps(b), "a\nb\n", "send_text", "--target", "s", "--text", "-"); code != ExitOK {
		t.Fatalf("exit %d: %s", code, errs)
	}
	args := decodeArgs(t, b.calls[2].Args)
	if args["text"] != "a\nb\n" {
		t.Errorf("text = %q", args["text"])
	}
	if _, has := args["submit"]; has {
		t.Error("stdin text carries submit without the flag")
	}
	// A control character that a paste would turn into input is refused before sending.
	if code, _, _ := runWith(t, testDeps(b), "a\rb", "send_text", "--target", "s", "--text", "-"); code != ExitUsage {
		t.Errorf("a CR in the text: exit %d, want a usage error", code)
	}
	if len(b.calls) != 3 {
		t.Errorf("the CR text reached the backend: %d calls", len(b.calls))
	}
}

// TestInternalMessageIDIsNotAProducerArgument: pipe_send's message_id is the
// Hub's and neither a flag nor an accepted member of the producer schemas.
func TestInternalMessageIDIsNotAProducerArgument(t *testing.T) {
	b := &fakeOpsBackend{call: answerWith(writeResult(ops.OutcomeDelivered, "", false), nil)}
	if code, _, _ := runWith(t, testDeps(b), "", "pipe_send", "--target", "s", "--text", "x", "--message-id", "5"); code != ExitUsage {
		t.Errorf("--message-id: exit %d, want a usage error (no such flag)", code)
	}
	if code, _, errs := runWith(t, testDeps(b), "", "pipe_send", "--target", "s", "--text", "x"); code == ExitUsage {
		t.Fatalf("pipe_send: %s", errs)
	}
	var m map[string]json.RawMessage
	_ = json.Unmarshal(b.calls[len(b.calls)-1].Args, &m)
	if _, has := m["message_id"]; has {
		t.Errorf("pipe_send carries message_id: %s", b.calls[len(b.calls)-1].Args)
	}
}

// TestPipeOpsLeaveTheWorkerPartOfTheTargetToTheLocalPipeService: the pipe ops go
// to the local control socket, and the Worker behind it knows whether it is
// enrolled with a Hub (it forwards the structured target to the Hub) or runs
// alone (it refuses a target that names a Worker). The command cannot tell and
// does not guess, so it sends target.worker_id as given. Every other op still
// refuses a foreign Worker here, before the socket is touched: it would act on a
// same-named local session.
func TestPipeOpsLeaveTheWorkerPartOfTheTargetToTheLocalPipeService(t *testing.T) {
	foreign := "01890a5d-ac96-774b-bcce-b302099a8057"
	for _, tc := range []struct {
		op   string
		args []string
	}{
		{"pipe_send", []string{"--text", "hello"}},
		{"pipe_read", nil},
		{"pipe_wait", []string{"--timeout-ms", "10"}},
	} {
		b := &fakeOpsBackend{
			call: answerWith(writeResult(ops.OutcomeDelivered, "", false), nil),
			stream: func(string, json.RawMessage, func(json.RawMessage) error) (json.RawMessage, error) {
				return json.RawMessage(`{}`), nil
			},
		}
		args := append([]string{tc.op, "--target", foreign + ":minion-1"}, tc.args...)
		code, _, errs := runWith(t, testDeps(b), "", args...)
		if code == ExitUsage {
			t.Fatalf("%s: the target with a Worker id was refused by the command: %s", tc.op, errs)
		}
		if b.count() != 1 {
			t.Fatalf("%s: %d calls reached the local backend (exit %d, %s), want 1", tc.op, b.count(), code, errs)
		}
		var a struct {
			Target ops.Target `json:"target"`
		}
		if err := json.Unmarshal(b.last(t).Args, &a); err != nil || a.Target.WorkerID != foreign || a.Target.SessionName != "minion-1" {
			t.Fatalf("%s: the backend got %s, want the structured target with worker_id", tc.op, b.last(t).Args)
		}
	}
	for _, op := range [][]string{
		{"send_text", "--text", "x"},
		{"send_keys", "--keys", "Enter"},
		{"session_snapshot"},
	} {
		b := &fakeOpsBackend{call: answerWith(writeResult(ops.OutcomeDelivered, "", false), nil)}
		code, _, errs := runWith(t, testDeps(b), "", append([]string{op[0], "--target", foreign + ":work"}, op[1:]...)...)
		if code != ExitUsage || b.count() != 0 {
			t.Errorf("%s with a foreign Worker: exit %d (%s), %d calls: the command must refuse it", op[0], code, errs, b.count())
		}
	}
}
