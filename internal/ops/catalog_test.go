package ops

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// writeArgs is the argument type of valid test write ops.
type testWriteArgs struct {
	WriteArgs
	Target Target `json:"target"`
}

type testReadArgs struct{}
type testReadResult struct{}

func goodRead(name string) Op {
	return Op{
		Name: name, Summary: "Test read.", Scope: ScopeWorker, Actions: MustActionSet(ActionRead),
		Args: reflect.TypeFor[testReadArgs](), Result: reflect.TypeFor[testReadResult](),
	}
}

func goodWrite(name string) Op {
	return Op{
		Name: name, Summary: "Test write.", Scope: ScopeWorker, Actions: MustActionSet(ActionOperate), Write: true,
		Guards: []Guard{{Name: "target_recheck", Class: GuardKeep, Reads: "pane.identity"}},
		Args:   reflect.TypeFor[testWriteArgs](), Result: reflect.TypeFor[WriteResult](),
	}
}

func mustPanic(t *testing.T, wantSubstr string, f func()) {
	t.Helper()
	defer func() {
		t.Helper()
		r := recover()
		if r == nil {
			t.Fatalf("no panic, want one containing %q", wantSubstr)
		}
		msg, _ := r.(string)
		if !strings.Contains(msg, wantSubstr) {
			t.Fatalf("panic = %q, want substring %q", msg, wantSubstr)
		}
	}()
	f()
}

func TestRegisterAcceptsValidOps(t *testing.T) {
	r := NewRegistry()
	r.Register(goodRead("things_list"))
	r.Register(goodWrite("thing_poke"))
	if _, ok := r.Lookup("thing_poke"); !ok {
		t.Fatal("Lookup failed")
	}
	if _, ok := r.Lookup("nope"); ok {
		t.Fatal("Lookup found a missing op")
	}
	if got := len(r.All()); got != 2 {
		t.Fatalf("All() = %d ops", got)
	}
}

func TestRegisterPanicsOnDuplicate(t *testing.T) {
	r := NewRegistry()
	r.Register(goodRead("things_list"))
	mustPanic(t, "already registered", func() { r.Register(goodRead("things_list")) })
}

func TestRegisterPanicsOnBadName(t *testing.T) {
	for _, name := range []string{"", "Things", "things-list", "things list", "_things", "things_", "things__list", "1things", "thingsList", "th\u00eengs"} {
		r := NewRegistry()
		mustPanic(t, "snake_case", func() { r.Register(goodRead(name)) })
	}
	r := NewRegistry()
	for _, name := range []string{"a", "a1", "a_b", "send_text2", "a_1"} {
		r.Register(goodRead(name)) // must not panic
	}
}

func TestRegisterPanicsOnWriteWithoutGuards(t *testing.T) {
	r := NewRegistry()
	op := goodWrite("thing_poke")
	op.Guards = nil
	mustPanic(t, "guard list", func() { r.Register(op) })
	op.Guards = []Guard{}
	mustPanic(t, "guard list", func() { r.Register(op) })
}

func TestRegisterPanicsOnBadGuards(t *testing.T) {
	r := NewRegistry()
	tests := map[string]Guard{
		"unknown class": {Name: "g", Class: "REMOVE", Reads: "pane.state"},
		"empty class":   {Name: "g", Reads: "pane.state"},
		"bad name":      {Name: "Not Snake", Class: GuardKeep, Reads: "pane.state"},
		"no state":      {Name: "g", Class: GuardWarn},
	}
	for name, g := range tests {
		t.Run(name, func(t *testing.T) {
			op := goodWrite("thing_poke")
			op.Guards = []Guard{g}
			mustPanic(t, "guard", func() { r.Register(op) })
		})
	}
	op := goodWrite("thing_poke")
	op.Guards = []Guard{op.Guards[0], op.Guards[0]}
	mustPanic(t, "duplicate guard", func() { r.Register(op) })
}

func TestRegisterPanicsOnGuardsOnRead(t *testing.T) {
	op := goodRead("things_list")
	op.Guards = []Guard{{Name: "g", Class: GuardKeep, Reads: "x"}}
	mustPanic(t, "only write ops", func() { NewRegistry().Register(op) })
}

func TestRegisterPanicsOnWriteContract(t *testing.T) {
	type noForce struct {
		RequestID string `json:"request_id"`
	}
	type noRequestID struct {
		Force bool `json:"force"`
	}
	type wrongTypes struct {
		RequestID int    `json:"request_id"`
		Force     string `json:"force"`
	}
	type renamed struct {
		RequestID string `json:"id"`
		Force     bool   `json:"force"`
	}
	for name, args := range map[string]reflect.Type{
		"no force":      reflect.TypeFor[noForce](),
		"no request_id": reflect.TypeFor[noRequestID](),
		"wrong types":   reflect.TypeFor[wrongTypes](),
		"renamed":       reflect.TypeFor[renamed](),
		"read args":     reflect.TypeFor[testReadArgs](),
	} {
		t.Run(name, func(t *testing.T) {
			op := goodWrite("thing_poke")
			op.Args = args
			mustPanic(t, "write op", func() { NewRegistry().Register(op) })
		})
	}
	op := goodWrite("thing_poke")
	op.Result = reflect.TypeFor[testReadResult]()
	mustPanic(t, "WriteResult", func() { NewRegistry().Register(op) })
}

func TestRegisterPanicsOnOtherBadDeclarations(t *testing.T) {
	tests := map[string]func(*Op){
		"no summary":      func(o *Op) { o.Summary = "" },
		"no scope":        func(o *Op) { o.Scope = "" },
		"no actions":      func(o *Op) { o.Actions = nil },
		"unknown action":  func(o *Op) { o.Actions = ActionSet{"admin"} },
		"unsorted action": func(o *Op) { o.Actions = ActionSet{ActionOperate, ActionMessage} },
		"nil args":        func(o *Op) { o.Args = nil },
		"non-struct args": func(o *Op) { o.Args = reflect.TypeFor[string]() },
		"nil result":      func(o *Op) { o.Result = nil },
		"pointer result":  func(o *Op) { o.Result = reflect.TypeFor[*testReadResult]() },
		"stream write":    func(o *Op) { *o = goodWrite("x_y"); o.Stream = true },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			op := goodRead("things_list")
			mutate(&op)
			mustPanic(t, "ops: Register", func() { NewRegistry().Register(op) })
		})
	}
}

func TestAllIsSortedWhateverTheRegistrationOrder(t *testing.T) {
	names := []string{"c_op", "a_op", "b_op"}
	r := NewRegistry()
	for _, n := range names {
		r.Register(goodRead(n))
	}
	var got []string
	for _, op := range r.All() {
		got = append(got, op.Name)
	}
	if want := []string{"a_op", "b_op", "c_op"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("All() = %v, want %v", got, want)
	}
}

func TestRegisterClonesSlices(t *testing.T) {
	r := NewRegistry()
	op := goodWrite("thing_poke")
	r.Register(op)
	op.Guards[0].Class = GuardWarn
	op.Actions[0] = ActionRead
	got, _ := r.Lookup("thing_poke")
	if got.Guards[0].Class != GuardKeep || got.Actions[0] != ActionOperate {
		t.Fatal("registry shares memory with the caller's declaration")
	}
}

func TestLookupAndAllReturnIsolatedCopies(t *testing.T) {
	r := NewRegistry()
	r.Register(goodWrite("thing_poke"))
	check := func(via string) {
		t.Helper()
		got, _ := r.Lookup("thing_poke")
		if got.Guards[0].Class != GuardKeep || got.Actions[0] != ActionOperate {
			t.Fatalf("mutation through %s reached the registry", via)
		}
	}
	got, _ := r.Lookup("thing_poke")
	got.Guards[0].Class = GuardWarn
	got.Actions[0] = ActionRead
	check("Lookup")
	all := r.All()
	all[0].Guards[0].Class = GuardWarn
	all[0].Actions[0] = ActionRead
	check("All")
}

func TestRegisterPanicsOnAmbiguousEmbeddedFields(t *testing.T) {
	// Built with reflect.StructOf: a static declaration would be rejected by
	// go vet's structtag check.
	wa := reflect.TypeFor[struct {
		RequestID string `json:"request_id"`
		Force     bool   `json:"force"`
	}]()
	ambiguous := reflect.StructOf([]reflect.StructField{
		{Name: "A", Type: wa, Anonymous: true},
		{Name: "B", Type: wa, Anonymous: true},
		{Name: "Target", Type: reflect.TypeFor[Target](), Tag: `json:"target"`},
	})
	// encoding/json drops both conflicting members.
	b, err := json.Marshal(reflect.New(ambiguous).Interface())
	if err != nil || strings.Contains(string(b), "request_id") || strings.Contains(string(b), "force") {
		t.Fatalf("encoding/json premise changed: %s %v", b, err)
	}
	op := goodWrite("thing_poke")
	op.Args = ambiguous
	mustPanic(t, "write op", func() { NewRegistry().Register(op) })

	// A shallower declaration dominates a deeper one.
	type shallow struct {
		WriteArgs
		RequestID string `json:"request_id"`
	}
	f := jsonFields(reflect.TypeFor[shallow]())
	if f["request_id"] != reflect.TypeFor[string]() || f["force"] != reflect.TypeFor[bool]() {
		t.Errorf("fields = %v", f)
	}
}

func TestDerivedNamesAndMethods(t *testing.T) {
	r := goodRead("things_list")
	w := goodWrite("thing_poke")
	if r.RESTPath() != "/api/v1/ops/things_list" || r.RESTRoute() != "POST /api/v1/ops/things_list" ||
		r.WireName() != "things_list" || r.MCPTool() != "things_list" || r.CLICommand() != "things_list" {
		t.Fatalf("derived names wrong: %+v", r)
	}
	if got := r.RESTMethods(); !reflect.DeepEqual(got, []string{"POST", "GET"}) {
		t.Errorf("read methods = %v", got)
	}
	if got := w.RESTMethods(); !reflect.DeepEqual(got, []string{"POST"}) {
		t.Errorf("write methods = %v", got)
	}
}

func TestNewArgsAndResult(t *testing.T) {
	op := goodWrite("thing_poke")
	if _, ok := op.NewArgs().(*testWriteArgs); !ok {
		t.Errorf("NewArgs type = %T", op.NewArgs())
	}
	if _, ok := op.NewResult().(*WriteResult); !ok {
		t.Errorf("NewResult type = %T", op.NewResult())
	}
}

func TestSelectPanicsOnUnknown(t *testing.T) {
	mustPanic(t, "unknown op", func() { Select("no_such_op") })
}

func TestJSONFieldsFollowsEmbedding(t *testing.T) {
	fields := jsonFields(reflect.TypeFor[SendTextArgs]())
	for _, want := range []string{"request_id", "force", "target", "text", "submit"} {
		if _, ok := fields[want]; !ok {
			t.Errorf("field %s missing from %v", want, fields)
		}
	}
	if len(fields) != 5 {
		t.Errorf("fields = %v", fields)
	}
}
