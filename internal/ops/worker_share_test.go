package ops

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

const shareOpsWorker = "01900000-0000-7000-8000-000000000001"

func TestShareOpsAreWorkerOpsThatNeedOperate(t *testing.T) {
	for _, name := range ShareWorkerOpNames {
		op, ok := Lookup(name)
		if !ok {
			t.Fatalf("%s is not in the catalog", name)
		}
		if op.Scope != ScopeWorker || !op.Actions.Permits(ActionOperate) || op.Actions.Permits(ActionRead) || op.Actions.Permits(ActionManage) || op.Actions.Permits(ActionMessage) {
			t.Errorf("%s: scope %s, actions %v (A-8: operate on the Worker, not manage)", name, op.Scope, op.Actions)
		}
		if op.Result != reflect.TypeFor[ShareResult]() || op.Stream {
			t.Errorf("%s: result %v", name, op.Result)
		}
	}
	get, _ := Lookup(OpShareGet)
	set, _ := Lookup(OpShareSet)
	if get.Write || get.Mutates || !set.Mutates || set.Write {
		t.Errorf("share_get read-only, share_set Mutates: %+v %+v", get, set)
	}
	if got := set.RESTMethods(); len(got) != 1 || got[0] != "POST" {
		t.Errorf("share_set methods %v: a state change is POST only", got)
	}
	for _, core := range CoreWorkerOpNames {
		if core == OpShareGet || core == OpShareSet {
			t.Errorf("%s joined the fixed core list", core)
		}
	}
	if len(CoreWorkerOpNames) != 11 {
		t.Errorf("core ops = %d", len(CoreWorkerOpNames))
	}
}

func TestShareSetArgsValidate(t *testing.T) {
	ok := func(a ShareSetArgs) {
		t.Helper()
		if err := a.Validate(); err != nil {
			t.Errorf("%+v: %v", a, err)
		}
	}
	bad := func(a ShareSetArgs, field string) {
		t.Helper()
		err := a.Validate()
		if err == nil || !strings.Contains(err.Error(), field) {
			t.Errorf("%+v: err = %v, want it to name %q", a, err, field)
		}
	}
	ok(ShareSetArgs{WorkerID: shareOpsWorker, Mode: "all"})
	ok(ShareSetArgs{WorkerID: shareOpsWorker, Mode: "none"})
	ok(ShareSetArgs{WorkerID: shareOpsWorker, Mode: "select", Names: []string{"a", "my session"}})
	bad(ShareSetArgs{Mode: "all"}, "worker_id")
	bad(ShareSetArgs{WorkerID: "x", Mode: "all"}, "worker_id")
	bad(ShareSetArgs{WorkerID: shareOpsWorker}, "mode")
	bad(ShareSetArgs{WorkerID: shareOpsWorker, Mode: "later"}, "mode")
	bad(ShareSetArgs{WorkerID: shareOpsWorker, Mode: "select"}, "names")
	bad(ShareSetArgs{WorkerID: shareOpsWorker, Mode: "all", Names: []string{"a"}}, "names")
	bad(ShareSetArgs{WorkerID: shareOpsWorker, Mode: "none", Names: []string{"a"}}, "names")
	bad(ShareSetArgs{WorkerID: shareOpsWorker, Mode: "select", Names: []string{"a:b"}}, "names")
	bad(ShareSetArgs{WorkerID: shareOpsWorker, Mode: "select", Names: []string{"a\nb"}}, "names")
	bad(ShareSetArgs{WorkerID: shareOpsWorker, Mode: "select", Names: []string{""}}, "names")
	many := make([]string, MaxShareNames+1)
	for i := range many {
		many[i] = "s"
	}
	bad(ShareSetArgs{WorkerID: shareOpsWorker, Mode: "select", Names: many}, "names")
	if err := (ShareGetArgs{WorkerID: "nope"}).Validate(); err == nil {
		t.Error("share_get took a bad worker id")
	}
	if err := (ShareGetArgs{WorkerID: shareOpsWorker}).Validate(); err != nil {
		t.Error(err)
	}
}

func TestShareResultIsNamesOnlyAndNormalized(t *testing.T) {
	r := ShareResult{Rule: ShareRule{Mode: "select", Names: []string{"b", "a", "a"}}, Unshared: []string{"z", "y", "y"}}
	r.Normalize()
	if !reflect.DeepEqual(r.Unshared, []string{"y", "z"}) || !reflect.DeepEqual(r.Rule.Names, []string{"a", "b"}) {
		t.Fatalf("%+v", r)
	}
	empty := ShareResult{Rule: ShareRule{Mode: "all"}}
	empty.Normalize()
	b, _ := json.Marshal(empty)
	if string(b) != `{"rule":{"mode":"all"},"unshared":[]}` {
		t.Fatalf("empty result = %s (unshared is a list, never null)", b)
	}
	// Names only: every member of the result is a string, a list of strings or the rule.
	var check func(reflect.Type, string)
	check = func(tp reflect.Type, path string) {
		switch tp.Kind() {
		case reflect.String, reflect.Bool:
		case reflect.Slice:
			check(tp.Elem(), path+"[]")
		case reflect.Struct:
			for i := 0; i < tp.NumField(); i++ {
				check(tp.Field(i).Type, path+"."+tp.Field(i).Name)
			}
		default:
			t.Errorf("%s is a %s: a result that is names only has no such member", path, tp.Kind())
		}
	}
	check(reflect.TypeFor[ShareResult](), "ShareResult")
}
