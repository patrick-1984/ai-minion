package ops

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/proto"
)

type expectedOp struct {
	actions string
	write   bool
	stream  bool
	args    string
	result  string
	guards  string // name:CLASS list, "" for none
}

// The release-1 Worker ops of story 1.3, spelled out independently of the
// declarations.
var expectedCore = map[string]expectedOp{
	"sessions_list":    {"read", false, false, "SessionsListArgs", "SessionsListResult", ""},
	"session_create":   {"operate", true, false, "SessionCreateArgs", "WriteResult", "session_name_free:KEEP"},
	"session_snapshot": {"read", false, false, "SessionSnapshotArgs", "SessionSnapshotResult", ""},
	"session_capture":  {"read", false, false, "SessionCaptureArgs", "SessionCaptureResult", ""},
	"send_text":        {"operate", true, false, "SendTextArgs", "WriteResult", "target_recheck:KEEP,writer_lock:KEEP,pane_busy:FORCE,pane_dialog:FORCE,pane_shell:FORCE,readiness_unknown:WARN"},
	"send_keys":        {"operate", true, false, "SendKeysArgs", "WriteResult", "target_recheck:KEEP,writer_lock:KEEP,pane_busy:FORCE,pane_dialog:FORCE,pane_shell:FORCE,readiness_unknown:WARN"},
	"set_task":         {"operate", true, false, "SetTaskArgs", "WriteResult", "session_exists:KEEP"},
	"answer":           {"operate", true, false, "AnswerArgs", "WriteResult", "target_recheck:KEEP,writer_lock:KEEP,dialog_binding:KEEP"},
	"pipe_send":        {"message|operate", true, false, "PipeSendArgs", "WriteResult", "target_recheck:KEEP,writer_lock:KEEP,pipe_limits:KEEP,pane_busy:FORCE,pane_dialog:FORCE,pane_shell:FORCE,readiness_unknown:WARN"},
	"pipe_read":        {"read", false, false, "PipeReadArgs", "PipeReadResult", ""},
	"pipe_wait":        {"read", false, true, "PipeWaitArgs", "PipeTurn", ""},
}

// coreOps returns the core Worker ops in the order of All (sorted by name),
// so that goldens stay stable when other stories register more ops.
func coreOps() []Op {
	var out []Op
	for _, op := range All() {
		if slices.Contains(CoreWorkerOpNames, op.Name) {
			out = append(out, op)
		}
	}
	return out
}

func TestCatalogDeclaresExactlyTheElevenWorkerOps(t *testing.T) {
	if len(coreOps()) != 11 {
		t.Fatalf("catalog holds %d core ops", len(coreOps()))
	}
	if len(CoreWorkerOpNames) != 11 || len(expectedCore) != 11 {
		t.Fatalf("core list has %d names, expectation %d", len(CoreWorkerOpNames), len(expectedCore))
	}
	for _, name := range CoreWorkerOpNames {
		want, ok := expectedCore[name]
		if !ok {
			t.Errorf("unexpected core op %s", name)
			continue
		}
		op, ok := Lookup(name)
		if !ok {
			t.Errorf("%s is not registered", name)
			continue
		}
		if op.Scope != ScopeWorker {
			t.Errorf("%s: scope %s", name, op.Scope)
		}
		if op.Actions.String() != want.actions || op.Write != want.write || op.Stream != want.stream {
			t.Errorf("%s: actions=%s write=%v stream=%v", name, op.Actions, op.Write, op.Stream)
		}
		if op.Args.Name() != want.args || op.Result.Name() != want.result {
			t.Errorf("%s: types %s -> %s", name, op.Args.Name(), op.Result.Name())
		}
		var guards []string
		for _, g := range op.Guards {
			guards = append(guards, g.Name+":"+string(g.Class))
		}
		if strings.Join(guards, ",") != want.guards {
			t.Errorf("%s: guards %v", name, guards)
		}
	}
}

func TestWriteOpsCarryRequestIDForceAndWriteResult(t *testing.T) {
	for _, op := range All() {
		if !op.Write {
			continue
		}
		fields := jsonFields(op.Args)
		if fields["request_id"] != reflect.TypeFor[string]() || fields["force"] != reflect.TypeFor[bool]() {
			t.Errorf("%s: arguments lack request_id/force: %v", op.Name, fields)
		}
		if op.Result != reflect.TypeFor[WriteResult]() {
			t.Errorf("%s: result %s", op.Name, op.Result)
		}
		if len(op.Guards) == 0 {
			t.Errorf("%s: no guards", op.Name)
		}
		// force is only meaningful against a FORCE guard or ops with none; never KEEP-only claims.
		for _, g := range op.Guards {
			if !g.Class.Valid() || g.Reads == "" {
				t.Errorf("%s: bad guard %+v", op.Name, g)
			}
		}
	}
}

func TestEveryKeepGuardIsNeverOverridable(t *testing.T) {
	for _, op := range Select(CoreWorkerOpNames...) {
		for _, g := range op.Guards {
			if g.Class == GuardKeep && g.Class.Overridable() {
				t.Errorf("%s.%s is KEEP and overridable", op.Name, g.Name)
			}
		}
	}
}

func TestOnlyMessageAndOperateReachPipeSend(t *testing.T) {
	op, _ := Lookup(OpPipeSend)
	if !op.Actions.Permits(ActionMessage) || !op.Actions.Permits(ActionOperate) || op.Actions.Permits(ActionRead) {
		t.Errorf("pipe_send actions = %s", op.Actions)
	}
}

func TestDerivedTableGolden(t *testing.T) {
	golden(t, "derived.golden", DerivedTable(coreOps()))
}

func TestCatalogTableGolden(t *testing.T) {
	golden(t, "catalog.golden", CatalogTable(coreOps()))
}

func TestDerivedNamesAreDeterministicAcrossSurfaces(t *testing.T) {
	snake := regexp.MustCompile(`^[a-z][a-z0-9]*(_[a-z0-9]+)*$`)
	seen := map[string]bool{}
	for _, op := range All() {
		if !snake.MatchString(op.Name) {
			t.Errorf("%s is not snake_case", op.Name)
		}
		if seen[op.RESTPath()] {
			t.Errorf("duplicate route %s", op.RESTPath())
		}
		seen[op.RESTPath()] = true
		if op.RESTPath() != "/api/v1/ops/"+op.Name || op.WireName() != op.Name || op.MCPTool() != op.Name || op.CLICommand() != op.Name {
			t.Errorf("%s: derived names differ", op.Name)
		}
	}
	// The table does not depend on the registration order: register the core
	// ops in reverse order into a fresh registry and compare.
	core := coreOps()
	r := NewRegistry()
	for i := len(core) - 1; i >= 0; i-- {
		r.Register(core[i])
	}
	if got, want := DerivedTable(r.All()), DerivedTable(core); got != want {
		t.Errorf("DerivedTable depends on registration order:\n%s\nwant:\n%s", got, want)
	}
}

// sample returns a populated value of every argument and result type.
func sampleValue(name string) any {
	id := NewUUIDv7()
	wa := WriteArgs{RequestID: id, Force: true}
	tgt := Target{WorkerID: NewUUIDv7(), SessionName: "minion-3", Pane: "0.1"}
	pane := PaneInfo{Session: "minion-3", ID: "%3", Window: 0, Index: 1, Command: "claude", Kind: "agent:claude", State: PaneStateDialog,
		PID: 4242, SessionCreated: 1794308400, DialogDigest: strings.Repeat("ab", 32)}
	at := Timestamp(time.Date(2026, 11, 9, 8, 0, 0, 123_000_000, time.UTC))
	screen := Screen{Width: 80, Height: 2, Lines: []string{"hello", "world"},
		Spans: []StyleSpan{{Line: 0, Start: 0, End: 5, FG: "p1", Bold: true}}, Cursor: &Cursor{Row: 1, Col: 5}}
	turn := PipeTurn{TurnID: NewUUIDv7(), Session: "minion-3", Direction: PipeReceived, Text: "done", At: at}
	wr := WriteResult{Outcome: OutcomePartial, Reason: "pane_changed", Warnings: []Warning{{Code: "readiness_unknown", Message: "state unknown"}}, Pane: &pane}
	switch name {
	case "SessionsListArgs":
		return SessionsListArgs{}
	case "SessionsListResult":
		return SessionsListResult{Sessions: []SessionInfo{{Name: "minion-3", Created: at, Windows: 2, Attached: true, Task: "fix build", Pane: &pane}}}
	case "SessionCreateArgs":
		return SessionCreateArgs{WriteArgs: wa, Name: "work", Cwd: "/srv/app", Argv: []string{"claude", "--resume"}}
	case "SessionSnapshotArgs":
		return SessionSnapshotArgs{Target: tgt}
	case "SessionSnapshotResult":
		return SessionSnapshotResult{Pane: pane, Screen: screen}
	case "SessionCaptureArgs":
		return SessionCaptureArgs{Target: tgt, Lines: 500}
	case "SessionCaptureResult":
		return SessionCaptureResult{Pane: pane, Screen: screen}
	case "SendTextArgs":
		return SendTextArgs{WriteArgs: wa, Target: tgt, Text: "line1\nline2 \"quoted\" \u65e5\u672c", Submit: true}
	case "SendKeysArgs":
		return SendKeysArgs{WriteArgs: wa, Target: tgt, Keys: []string{"C-c", "Enter"}}
	case "SetTaskArgs":
		return SetTaskArgs{WriteArgs: wa, Target: tgt, Task: "review PR 12"}
	case "AnswerArgs":
		return AnswerArgs{WriteArgs: wa, Target: tgt, Choice: "yes", Expect: goodReviewed}
	case "PipeSendArgs":
		return PipeSendArgs{WriteArgs: wa, Target: tgt, Text: "hello agent"}
	case "PipeReadArgs":
		return PipeReadArgs{Target: tgt, AfterTurnID: id, Limit: 20}
	case "PipeReadResult":
		return PipeReadResult{Turns: []PipeTurn{turn}}
	case "PipeWaitArgs":
		return PipeWaitArgs{Target: tgt, AfterTurnID: id, TimeoutMS: 30000}
	case "PipeTurn":
		return turn
	case "WriteResult":
		return wr
	}
	return nil
}

func TestJSONRoundTripOfEveryArgAndResultType(t *testing.T) {
	for _, op := range Select(CoreWorkerOpNames...) {
		for _, typ := range []reflect.Type{op.Args, op.Result} {
			t.Run(op.Name+"/"+typ.Name(), func(t *testing.T) {
				in := sampleValue(typ.Name())
				if in == nil {
					t.Fatalf("no sample for %s", typ.Name())
				}
				if reflect.TypeOf(in) != typ {
					t.Fatalf("sample type %T, want %s", in, typ)
				}
				b, err := json.Marshal(in)
				if err != nil {
					t.Fatal(err)
				}
				// Strict decode into a fresh value: no unknown fields.
				ptr := reflect.New(typ)
				dec := json.NewDecoder(bytes.NewReader(b))
				dec.DisallowUnknownFields()
				if err := dec.Decode(ptr.Interface()); err != nil {
					t.Fatalf("decode %s: %v", b, err)
				}
				if !reflect.DeepEqual(ptr.Elem().Interface(), in) {
					t.Fatalf("round trip differs:\n got %+v\nwant %+v", ptr.Elem().Interface(), in)
				}
				// Re-encoding is byte-identical.
				b2, err := json.Marshal(ptr.Elem().Interface())
				if err != nil || !bytes.Equal(b, b2) {
					t.Fatalf("re-encode differs: %s vs %s (%v)", b, b2, err)
				}
				if v, ok := in.(Validator); ok {
					if err := v.Validate(); err != nil {
						t.Fatalf("sample does not validate: %v", err)
					}
				}
			})
		}
	}
}

// Every JSON member of every argument and result type is snake_case.
func TestAllJSONMembersAreSnakeCase(t *testing.T) {
	snake := regexp.MustCompile(`^[a-z][a-z0-9]*(_[a-z0-9]+)*$`)
	var walk func(t reflect.Type, path string, seen map[reflect.Type]bool)
	walk = func(typ reflect.Type, path string, seen map[reflect.Type]bool) {
		for typ.Kind() == reflect.Pointer || typ.Kind() == reflect.Slice {
			typ = typ.Elem()
		}
		if typ.Kind() != reflect.Struct || seen[typ] || typ == reflect.TypeFor[Timestamp]() {
			return
		}
		seen[typ] = true
		for i := 0; i < typ.NumField(); i++ {
			f := typ.Field(i)
			name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
			if f.Anonymous && name == "" {
				walk(f.Type, path, seen)
				continue
			}
			if name == "" || !snake.MatchString(name) {
				t.Errorf("%s.%s: json name %q", path, f.Name, name)
			}
			walk(f.Type, path+"."+f.Name, seen)
		}
	}
	for _, op := range All() {
		walk(op.Args, op.Name+".args", map[reflect.Type]bool{})
		walk(op.Result, op.Name+".result", map[reflect.Type]bool{})
	}
}

func TestArgumentValidation(t *testing.T) {
	good := Target{WorkerID: NewUUIDv7(), SessionName: "minion-1"}
	wa := WriteArgs{RequestID: NewUUIDv7()}
	bad := Target{SessionName: "a:b"}
	long := strings.Repeat("x", MaxTaskLen+1)
	tests := []struct {
		name    string
		v       Validator
		wantErr bool
	}{
		{"snapshot ok", SessionSnapshotArgs{good}, false},
		{"snapshot bad target", SessionSnapshotArgs{bad}, true},
		{"capture default", SessionCaptureArgs{Target: good}, false},
		{"capture max", SessionCaptureArgs{Target: good, Lines: MaxCaptureLines}, false},
		{"capture too many", SessionCaptureArgs{Target: good, Lines: MaxCaptureLines + 1}, true},
		{"capture negative", SessionCaptureArgs{Target: good, Lines: -1}, true},
		{"send_text ok", SendTextArgs{WriteArgs: wa, Target: good, Text: "x\n"}, false},
		{"send_text no request id", SendTextArgs{Target: good, Text: "x"}, true},
		{"send_text empty", SendTextArgs{WriteArgs: wa, Target: good}, true},
		{"send_text invalid utf8", SendTextArgs{WriteArgs: wa, Target: good, Text: "a\xff"}, true},
		{"send_keys ok", SendKeysArgs{WriteArgs: wa, Target: good, Keys: []string{"Enter"}}, false},
		{"send_keys none", SendKeysArgs{WriteArgs: wa, Target: good}, true},
		{"send_keys empty key", SendKeysArgs{WriteArgs: wa, Target: good, Keys: []string{""}}, true},
		{"send_keys 32 byte word", SendKeysArgs{WriteArgs: wa, Target: good, Keys: []string{strings.Repeat("k", MaxKeyLen)}}, true},
		{"send_keys 33 bytes", SendKeysArgs{WriteArgs: wa, Target: good, Keys: []string{strings.Repeat("k", MaxKeyLen+1)}}, true},
		{"send_keys free text", SendKeysArgs{WriteArgs: wa, Target: good, Keys: []string{"my secret password text that is long", "\x1b[200~"}}, true},
		{"send_keys word", SendKeysArgs{WriteArgs: wa, Target: good, Keys: []string{"abc"}}, true},
		{"send_keys phrase", SendKeysArgs{WriteArgs: wa, Target: good, Keys: []string{"my password"}}, true},
		{"send_keys single x", SendKeysArgs{WriteArgs: wa, Target: good, Keys: []string{"x"}}, false},
		{"send_keys modifier on word", SendKeysArgs{WriteArgs: wa, Target: good, Keys: []string{"C-abc"}}, true},
		{"send_keys bare modifier", SendKeysArgs{WriteArgs: wa, Target: good, Keys: []string{"C-"}}, true},
		{"send_keys M-F12", SendKeysArgs{WriteArgs: wa, Target: good, Keys: []string{"M-F12"}}, false},
		{"send_keys F13", SendKeysArgs{WriteArgs: wa, Target: good, Keys: []string{"F13"}}, true},
		{"send_keys escape", SendKeysArgs{WriteArgs: wa, Target: good, Keys: []string{"\x1b[200~"}}, true},
		{"send_keys newline", SendKeysArgs{WriteArgs: wa, Target: good, Keys: []string{"a\nb"}}, true},
		{"send_keys C-c", SendKeysArgs{WriteArgs: wa, Target: good, Keys: []string{"C-c"}}, false},
		{"send_keys longest name", SendKeysArgs{WriteArgs: wa, Target: good, Keys: []string{"C-M-S-PageDown"}}, false},
		{"send_keys unicode", SendKeysArgs{WriteArgs: wa, Target: good, Keys: []string{"\u00e9"}}, false},
		{"send_keys too many", SendKeysArgs{WriteArgs: wa, Target: good, Keys: make([]string, MaxSendKeys+1)}, true},
		{"set_task ok", SetTaskArgs{WriteArgs: wa, Target: good, Task: "t"}, false},
		{"set_task clear", SetTaskArgs{WriteArgs: wa, Target: good}, false},
		{"set_task long", SetTaskArgs{WriteArgs: wa, Target: good, Task: long}, true},
		{"answer ok", AnswerArgs{WriteArgs: wa, Target: good, Choice: "1", Expect: goodReviewed}, false},
		{"answer empty", AnswerArgs{WriteArgs: wa, Target: good, Expect: goodReviewed}, true},
		{"answer unbound", AnswerArgs{WriteArgs: wa, Target: good, Choice: "1"}, true},
		{"send_text control", SendTextArgs{WriteArgs: wa, Target: good, Text: "a\rb"}, true},
		{"send_text tab and lf", SendTextArgs{WriteArgs: wa, Target: good, Text: "a\tb\nc"}, false},
		{"send_text submit", SendTextArgs{WriteArgs: wa, Target: good, Text: "x", Submit: true}, false},
		{"pipe_send ok", PipeSendArgs{WriteArgs: wa, Target: good, Text: "hi"}, false},
		{"pipe_send bad id", PipeSendArgs{WriteArgs: WriteArgs{RequestID: "x"}, Target: good, Text: "hi"}, true},
		{"pipe_read ok", PipeReadArgs{Target: good, AfterTurnID: NewUUIDv7(), Limit: 5}, false},
		{"pipe_read bad cursor", PipeReadArgs{Target: good, AfterTurnID: "abc"}, true},
		{"pipe_read limit", PipeReadArgs{Target: good, Limit: MaxPipeReadLimit + 1}, true},
		{"pipe_wait ok", PipeWaitArgs{Target: good, TimeoutMS: 1000}, false},
		{"pipe_wait timeout", PipeWaitArgs{Target: good, TimeoutMS: MaxPipeWaitMS + 1}, true},
		{"pipe_wait negative", PipeWaitArgs{Target: good, TimeoutMS: -1}, true},
		{"create default", SessionCreateArgs{WriteArgs: wa}, false},
		{"create full", SessionCreateArgs{WriteArgs: wa, Name: "work", Cwd: "/tmp", Argv: []string{"sh", "-c", "x"}}, false},
		{"create bad name", SessionCreateArgs{WriteArgs: wa, Name: "a.b"}, true},
		{"create empty program", SessionCreateArgs{WriteArgs: wa, Argv: []string{""}}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.v.Validate()
			if (err != nil) != tc.wantErr {
				t.Fatalf("Validate() = %v, wantErr %v", err, tc.wantErr)
			}
			if err != nil && !errors.Is(err, ErrInvalidArgument) {
				t.Fatalf("error does not match ErrInvalidArgument: %v", err)
			}
		})
	}
}

func TestCaptureEffectiveLines(t *testing.T) {
	if got := (SessionCaptureArgs{}).EffectiveLines(); got != DefaultCaptureLines {
		t.Errorf("default = %d", got)
	}
	if got := (SessionCaptureArgs{Lines: 7}).EffectiveLines(); got != 7 {
		t.Errorf("explicit = %d", got)
	}
}

func TestWriteArgsJSONFlattened(t *testing.T) {
	b, _ := json.Marshal(SendTextArgs{WriteArgs: WriteArgs{RequestID: "r", Force: true}, Text: "t"})
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if m["request_id"] != "r" || m["force"] != true {
		t.Fatalf("request_id/force not at top level: %s", b)
	}
}

// goodReviewed is a complete binding of an answer.
var goodReviewed = ReviewedDialog{PaneID: "%3", PID: 4242, Command: "claude", SessionCreated: 1794308400, DialogDigest: strings.Repeat("ab", 32)}

func TestPlainTextAndCheckPasteText(t *testing.T) {
	for text, plain := range map[string]bool{"hello": true, "a b-c": true, "\u00fcn\u00ef \u2713": true, "": true, "a\nb": false, "a\tb": false, "a\rb": false, "\x1b": false, "a\u0085": false, "\x7f": false} {
		if got := PlainText(text); got != plain {
			t.Errorf("PlainText(%q) = %v, want %v", text, got, plain)
		}
	}
	for _, ok := range []string{"x", "a\nb", "a\tb"} {
		if err := CheckPasteText(ok); err != nil {
			t.Errorf("CheckPasteText(%q) = %v", ok, err)
		}
	}
	for _, bad := range []string{"", "a\rb", "\x1b[201~", "\x00", "\u009b"} {
		if err := CheckPasteText(bad); !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("CheckPasteText(%q) = %v, want ErrInvalidArgument", bad, err)
		}
	}
}

func TestHiddenMember(t *testing.T) {
	if !HiddenMember(OpPipeSend, "message_id") || HiddenMember(OpPipeSend, "text") || HiddenMember(OpSendText, "message_id") {
		t.Error("HiddenMember is wrong")
	}
}

func TestRequiredFeaturesAreOneRuleForTheHubAndTheLocalClients(t *testing.T) {
	type tc struct {
		op, args string
		hub, loc []string
	}
	for _, c := range []tc{
		{OpAnswer, `{}`, []string{proto.FeatureAnswerReviewed}, []string{proto.FeatureAnswerReviewed}},
		{OpSendText, `{"text":"plain"}`, nil, nil},
		{OpSendText, `{"text":"plain","submit":true}`, []string{proto.FeatureSendTextSubmit}, []string{proto.FeatureSendTextSubmit}},
		{OpSendText, `{"text":"a\nb"}`, []string{proto.FeatureTextVerbatim}, []string{proto.FeatureTextVerbatim}},
		{OpSendText, `[1]`, []string{proto.FeatureSendTextSubmit, proto.FeatureTextVerbatim}, []string{proto.FeatureSendTextSubmit, proto.FeatureTextVerbatim}},
		{OpPipeSend, `{"text":"plain"}`, []string{proto.FeaturePipeCausal}, nil},
		{OpPipeSend, `{"text":"a\tb"}`, []string{proto.FeaturePipeCausal, proto.FeatureTextVerbatim}, []string{proto.FeatureTextVerbatim}},
		{OpSendKeys, `{}`, nil, nil},
		{OpSessionSnapshot, `{}`, nil, nil},
	} {
		if got := RequiredFeatures(c.op, json.RawMessage(c.args)); strings.Join(got, ",") != strings.Join(c.hub, ",") {
			t.Errorf("%s %s: hub %v, want %v", c.op, c.args, got, c.hub)
		}
		if got := LocalRequiredFeatures(c.op, json.RawMessage(c.args)); strings.Join(got, ",") != strings.Join(c.loc, ",") {
			t.Errorf("%s %s: local %v, want %v", c.op, c.args, got, c.loc)
		}
	}
}
