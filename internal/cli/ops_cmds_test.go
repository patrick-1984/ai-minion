package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/patrick-1984/ai-minion/internal/client"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/worker/term"
)

var update = flag.Bool("update", false, "rewrite the golden files")

// fakeOpsBackend records calls and answers from functions.
type fakeOpsBackend struct {
	mu     sync.Mutex
	calls  []fakeCall
	call   func(op string, args json.RawMessage) (json.RawMessage, error)
	stream func(op string, args json.RawMessage, on func(json.RawMessage) error) (json.RawMessage, error)
}

type fakeCall struct {
	Op   string
	Args json.RawMessage
}

func (f *fakeOpsBackend) record(op string, args any) json.RawMessage {
	raw, _ := args.(json.RawMessage)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fakeCall{op, raw})
	return raw
}

func (f *fakeOpsBackend) Call(_ context.Context, op string, args any) (json.RawMessage, error) {
	raw := f.record(op, args)
	return f.call(op, raw)
}

func (f *fakeOpsBackend) Stream(_ context.Context, op string, args any, on func(json.RawMessage) error) (json.RawMessage, error) {
	raw := f.record(op, args)
	return f.stream(op, raw, on)
}

func (f *fakeOpsBackend) last(t *testing.T) fakeCall {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		t.Fatal("the backend was not called")
	}
	return f.calls[len(f.calls)-1]
}

func (f *fakeOpsBackend) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

const testRequestID = "01890a5d-ac96-774b-bcce-b302099a8057"

func testDeps(b *fakeOpsBackend) opsDeps {
	return opsDeps{
		local:   func() client.Backend { return b },
		session: func(context.Context) (string, error) { return "", nil },
		confirm: func(context.Context, string, client.Backend) error { return nil },
		newID:   func() string { return testRequestID },
	}
}

// runWith runs args against a fresh command tree that uses deps.
func runWith(t *testing.T, deps opsDeps, stdin string, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out bytes.Buffer
	code, stderr = runWithIO(context.Background(), deps, strings.NewReader(stdin), &out, args...)
	return code, out.String(), stderr
}

// runWithIO is runWith with the context, the stdin reader and the stdout
// writer chosen by the test.
func runWithIO(ctx context.Context, deps opsDeps, stdin io.Reader, stdout io.Writer, args ...string) (code int, stderr string) {
	root := &cobra.Command{Use: "minion", SilenceUsage: true, SilenceErrors: true}
	addOpCommands(root, deps, ops.All())
	root.SetIn(stdin)
	var errb bytes.Buffer
	code = execute(ctx, root, args, stdout, &errb)
	return code, errb.String()
}

func writeResult(o ops.Outcome, reason string, forceable bool) json.RawMessage {
	raw, _ := json.Marshal(ops.WriteResult{Outcome: o, Reason: reason, Forceable: forceable,
		Pane: &ops.PaneInfo{Session: "minion-3", ID: "%3", Kind: "agent:claude", State: "busy"}})
	return raw
}

func answerWith(raw json.RawMessage, err error) func(string, json.RawMessage) (json.RawMessage, error) {
	return func(string, json.RawMessage) (json.RawMessage, error) { return raw, err }
}

func decodeArgs(t *testing.T, raw json.RawMessage) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("args %s: %v", raw, err)
	}
	return m
}

// --- catalog derivation ------------------------------------------------------

func TestEveryCatalogOpHasACommandInMinionAndMn(t *testing.T) {
	list := ops.All()
	if len(list) < 11 {
		t.Fatalf("catalog has %d ops", len(list))
	}
	for _, name := range []string{"minion", "mn"} {
		root := NewRoot()
		root.Use = name
		for _, op := range list {
			cmd, _, err := root.Find([]string{op.CLICommand()})
			if err != nil || cmd == root || cmd.Name() != op.CLICommand() {
				t.Errorf("%s: no command for op %s (%v)", name, op.Name, err)
				continue
			}
			if cmd.Short != op.Summary {
				t.Errorf("%s %s: short = %q, want the catalog summary", name, op.Name, cmd.Short)
			}
			if cmd.Flags().Lookup(flagJSON) == nil {
				t.Errorf("%s %s: no --json flag", name, op.Name)
			}
		}
	}
	for _, want := range []string{"sessions_list", "send_text", "pipe_send", "pipe_wait"} {
		if cmd, _, _ := NewRoot().Find([]string{want}); cmd == nil || cmd.Name() != want {
			t.Errorf("no %s command", want)
		}
	}
}

func TestEveryCatalogOpDerivesDistinctFlags(t *testing.T) {
	for _, op := range ops.All() {
		fields, err := deriveFields(op.Args)
		if err != nil {
			t.Errorf("%s: %v", op.Name, err)
			continue
		}
		cmd, err := newOpCmd(op, testDeps(&fakeOpsBackend{}))
		if err != nil {
			t.Errorf("%s: %v", op.Name, err)
			continue
		}
		for _, f := range fields {
			if ops.HiddenMember(op.Name, f.JSON) {
				if cmd.Flags().Lookup(f.Flag) != nil {
					t.Errorf("%s: the internal member %s has a flag --%s", op.Name, f.JSON, f.Flag)
				}
				continue
			}
			if cmd.Flags().Lookup(f.Flag) == nil {
				t.Errorf("%s: no flag --%s for member %s", op.Name, f.Flag, f.JSON)
			}
		}
		if op.Write {
			for _, name := range []string{"force", "request-id"} {
				if cmd.Flags().Lookup(name) == nil {
					t.Errorf("write op %s has no --%s", op.Name, name)
				}
			}
		}
	}
}

type allKinds struct {
	ops.WriteArgs
	Text      string         `json:"text"`
	Mode      namedString    `json:"mode,omitempty"`
	Enabled   *bool          `json:"enabled,omitempty"`
	Count     int            `json:"count"`
	Small     int8           `json:"small"`
	Size      uint           `json:"size"`
	Ratio     float64        `json:"ratio"`
	Keys      []string       `json:"keys"`
	From      *ops.Timestamp `json:"from,omitempty"`
	Extra     map[string]int `json:"extra,omitempty"`
	Nested    nestedArg      `json:"nested"`
	Target    ops.Target     `json:"target"`
	Skipped   string         `json:"-"`
	hidden    string
	LongName  string `json:"after_turn_id"`
	NoTagName string
}

type namedString string

type nestedArg struct {
	A string `json:"a"`
}

var _ = allKinds{hidden: ""}

func TestFlagDerivationForEveryArgType(t *testing.T) {
	fields, err := deriveFields(reflect.TypeFor[allKinds]())
	if err != nil {
		t.Fatal(err)
	}
	want := []struct {
		json, flag string
		kind       argKind
	}{
		{"request_id", "request-id", kindString},
		{"force", "force", kindBool},
		{"text", "text", kindString},
		{"mode", "mode", kindString},
		{"enabled", "enabled", kindBool},
		{"count", "count", kindInt},
		{"small", "small", kindInt},
		{"size", "size", kindUint},
		{"ratio", "ratio", kindFloat},
		{"keys", "keys", kindStringList},
		{"from", "from", kindStringLike},
		{"extra", "extra", kindJSON},
		{"nested", "nested", kindJSON},
		{"target", "target", kindTarget},
		{"after_turn_id", "after-turn-id", kindString},
		{"NoTagName", "notagname", kindString},
	}
	if len(fields) != len(want) {
		t.Fatalf("fields = %+v", fields)
	}
	for i, w := range want {
		if f := fields[i]; f.JSON != w.json || f.Flag != w.flag || f.Kind != w.kind {
			t.Errorf("field %d = %+v, want %+v", i, f, w)
		}
	}

	cmd := &cobra.Command{Use: "x"}
	bindFlags(cmd.Flags(), "", fields)
	args := []string{
		"--text", "hi", "--mode", "fast", "--enabled=false", "--count", "-5", "--small", "7", "--size", "9",
		"--ratio", "1.5", "--keys", "Enter", "--keys", "a,b", "--from", "2026-11-10T10:00:00.000Z",
		"--extra", `{"a":1}`, "--nested", `{"a":"x"}`, "--after-turn-id", "t", "--force",
	}
	if err := cmd.ParseFlags(args); err != nil {
		t.Fatal(err)
	}
	got, err := collectArgs(context.Background(), cmd, fields, &stdinOnce{r: strings.NewReader("")})
	if err != nil {
		t.Fatal(err)
	}
	wantJSON := map[string]string{
		"text": `"hi"`, "mode": `"fast"`, "enabled": `false`, "count": `-5`, "small": `7`, "size": `9`,
		"ratio": `1.5`, "keys": `["Enter","a,b"]`, "from": `"2026-11-10T10:00:00.000Z"`,
		"extra": `{"a":1}`, "nested": `{"a":"x"}`, "after_turn_id": `"t"`, "force": `true`,
	}
	if len(got) != len(wantJSON) {
		t.Fatalf("members = %v", got)
	}
	for k, v := range wantJSON {
		if string(got[k]) != v {
			t.Errorf("member %s = %s, want %s", k, got[k], v)
		}
	}
	// Only the flags the user passes become members: no request_id, no target.
	if _, ok := got["request_id"]; ok {
		t.Error("request_id was set without the flag")
	}
	// The members decode into the declared type.
	raw, _ := json.Marshal(got)
	var back allKinds
	if err := json.Unmarshal(raw, &back); err != nil || back.From == nil || back.Count != -5 || !back.Force {
		t.Fatalf("decoded = %+v, %v", back, err)
	}
}

func TestFlagDerivationRejectsCollisionsAndReservedNames(t *testing.T) {
	type clash struct {
		A string `json:"after_id"`
		B string `json:"after-id"`
	}
	if _, err := deriveFields(reflect.TypeFor[clash]()); err == nil {
		t.Error("two members with one flag name were accepted")
	}
	type reserved struct {
		J bool `json:"json"`
	}
	if _, err := deriveFields(reflect.TypeFor[reserved]()); err == nil {
		t.Error("a member named json was accepted")
	}
}

func TestBadFlagValuesAreUsageErrors(t *testing.T) {
	b := &fakeOpsBackend{call: answerWith(nil, nil)}
	for _, args := range [][]string{
		{"session_capture", "--target", "w", "--lines", "abc"},
		{"session_capture", "--target", "w", "--lines", "-1"},
		{"session_capture", "--target", "w", "--lines", "999999"},
		{"session_capture", "--target", "a:b:c:d"},
		{"session_capture", "--target", "x:w"},
		{"session_capture", "--target", "w", "--nonsense"},
		{"send_text", "--target", "w"},
		{"send_text", "--target", "w", "--text", "x", "--request-id", "nope"},
		{"audit_list", "--from", "yesterday"},
		{"send_keys", "--target", "w", "--keys", "NotAKey"},
		{"sessions_list", "extra"},
	} {
		code, out, errs := runWith(t, testDeps(b), "", args...)
		if code != ExitUsage {
			t.Errorf("%v: exit %d, want %d (stdout %q stderr %q)", args, code, ExitUsage, out, errs)
		}
	}
	if b.count() != 0 {
		t.Errorf("the backend was called %d times for invalid input", b.count())
	}
}

// --- running -----------------------------------------------------------------

func TestSendTextCallsTheOpWithDerivedArguments(t *testing.T) {
	b := &fakeOpsBackend{call: answerWith(writeResult(ops.OutcomeDelivered, "", false), nil)}
	code, out, errs := runWith(t, testDeps(b), "", "send_text", "--target", "minion-3", "--text", "hello there")
	if code != 0 || errs != "" {
		t.Fatalf("exit %d, stderr %q", code, errs)
	}
	c := b.last(t)
	if c.Op != "send_text" {
		t.Fatalf("op = %s", c.Op)
	}
	m := decodeArgs(t, c.Args)
	if m["text"] != "hello there" || m["request_id"] != testRequestID {
		t.Errorf("args = %v", m)
	}
	if _, forced := m["force"]; forced {
		t.Errorf("force sent without --force: %v", m)
	}
	if tg, _ := m["target"].(map[string]any); tg["session_name"] != "minion-3" {
		t.Errorf("target = %v", m["target"])
	}
	if !strings.Contains(out, "outcome: delivered") {
		t.Errorf("output = %q", out)
	}
}

func TestTextFromStdin(t *testing.T) {
	b := &fakeOpsBackend{call: answerWith(writeResult(ops.OutcomeDelivered, "", false), nil)}
	secret := "line one\nline two with \"quotes\"\n"
	code, _, errs := runWith(t, testDeps(b), secret, "send_text", "--target", "minion-3", "--text", "-")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errs)
	}
	if got := decodeArgs(t, b.last(t).Args)["text"]; got != secret {
		t.Errorf("text = %q, want the stdin content verbatim", got)
	}
}

func TestStdinCanBeUsedOnceAndHasALimit(t *testing.T) {
	b := &fakeOpsBackend{call: answerWith(nil, nil)}
	code, _, errs := runWith(t, testDeps(b), "x", "pipe_read", "--target", "w", "--after-turn-id", "-", "--limit", "1")
	if code != ExitUsage {
		t.Errorf("a stdin value that is not a turn id: exit %d (%s)", code, errs)
	}
	s := &stdinOnce{r: strings.NewReader("a")}
	if _, err := s.read(context.Background(), "one"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.read(context.Background(), "two"); err == nil {
		t.Error("stdin was read twice")
	}
	big := &stdinOnce{r: strings.NewReader(strings.Repeat("x", maxStdinBytes+1))}
	if _, err := big.read(context.Background(), "big"); err == nil {
		t.Error("oversized stdin accepted")
	}
}

func TestForceIsSentOnlyWhenPassed(t *testing.T) {
	b := &fakeOpsBackend{call: answerWith(writeResult(ops.OutcomeFailedNotSent, proto.CodeBusy.String(), true), nil)}
	code, out, _ := runWith(t, testDeps(b), "", "pipe_send", "--target", "minion-3", "--text", "hi")
	if code != ExitFailedNotSent {
		t.Fatalf("exit %d, want 3", code)
	}
	if b.count() != 1 {
		t.Fatalf("the CLI retried on its own: %d calls", b.count())
	}
	if _, ok := decodeArgs(t, b.last(t).Args)["force"]; ok {
		t.Error("force was sent without --force")
	}
	for _, want := range []string{"outcome: failed_not_sent", "reason: busy", "forceable: true", "--force"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}

	b.call = answerWith(writeResult(ops.OutcomeDelivered, "", false), nil)
	if code, _, _ := runWith(t, testDeps(b), "", "pipe_send", "--target", "minion-3", "--text", "hi", "--force"); code != 0 {
		t.Fatalf("with --force: exit %d", code)
	}
	if got := decodeArgs(t, b.last(t).Args)["force"]; got != true {
		t.Errorf("force = %v, want true", got)
	}
}

func TestExitCodeMapping(t *testing.T) {
	for _, c := range []struct {
		outcome ops.Outcome
		want    int
	}{
		{ops.OutcomeDelivered, 0},
		{ops.OutcomeFailedNotSent, 3},
		{ops.OutcomePartial, 4},
		{ops.OutcomeUnknown, 4},
		{ops.Outcome("from_the_future"), 4},
	} {
		if got := outcomeExitCode(c.outcome); got != c.want {
			t.Errorf("outcomeExitCode(%q) = %d, want %d", c.outcome, got, c.want)
		}
		b := &fakeOpsBackend{call: answerWith(writeResult(c.outcome, "some_reason", false), nil)}
		code, _, _ := runWith(t, testDeps(b), "", "send_keys", "--target", "w", "--keys", "Enter")
		if code != c.want {
			t.Errorf("send_keys with outcome %q exits %d, want %d", c.outcome, code, c.want)
		}
	}
}

func TestWriteResultShowsWarningsAndPaneInHumanOutput(t *testing.T) {
	res := ops.WriteResult{Outcome: ops.OutcomeDelivered, Forced: true,
		Warnings: []ops.Warning{{Code: "readiness_unknown", Message: "could not tell if the agent is ready"}},
		Pane:     &ops.PaneInfo{Session: "work", ID: "%1", Window: 0, Index: 2, Kind: "shell", State: "stable"}}
	raw, _ := json.Marshal(res)
	b := &fakeOpsBackend{call: answerWith(raw, nil)}
	code, out, _ := runWith(t, testDeps(b), "", "send_text", "--target", "work", "--text", "x")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	want := "outcome: delivered\nforceable: false\nforced: true\nwarning: readiness_unknown: could not tell if the agent is ready\npane: work %1 (0.2) kind=shell state=stable\n"
	if out != want {
		t.Errorf("output:\n%s\nwant:\n%s", out, want)
	}
}

func TestJSONOutputIsTheResultAndKeepsTheExitCode(t *testing.T) {
	raw := writeResult(ops.OutcomeFailedNotSent, "busy", true)
	b := &fakeOpsBackend{call: answerWith(raw, nil)}
	code, out, _ := runWith(t, testDeps(b), "", "send_text", "--target", "w", "--text", "x", "--json")
	if code != ExitFailedNotSent {
		t.Errorf("exit %d", code)
	}
	if strings.Count(out, "\n") != 1 {
		t.Fatalf("json output is not one line: %q", out)
	}
	var res ops.WriteResult
	if err := json.Unmarshal([]byte(out), &res); err != nil || res.Outcome != ops.OutcomeFailedNotSent || !res.Forceable || res.Reason != "busy" {
		t.Fatalf("json output = %q (%v)", out, err)
	}
}

func TestConnectionLostDuringAWriteIsUnknownNotAnError(t *testing.T) {
	b := &fakeOpsBackend{call: answerWith(nil, fmt.Errorf("%w: eof", client.ErrConnectionLost))}
	code, out, errs := runWith(t, testDeps(b), "", "send_text", "--target", "w", "--text", "x")
	if code != ExitPartial {
		t.Errorf("exit %d, want 4", code)
	}
	if !strings.Contains(out, "outcome: unknown") || !strings.Contains(out, "reason: connection_lost") {
		t.Errorf("stdout = %q", out)
	}
	if !strings.Contains(errs, "--request-id "+testRequestID) || !strings.Contains(errs, "10 minutes") {
		t.Errorf("stderr lacks the safe-retry hint with its time limit: %q", errs)
	}
	// A read that loses the connection is an ordinary error.
	b.call = answerWith(nil, client.ErrConnectionLost)
	if code, _, _ := runWith(t, testDeps(b), "", "session_snapshot", "--target", "w"); code != ExitError {
		t.Errorf("read: exit %d, want 1", code)
	}
}

func TestAPIErrorsAreErrorsWithTheCode(t *testing.T) {
	b := &fakeOpsBackend{call: answerWith(nil, proto.NewError(proto.CodeForbidden, "preview only"))}
	code, out, errs := runWith(t, testDeps(b), "", "send_text", "--target", "w", "--text", "x")
	if code != ExitError || out != "" || !strings.Contains(errs, "forbidden: preview only") {
		t.Errorf("exit %d, stdout %q, stderr %q", code, out, errs)
	}
	b.call = answerWith(nil, client.ErrWorkerNotRunning)
	code, _, errs = runWith(t, testDeps(b), "", "sessions_list")
	if code != ExitError || !strings.Contains(errs, "minion worker run") {
		t.Errorf("exit %d, stderr %q", code, errs)
	}
}

func TestRequestIDFlagIsUsed(t *testing.T) {
	b := &fakeOpsBackend{call: answerWith(writeResult(ops.OutcomeDelivered, "", false), nil)}
	id := "01890a5d-ac96-774b-bcce-b302099a8058"
	if code, _, errs := runWith(t, testDeps(b), "", "send_text", "--target", "w", "--text", "x", "--request-id", id); code != 0 {
		t.Fatalf("exit %d: %s", code, errs)
	}
	if got := decodeArgs(t, b.last(t).Args)["request_id"]; got != id {
		t.Errorf("request_id = %v", got)
	}
}

func TestReadOpsPrintHumanAndJSON(t *testing.T) {
	list, _ := json.Marshal(ops.SessionsListResult{Sessions: []ops.SessionInfo{
		{Name: "minion-1", Windows: 2, Attached: true, Task: "fix the build", Pane: &ops.PaneInfo{Kind: "agent:claude", State: "busy"}},
		{Name: "plain", Windows: 1},
	}})
	b := &fakeOpsBackend{call: answerWith(list, nil)}
	code, out, _ := runWith(t, testDeps(b), "", "sessions_list")
	if code != 0 || b.last(t).Op != "sessions_list" {
		t.Fatalf("exit %d", code)
	}
	for _, want := range []string{"NAME", "minion-1", "agent:claude", "busy", "fix the build", "plain"} {
		if !strings.Contains(out, want) {
			t.Errorf("sessions_list output lacks %q:\n%s", want, out)
		}
	}
	code, out, _ = runWith(t, testDeps(b), "", "sessions_list", "--json")
	if code != 0 || strings.TrimSpace(out) != string(list) {
		t.Errorf("--json output = %q, want %s", out, list)
	}

	scr, _ := json.Marshal(ops.SessionSnapshotResult{Screen: ops.Screen{Lines: []string{"$ ls", "file"}}})
	b.call = answerWith(scr, nil)
	if code, out, _ = runWith(t, testDeps(b), "", "session_snapshot", "--target", "w"); code != 0 || out != "$ ls\nfile\n" {
		t.Errorf("snapshot: exit %d, output %q", code, out)
	}
}

func TestRemoteTextCannotDriveTheTerminal(t *testing.T) {
	scr, _ := json.Marshal(ops.SessionCaptureResult{Screen: ops.Screen{Lines: []string{"evil\x1b[2J\x07done"}}})
	b := &fakeOpsBackend{call: answerWith(scr, nil)}
	_, out, _ := runWith(t, testDeps(b), "", "session_capture", "--target", "w")
	if strings.ContainsAny(out, "\x1b\x07") {
		t.Errorf("control characters reached the terminal: %q", out)
	}
	res := ops.WriteResult{Outcome: ops.OutcomeFailedNotSent, Reason: "x\x1b]0;title\x07", Warnings: []ops.Warning{{Code: "w", Message: "a\x1bb"}}}
	raw, _ := json.Marshal(res)
	b.call = answerWith(raw, nil)
	_, out, _ = runWith(t, testDeps(b), "", "send_text", "--target", "w", "--text", "x")
	if strings.ContainsAny(out, "\x1b\x07") {
		t.Errorf("control characters in a write result: %q", out)
	}
}

func TestPipeWaitStreamsElements(t *testing.T) {
	turn := func(id, dir, text string) json.RawMessage {
		raw, _ := json.Marshal(ops.PipeTurn{TurnID: id, Session: "w", Direction: dir, Text: text, At: ops.Timestamp(time.Date(2026, 11, 10, 9, 8, 7, 0, time.UTC))})
		return raw
	}
	b := &fakeOpsBackend{stream: func(op string, args json.RawMessage, on func(json.RawMessage) error) (json.RawMessage, error) {
		for _, el := range []json.RawMessage{turn("01890a5d-ac96-774b-bcce-b302099a8001", "received", "first\nsecond"), turn("01890a5d-ac96-774b-bcce-b302099a8002", "received", "third")} {
			if err := on(el); err != nil {
				return nil, err
			}
		}
		return json.RawMessage(`{}`), nil
	}}
	code, out, errs := runWith(t, testDeps(b), "", "pipe_wait", "--target", "w", "--timeout-ms", "1000")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errs)
	}
	want := "01890a5d-ac96-774b-bcce-b302099a8001  2026-11-10T09:08:07.000Z  received\n  first\n  second\n" +
		"01890a5d-ac96-774b-bcce-b302099a8002  2026-11-10T09:08:07.000Z  received\n  third\n"
	if out != want {
		t.Errorf("output:\n%q\nwant:\n%q", out, want)
	}
	if got := decodeArgs(t, b.last(t).Args)["timeout_ms"]; got != float64(1000) {
		t.Errorf("timeout_ms = %v", got)
	}
	code, out, _ = runWith(t, testDeps(b), "", "pipe_wait", "--target", "w", "--json")
	if code != 0 || strings.Count(out, "\n") != 2 || !strings.HasPrefix(out, `{"turn_id":"01890a5d-ac96-774b-bcce-b302099a8001"`) {
		t.Errorf("json stream = %q", out)
	}
}

func TestPipeWaitStopsWhenStdoutFails(t *testing.T) {
	// The element callback returns the write error, which stops the stream.
	stopped := errors.New("stopped")
	b := &fakeOpsBackend{stream: func(string, json.RawMessage, func(json.RawMessage) error) (json.RawMessage, error) {
		return nil, stopped
	}}
	if code, _, errs := runWith(t, testDeps(b), "", "pipe_wait", "--target", "w"); code != ExitError || !strings.Contains(errs, "stopped") {
		t.Errorf("exit %d, stderr %q", code, errs)
	}
}

// --- targets and backends ----------------------------------------------------

func TestParseTargetFlag(t *testing.T) {
	worker := "01890a5d-ac96-774b-bcce-b302099a8057"
	for _, c := range []struct {
		in   string
		want ops.Target
		bad  bool
	}{
		{in: "minion-3", want: ops.Target{SessionName: "minion-3"}},
		{in: "minion-3:0.1", want: ops.Target{SessionName: "minion-3", Pane: "0.1"}},
		{in: worker + ":minion-3", want: ops.Target{WorkerID: worker, SessionName: "minion-3"}},
		{in: worker + ":minion-3:2.0", want: ops.Target{WorkerID: worker, SessionName: "minion-3", Pane: "2.0"}},
		{in: "my session", want: ops.Target{SessionName: "my session"}},
		{in: "", bad: true},
		{in: "pi-1:minion-3", bad: true}, // a Worker name, not an id
		{in: "a:b:c:d", bad: true},
		{in: "minion-3:x.y", bad: true},
		{in: "a.b", bad: true},
	} {
		got, err := ParseTargetFlag(c.in)
		if c.bad {
			if err == nil {
				t.Errorf("ParseTargetFlag(%q) = %+v, want an error", c.in, got)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("ParseTargetFlag(%q) = %+v, %v; want %+v", c.in, got, err, c.want)
		}
	}
}

func TestDefaultTargetIsTheCurrentSession(t *testing.T) {
	b := &fakeOpsBackend{call: answerWith(writeResult(ops.OutcomeDelivered, "", false), nil)}
	deps := testDeps(b)
	deps.session = func(context.Context) (string, error) { return "minion-9", nil }
	if code, _, errs := runWith(t, deps, "", "send_text", "--text", "x"); code != 0 {
		t.Fatalf("exit %d: %s", code, errs)
	}
	tg, _ := decodeArgs(t, b.last(t).Args)["target"].(map[string]any)
	if tg["session_name"] != "minion-9" {
		t.Errorf("target = %v", tg)
	}
	// An explicit --target wins and the resolver is not asked.
	deps.session = func(context.Context) (string, error) { t.Error("resolver called"); return "", nil }
	if code, _, _ := runWith(t, deps, "", "send_text", "--text", "x", "--target", "other"); code != 0 {
		t.Fatal("explicit target failed")
	}
}

func TestNoTargetOutsideTmuxIsAUsageError(t *testing.T) {
	b := &fakeOpsBackend{call: answerWith(nil, nil)}
	code, _, errs := runWith(t, testDeps(b), "", "send_text", "--text", "x")
	if code != ExitUsage || !strings.Contains(errs, "--target") || b.count() != 0 {
		t.Errorf("exit %d, stderr %q, calls %d", code, errs, b.count())
	}
	deps := testDeps(b)
	deps.session = func(context.Context) (string, error) { return "", errors.New("tmux is gone") }
	if code, _, errs := runWith(t, deps, "", "send_text", "--text", "x"); code != ExitUsage || !strings.Contains(errs, "tmux is gone") {
		t.Errorf("resolver error: exit %d, stderr %q", code, errs)
	}
}

// fakeTmuxLister is the tmux adapter seam of the default session resolver.
type fakeTmuxLister struct {
	sessions []term.Session
	err      error
}

func (f fakeTmuxLister) ListSessions(context.Context) ([]term.Session, error) {
	return f.sessions, f.err
}

func (f fakeTmuxLister) SetOption(context.Context, string, string, string) error { return nil }

func paneSession(name string, ids ...string) term.Session {
	s := term.Session{Name: name}
	for i, id := range ids {
		s.Panes = append(s.Panes, term.Pane{Ref: term.PaneRef{ID: id}, Index: i})
	}
	return s
}

func TestResolveTmuxSession(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	var gotSocket string
	opened := 0
	lister := fakeTmuxLister{sessions: []term.Session{paneSession("other", "%1"), paneSession("minion-4", "%6", "%7")}}
	open := func(sock string) (sessionLister, error) {
		opened++
		gotSocket = sock
		return lister, nil
	}
	inTmux := func(pane string) func(string) string {
		return env(map[string]string{"TMUX": "/tmp/tmux-1000/default,1234,0", "TMUX_PANE": pane})
	}
	// Outside tmux (no $TMUX or no $TMUX_PANE): no session, no tmux.
	for _, m := range []map[string]string{nil, {"TMUX_PANE": "%7"}, {"TMUX": "/s,1,0"}} {
		if name, err := resolveTmuxSession(env(m), open)(context.Background()); name != "" || err != nil || opened != 0 {
			t.Errorf("env %v = %q, %v, opened %d", m, name, err, opened)
		}
	}
	// The pane is found through the adapter on the socket of $TMUX.
	name, err := resolveTmuxSession(inTmux("%7"), open)(context.Background())
	if err != nil || name != "minion-4" {
		t.Fatalf("= %q, %v", name, err)
	}
	if gotSocket != "/tmp/tmux-1000/default" {
		t.Errorf("socket = %q", gotSocket)
	}
	// A comma in the socket path is part of the path.
	var sock string
	commaOpen := func(s string) (sessionLister, error) { sock = s; return lister, nil }
	commaEnv := env(map[string]string{"TMUX": "/tmp/a,b/sock,99,3", "TMUX_PANE": "%1"})
	if name, err := resolveTmuxSession(commaEnv, commaOpen)(context.Background()); err != nil || name != "other" || sock != "/tmp/a,b/sock" {
		t.Errorf("comma socket: %q, %v, socket %q", name, err, sock)
	}
	// A pane that is in no session is an error.
	if _, err := resolveTmuxSession(inTmux("%99"), open)(context.Background()); err == nil {
		t.Error("an unknown pane resolved")
	}
	// A hostile $TMUX_PANE never reaches the adapter.
	before := opened
	for _, bad := range []string{"-t evil", "%x", "7", "%7;list-sessions"} {
		if _, err := resolveTmuxSession(inTmux(bad), open)(context.Background()); err == nil {
			t.Errorf("TMUX_PANE=%q accepted", bad)
		}
	}
	if opened != before {
		t.Error("the adapter was opened for an invalid pane id")
	}
	if _, err := resolveTmuxSession(env(map[string]string{"TMUX": "nonsense", "TMUX_PANE": "%1"}), open)(context.Background()); err == nil {
		t.Error("a malformed $TMUX was accepted")
	}
	failOpen := func(string) (sessionLister, error) { return nil, errors.New("tmux too old") }
	if _, err := resolveTmuxSession(inTmux("%1"), failOpen)(context.Background()); err == nil {
		t.Error("an adapter failure was not reported")
	}
	lister.err = errors.New("no server")
	if _, err := resolveTmuxSession(inTmux("%1"), open)(context.Background()); err == nil {
		t.Error("tmux failure not reported")
	}
}

// fakeServer is one tmux server for the server check: sessions with a task
// and a probe option, written by the shell side and read by the Worker side.
type fakeServer struct {
	mu       sync.Mutex
	sessions []term.Session
	setErr   error
	sets     []string // "option=value" of every write, in order
}

func (f *fakeServer) ListSessions(context.Context) ([]term.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]term.Session(nil), f.sessions...), nil
}

func (f *fakeServer) SetOption(_ context.Context, session, key, value string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.setErr != nil {
		return f.setErr
	}
	if key != "@minion_task" && key != "@minion_probe" {
		return fmt.Errorf("unexpected option %q", key)
	}
	for i := range f.sessions {
		if f.sessions[i].Name == session {
			if key == "@minion_task" {
				f.sessions[i].Task = value
			} else {
				f.sessions[i].Probe = value
			}
			f.sets = append(f.sets, key+"="+value)
			return nil
		}
	}
	return term.ErrNotFound
}

// probe is the probe option of session name.
func (f *fakeServer) probe(name string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, s := range f.sessions {
		if s.Name == name {
			return s.Probe
		}
	}
	return ""
}

// setTask is a set_task of another writer.
func (f *fakeServer) setTask(name, task string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.sessions {
		if f.sessions[i].Name == name {
			f.sessions[i].Task = task
		}
	}
}

func (f *fakeServer) task(name string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, s := range f.sessions {
		if s.Name == name {
			return s.Task
		}
	}
	return ""
}

// workerOn is a Worker backend whose sessions_list reads server.
func workerOn(server *fakeServer, share ...string) *fakeOpsBackend {
	return &fakeOpsBackend{call: func(op string, _ json.RawMessage) (json.RawMessage, error) {
		if op != ops.OpSessionsList {
			return nil, fmt.Errorf("unexpected op %q", op)
		}
		var res ops.SessionsListResult
		sessions, _ := server.ListSessions(context.Background())
		for _, s := range sessions {
			for _, name := range share {
				if s.Name == name {
					res.Sessions = append(res.Sessions, ops.SessionInfo{Name: s.Name, Task: s.Task, Probe: s.Probe})
				}
			}
		}
		return json.Marshal(res)
	}}
}

func TestConfirmTmuxServer(t *testing.T) {
	getenv := func(k string) string {
		return map[string]string{"TMUX": "/tmp/shell.sock,1,0", "TMUX_PANE": "%1"}[k]
	}
	fixedNonce := func() (string, error) { return "nonce-1", nil }
	newShell := func() *fakeServer {
		s := paneSession("work", "%1")
		s.Task = "my task"
		return &fakeServer{sessions: []term.Session{s}}
	}
	openOn := func(s *fakeServer) func(string) (sessionLister, error) {
		return func(string) (sessionLister, error) { return s, nil }
	}
	ctx := context.Background()

	// Same server: the Worker echoes the nonce; the task text is never written
	// and the probe is cleared afterwards.
	shell := newShell()
	if err := confirmTmuxServer(getenv, openOn(shell), fixedNonce, noLock)(ctx, "work", workerOn(shell, "work")); err != nil {
		t.Fatalf("same server refused: %v", err)
	}
	if got := shell.task("work"); got != "my task" {
		t.Errorf("task after the check = %q, want it untouched", got)
	}
	if got := shell.probe("work"); got != "" {
		t.Errorf("probe left behind: %q", got)
	}
	if len(shell.sets) != 2 || shell.sets[0] != "@minion_probe=nonce-1" || shell.sets[1] != "@minion_probe=" {
		t.Errorf("writes = %v, want only the probe option", shell.sets)
	}

	// Another server with a session of the same name: refused, and neither
	// server's task text is touched.
	shell = newShell()
	other := &fakeServer{sessions: []term.Session{paneSession("work", "%1")}}
	err := confirmTmuxServer(getenv, openOn(shell), fixedNonce, noLock)(ctx, "work", workerOn(other, "work"))
	if err == nil || !strings.Contains(err.Error(), "not the one the Worker serves") {
		t.Fatalf("different server: err = %v", err)
	}
	if got := shell.task("work"); got != "my task" || shell.probe("work") != "" {
		t.Errorf("after a refusal: task %q probe %q", got, shell.probe("work"))
	}
	if len(other.sets) != 0 {
		t.Errorf("the other server was written to: %v", other.sets)
	}
	if !strings.Contains(err.Error(), "--target") {
		t.Errorf("the refusal does not name --target: %v", err)
	}

	// The Worker does not share the session: refused before anything is written.
	shell = newShell()
	if err := confirmTmuxServer(getenv, openOn(shell), fixedNonce, noLock)(ctx, "work", workerOn(shell)); err == nil {
		t.Error("an unshared session was accepted")
	}
	if len(shell.sets) != 0 {
		t.Errorf("a refusal without a candidate wrote %v", shell.sets)
	}

	// No Worker reachable: refused.
	shell = newShell()
	down := &fakeOpsBackend{call: answerWith(nil, client.ErrWorkerNotRunning)}
	if err := confirmTmuxServer(getenv, openOn(shell), fixedNonce, noLock)(ctx, "work", down); !errors.Is(err, client.ErrWorkerNotRunning) {
		t.Errorf("worker down: err = %v", err)
	}

	// The marker cannot be written: refused.
	shell = newShell()
	shell.setErr = errors.New("read-only")
	if err := confirmTmuxServer(getenv, openOn(shell), fixedNonce, noLock)(ctx, "work", workerOn(shell, "work")); err == nil {
		t.Error("an unwritable marker was accepted")
	}

	// The pane is not in the session the resolver named, or not in tmux.
	shell = newShell()
	if err := confirmTmuxServer(getenv, openOn(shell), fixedNonce, noLock)(ctx, "elsewhere", workerOn(shell, "elsewhere")); err == nil {
		t.Error("a session that does not own the pane was accepted")
	}
	if err := confirmTmuxServer(func(string) string { return "" }, openOn(shell), fixedNonce, noLock)(ctx, "work", workerOn(shell, "work")); err == nil {
		t.Error("accepted outside tmux")
	}
}

func TestImplicitTargetNeedsAConfirmedServer(t *testing.T) {
	b := &fakeOpsBackend{call: answerWith(writeResult(ops.OutcomeDelivered, "", false), nil)}
	deps := testDeps(b)
	deps.session = func(context.Context) (string, error) { return "minion-9", nil }

	// A refusing confirmer: usage error, nothing sent.
	deps.confirm = func(context.Context, string, client.Backend) error { return errors.New("other server") }
	code, _, errs := runWith(t, deps, "", "send_text", "--text", "x")
	if code != ExitUsage || !strings.Contains(errs, "other server") || b.count() != 0 {
		t.Errorf("refused: exit %d, stderr %q, calls %d", code, errs, b.count())
	}
	// No confirmer at all: refused.
	deps.confirm = nil
	if code, _, _ := runWith(t, deps, "", "send_text", "--text", "x"); code != ExitUsage || b.count() != 0 {
		t.Errorf("nil confirmer: exit %d, calls %d", code, b.count())
	}
	// The confirmer sees the session name and the local backend.
	var gotName string
	var gotBackend client.Backend
	deps.confirm = func(_ context.Context, name string, w client.Backend) error {
		gotName, gotBackend = name, w
		return nil
	}
	if code, _, errs := runWith(t, deps, "", "send_text", "--text", "x"); code != 0 || gotName != "minion-9" || gotBackend != client.Backend(b) {
		t.Errorf("confirmed: exit %d (%s), name %q", code, errs, gotName)
	}
	// An explicit target never runs the confirmer.
	deps.confirm = func(context.Context, string, client.Backend) error { t.Error("confirmer called"); return nil }
	if code, _, _ := runWith(t, deps, "", "send_text", "--text", "x", "--target", "other"); code != 0 {
		t.Error("explicit target failed")
	}
	// Through a Hub there is no Worker to match: refused.
	hub := &fakeOpsBackend{call: answerWith(writeResult(ops.OutcomeDelivered, "", false), nil)}
	deps.hub = func() (client.Backend, error) { return hub, nil }
	before := b.count()
	if code, _, errs := runWith(t, deps, "", "send_text", "--text", "x"); code != ExitUsage || hub.count() != 0 || b.count() != before || !strings.Contains(errs, "Hub") {
		t.Errorf("hub: exit %d, stderr %q, hub calls %d", code, errs, hub.count())
	}
}

func TestHubOpsNeedAHubAndWorkerOpsPreferIt(t *testing.T) {
	local := &fakeOpsBackend{call: answerWith(writeResult(ops.OutcomeDelivered, "", false), nil)}
	deps := testDeps(local)
	code, _, errs := runWith(t, deps, "", "settings_list")
	if code != ExitError || !strings.Contains(errs, "no Hub is configured") || local.count() != 0 {
		t.Errorf("hub op without hub: exit %d, stderr %q, local calls %d", code, errs, local.count())
	}

	hub := &fakeOpsBackend{call: answerWith(writeResult(ops.OutcomeDelivered, "", false), nil)}
	deps.hub = func() (client.Backend, error) { return hub, nil }
	if code, _, errs := runWith(t, deps, "", "send_text", "--target", "w", "--text", "x"); code != 0 {
		t.Fatalf("exit %d: %s", code, errs)
	}
	if hub.count() != 1 || local.count() != 0 {
		t.Errorf("calls: hub %d, local %d; the configured Hub must serve the op", hub.count(), local.count())
	}

	deps.hub = func() (client.Backend, error) { return nil, errors.New("api key file unreadable") }
	if code, _, errs := runWith(t, deps, "", "sessions_list"); code != ExitError || !strings.Contains(errs, "api key file unreadable") {
		t.Errorf("hub config error: exit %d, stderr %q", code, errs)
	}

	// "No Hub configured" falls back to the local socket.
	deps.hub = func() (client.Backend, error) { return nil, nil }
	local.call = answerWith(json.RawMessage(`{"sessions":[]}`), nil)
	if code, out, _ := runWith(t, deps, "", "sessions_list"); code != 0 || !strings.Contains(out, "no sessions") {
		t.Errorf("fallback: exit %d, output %q", code, out)
	}
}

func TestSetHubBackendIsUsedByTheDefaultDeps(t *testing.T) {
	t.Cleanup(func() { SetHubBackend(nil) })
	if b, err := defaultOpsDeps().hub(); b != nil || err != nil {
		t.Fatalf("without a factory = %v, %v", b, err)
	}
	hub := &fakeOpsBackend{}
	SetHubBackend(func() (client.Backend, error) { return hub, nil })
	if b, err := defaultOpsDeps().hub(); b != hub || err != nil {
		t.Fatalf("with a factory = %v, %v", b, err)
	}
}

func TestSecretsAreNotNeededOnArgv(t *testing.T) {
	// A string argument of a Hub op can be given on stdin.
	hub := &fakeOpsBackend{call: answerWith(writeResult(ops.OutcomeDelivered, "", false), nil)}
	deps := testDeps(&fakeOpsBackend{})
	deps.hub = func() (client.Backend, error) { return hub, nil }
	code, _, errs := runWith(t, deps, "s3cret\n", "settings_set", "--key", "session_ttl", "--value", "-")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errs)
	}
	if got := decodeArgs(t, hub.last(t).Args)["value"]; got != "s3cret\n" {
		t.Errorf("value = %v", got)
	}
}

// --- help --------------------------------------------------------------------

func TestGoldenHelpOfGeneratedCommands(t *testing.T) {
	for _, name := range []string{"sessions_list", "send_text", "answer", "send_keys", "session_capture", "pipe_wait", "settings_set", "audit_list", "password_change"} {
		t.Run(name, func(t *testing.T) {
			root := &cobra.Command{Use: "minion", SilenceUsage: true, SilenceErrors: true}
			addOpCommands(root, testDeps(&fakeOpsBackend{}), ops.All())
			var out bytes.Buffer
			root.SetOut(&out)
			root.SetErr(&out)
			root.SetArgs([]string{name, "--help"})
			if err := root.Execute(); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join("testdata", "help_"+name+".golden")
			if *update {
				if err := os.MkdirAll("testdata", 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, out.Bytes(), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("%v (run go test ./internal/cli -run Golden -update)", err)
			}
			if out.String() != string(want) {
				t.Errorf("help for %s changed; run with -update after review.\ngot:\n%s\nwant:\n%s", name, out.String(), want)
			}
		})
	}
}

// --- interrupted writes, failing output, secrets, foreign Workers --------------

func TestInterruptedWriteIsUnknownNotAnError(t *testing.T) {
	for _, cause := range []error{context.Canceled, context.DeadlineExceeded, fmt.Errorf("wrapped: %w", context.Canceled)} {
		b := &fakeOpsBackend{call: answerWith(nil, cause)}
		code, out, errs := runWith(t, testDeps(b), "", "send_text", "--target", "w", "--text", "x")
		if code != ExitPartial {
			t.Errorf("%v: exit %d, want 4", cause, code)
		}
		if !strings.Contains(out, "outcome: unknown") || !strings.Contains(out, "reason: connection_lost") {
			t.Errorf("%v: stdout = %q", cause, out)
		}
		if !strings.Contains(errs, "--request-id "+testRequestID) {
			t.Errorf("%v: stderr lacks the retry hint: %q", cause, errs)
		}
	}
	// A read that is interrupted is an ordinary error.
	b := &fakeOpsBackend{call: answerWith(nil, context.Canceled)}
	if code, _, _ := runWith(t, testDeps(b), "", "session_snapshot", "--target", "w"); code != ExitError {
		t.Errorf("read: exit %d, want 1", code)
	}
}

// failAfter is a writer that fails once n bytes were written.
type failAfter struct {
	n   int
	err error
}

func (f *failAfter) Write(p []byte) (int, error) {
	if f.n <= 0 {
		return 0, f.err
	}
	if len(p) > f.n {
		n := f.n
		f.n = 0
		return n, f.err
	}
	f.n -= len(p)
	return len(p), nil
}

func TestFailingStdoutIsAnErrorForEveryRenderer(t *testing.T) {
	full := errors.New("no space left on device")
	b := &fakeOpsBackend{call: func(op string, _ json.RawMessage) (json.RawMessage, error) {
		switch op {
		case "sessions_list":
			return json.RawMessage(`{"sessions":[{"name":"a","windows":1}]}`), nil
		case "session_capture":
			return json.RawMessage(`{"screen":{"lines":["x","y"]}}`), nil
		}
		return writeResult(ops.OutcomeDelivered, "", false), nil
	}}
	for _, args := range [][]string{
		{"send_text", "--target", "w", "--text", "x"},
		{"sessions_list"},
		{"session_capture", "--target", "w"},
	} {
		code, errs := runWithIO(context.Background(), testDeps(b), strings.NewReader(""), &failAfter{err: full}, args...)
		if code != ExitError || !strings.Contains(errs, "no space left") {
			t.Errorf("%v: exit %d, stderr %q; a lost result must not exit 0", args, code, errs)
		}
	}
}

func TestPipeWaitStopsWhenTheWriterFailsInTheCallback(t *testing.T) {
	full := errors.New("broken pipe")
	delivered := 0
	b := &fakeOpsBackend{stream: func(_ string, _ json.RawMessage, on func(json.RawMessage) error) (json.RawMessage, error) {
		for i := 0; i < 5; i++ {
			turn, _ := json.Marshal(ops.PipeTurn{TurnID: "t", Session: "w", Direction: "in", Text: "hello"})
			if err := on(turn); err != nil {
				return nil, err
			}
			delivered++
		}
		return nil, nil
	}}
	code, errs := runWithIO(context.Background(), testDeps(b), strings.NewReader(""), &failAfter{err: full}, "pipe_wait", "--target", "w")
	if code != ExitError || !strings.Contains(errs, "broken pipe") || delivered != 0 {
		t.Errorf("exit %d, stderr %q, elements accepted after the failure %d", code, errs, delivered)
	}
}

func TestSecretFlagsRefuseALiteralValue(t *testing.T) {
	hub := &fakeOpsBackend{call: answerWith(writeResult(ops.OutcomeDelivered, "", false), nil)}
	deps := testDeps(&fakeOpsBackend{})
	deps.hub = func() (client.Backend, error) { return hub, nil }

	for _, args := range [][]string{
		{"oidc_provider_set", "--id", "corp", "--client-secret", "s3cret"},
		{"settings_set", "--key", "oidc_client_secret", "--value", "s3cret"},
	} {
		code, _, errs := runWith(t, deps, "", args...)
		if code != ExitUsage || !strings.Contains(errs, "standard input") || strings.Contains(errs, "s3cret") {
			t.Errorf("%v: exit %d, stderr %q", args, code, errs)
		}
		if hub.count() != 0 {
			t.Fatalf("%v: the backend was called with a literal secret", args)
		}
	}

	// Standard input is accepted.
	code, _, errs := runWith(t, deps, "s3cret", "oidc_provider_set", "--id", "corp", "--name", "Corp", "--issuer", "https://idp.example.com", "--client-id", "c", "--client-secret", "-")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errs)
	}
	if got := decodeArgs(t, hub.last(t).Args)["client_secret"]; got != "s3cret" {
		t.Errorf("client_secret = %v", got)
	}
	code, _, errs = runWith(t, deps, "s3cret", "settings_set", "--key", "oidc_client_secret", "--value", "-")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errs)
	}
	if got := decodeArgs(t, hub.last(t).Args)["value"]; got != "s3cret" {
		t.Errorf("value = %v", got)
	}
	// A value for an ordinary setting may be a literal.
	if code, _, errs := runWith(t, deps, "", "settings_set", "--key", "log_level", "--value", "debug"); code != 0 {
		t.Fatalf("exit %d: %s", code, errs)
	}
	// The help says so.
	root := &cobra.Command{Use: "minion"}
	addOpCommands(root, deps, ops.All())
	cmd, _, _ := root.Find([]string{"oidc_provider_set"})
	if u := cmd.Flags().Lookup("client-secret").Usage; !strings.Contains(u, "never taken from the command line") || !strings.Contains(u, "--client-secret-file") {
		t.Errorf("client-secret help = %q", u)
	}
	cmd, _, _ = root.Find([]string{"settings_set"})
	if u := cmd.Flags().Lookup("value").Usage; !strings.Contains(u, "read from standard input only") {
		t.Errorf("value help = %q", u)
	}
}

// blockingReader never returns: standard input held open by a terminal or a
// parent process.
type blockingReader struct{ release chan struct{} }

func (r blockingReader) Read([]byte) (int, error) { <-r.release; return 0, io.EOF }

func TestWaitingForStdinEndsWithTheContext(t *testing.T) {
	r := blockingReader{release: make(chan struct{})}
	defer close(r.release)
	b := &fakeOpsBackend{call: answerWith(writeResult(ops.OutcomeDelivered, "", false), nil)}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() {
		var out bytes.Buffer
		code, _ := runWithIO(ctx, testDeps(b), r, &out, "send_text", "--target", "w", "--text", "-")
		done <- code
	}()
	cancel()
	select {
	case code := <-done:
		if code != ExitError || b.count() != 0 {
			t.Errorf("exit %d, calls %d", code, b.count())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the command kept waiting for standard input after the context ended")
	}
}

func TestForeignWorkerTargetIsRefusedOnTheLocalSocket(t *testing.T) {
	local := &fakeOpsBackend{call: answerWith(writeResult(ops.OutcomeDelivered, "", false), nil)}
	foreign := "01890a5d-ac96-774b-bcce-b302099a8057"
	code, _, errs := runWith(t, testDeps(local), "", "send_text", "--target", foreign+":work", "--text", "x")
	if code != ExitUsage || !strings.Contains(errs, "no Hub") || local.count() != 0 {
		t.Errorf("exit %d, stderr %q, local calls %d", code, errs, local.count())
	}
	// With a Hub the Worker part is routed by the Hub.
	hub := &fakeOpsBackend{call: answerWith(writeResult(ops.OutcomeDelivered, "", false), nil)}
	deps := testDeps(local)
	deps.hub = func() (client.Backend, error) { return hub, nil }
	if code, _, errs := runWith(t, deps, "", "send_text", "--target", foreign+":work", "--text", "x"); code != 0 || hub.count() != 1 {
		t.Errorf("hub: exit %d (%s), hub calls %d", code, errs, hub.count())
	}
}

// noLock is the probe lock of tests that run one probe at a time.
func noLock(context.Context, string) (func(), error) { return func() {}, nil }
