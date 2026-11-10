package agent

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/worker/store"
)

const shareWorkerID = "01900000-0000-7000-8000-000000000001"

func shareGetArgs() ops.ShareGetArgs { return ops.ShareGetArgs{WorkerID: shareWorkerID} }

func shareSetArgs(mode string, names ...string) ops.ShareSetArgs {
	return ops.ShareSetArgs{WorkerID: shareWorkerID, Mode: mode, Names: names}
}

func (e *env) share(op string, actions []ops.Action, args any) (ops.ShareResult, error) {
	e.t.Helper()
	raw, err := e.call(op, actions, args)
	if err != nil {
		return ops.ShareResult{}, err
	}
	var res ops.ShareResult
	if err := json.Unmarshal(raw, &res); err != nil {
		e.t.Fatalf("result %s is not a ShareResult: %v", raw, err)
	}
	return res, nil
}

func (e *env) mustShare(op string, args any) ops.ShareResult {
	e.t.Helper()
	res, err := e.share(op, operateActions, args)
	if err != nil {
		e.t.Fatalf("%s: %v", op, err)
	}
	return res
}

func (e *env) saved() store.ShareSelection {
	e.t.Helper()
	sel, err := e.st.LoadShare()
	if err != nil {
		e.t.Fatal(err)
	}
	return sel
}

func shareEnv(t *testing.T, sel store.ShareSelection, sessions ...string) *env {
	// The Worker of these tests has the id the share arguments name: an
	// operation that names another Worker is refused (see requireSelf).
	e := newEnv(t)
	e.policy.SetWorkerID(shareWorkerID)
	for _, s := range sessions {
		e.host.addSession(s, "bash")
	}
	if err := e.st.SaveShare(sel); err != nil {
		t.Fatal(err)
	}
	return e
}

// share_get returns the rule and only the NAMES of the sessions the rule does
// not share, in every mode.
func TestShareGetReportsTheRuleAndTheUnsharedNamesOnly(t *testing.T) {
	for _, tc := range []struct {
		name         string
		sel          store.ShareSelection
		wantRule     ops.ShareRule
		wantUnshared []string
	}{
		{"all", store.ShareSelection{Mode: store.ShareAll}, ops.ShareRule{Mode: "all"}, []string{}},
		{"none", store.ShareSelection{Mode: store.ShareNone}, ops.ShareRule{Mode: "none"}, []string{"a", "b", "c"}},
		{"later", store.ShareSelection{Mode: store.ShareLater}, ops.ShareRule{Mode: "later"}, []string{"a", "b", "c"}},
		{"select", store.ShareSelection{Mode: store.ShareSelect, Sessions: []string{"b", "gone"}},
			ops.ShareRule{Mode: "select", Names: []string{"b", "gone"}}, []string{"a", "c"}},
		{"none keeps the list out of the rule", store.ShareSelection{Mode: store.ShareNone, Sessions: []string{"a"}},
			ops.ShareRule{Mode: "none"}, []string{"a", "b", "c"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := shareEnv(t, tc.sel, "c", "a", "b")
			res := e.mustShare(ops.OpShareGet, shareGetArgs())
			if !reflect.DeepEqual(res.Rule, tc.wantRule) || !reflect.DeepEqual(res.Unshared, tc.wantUnshared) {
				t.Fatalf("share_get = %+v, want rule %+v unshared %v", res, tc.wantRule, tc.wantUnshared)
			}
		})
	}
}

// The result type holds names and the rule, nothing else: a screen, pane,
// task or turn cannot be in it, and a session that holds secrets is still only
// a name.
func TestShareResultCarriesNoContentAndNothingLeaksIntoTheBytes(t *testing.T) {
	e := shareEnv(t, store.ShareSelection{Mode: store.ShareNone}, "private")
	ref := e.host.addSession("secret-work", "bash")
	e.host.setScreen(ref, "TOP-SECRET-SCREEN-TEXT")
	if err := e.host.SetOption(context.Background(), "secret-work", "@minion_task", "TOP-SECRET-TASK"); err != nil {
		t.Fatal(err)
	}
	raw, err := e.call(ops.OpShareGet, operateActions, shareGetArgs())
	if err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{"TOP-SECRET-SCREEN-TEXT", "TOP-SECRET-TASK", "pane", "screen", "task", "bash"} {
		if strings.Contains(string(raw), leak) {
			t.Fatalf("share_get leaked %q: %s", leak, raw)
		}
	}
	var generic map[string]any
	_ = json.Unmarshal(raw, &generic)
	for k := range generic {
		if k != "rule" && k != "unshared" && k != "locked" {
			t.Fatalf("unexpected member %q in %s", k, raw)
		}
	}
	rt := reflect.TypeFor[ops.ShareResult]()
	for i := 0; i < rt.NumField(); i++ {
		if k := rt.Field(i).Type.Kind(); k == reflect.Map || k == reflect.Interface {
			t.Fatalf("ShareResult.%s can hold anything", rt.Field(i).Name)
		}
	}
}

// share_set takes effect for the next read of every reader and is durable.
func TestShareSetAppliesAtomicallyAndDurably(t *testing.T) {
	e := shareEnv(t, store.ShareSelection{Mode: store.ShareNone}, "a", "b")
	ctx := t.Context()
	if _, err := e.ex.Execute(ctx, Request{Op: ops.OpSessionSnapshot, Actions: allActions, Args: mustJSON(t, ops.SessionSnapshotArgs{Target: target("a")})}); err == nil {
		t.Fatal("an unshared session answered a snapshot")
	}

	res := e.mustShare(ops.OpShareSet, shareSetArgs("select", "b", "a", "a"))
	if res.Rule.Mode != "select" || !reflect.DeepEqual(res.Rule.Names, []string{"a", "b"}) || len(res.Unshared) != 0 {
		t.Fatalf("share_set = %+v", res)
	}
	if got := e.saved(); got.Mode != store.ShareSelect || !reflect.DeepEqual(got.Sessions, []string{"b", "a"}) {
		t.Fatalf("saved %+v", got)
	}
	if _, err := e.ex.Execute(ctx, Request{Op: ops.OpSessionSnapshot, Actions: allActions, Args: mustJSON(t, ops.SessionSnapshotArgs{Target: target("a")})}); err != nil {
		t.Fatalf("a shared session refused a snapshot: %v", err)
	}

	// Unshare one of them: the next snapshot, capture and list stop at once.
	res = e.mustShare(ops.OpShareSet, shareSetArgs("select", "b"))
	if !reflect.DeepEqual(res.Unshared, []string{"a"}) {
		t.Fatalf("unshared = %v", res.Unshared)
	}
	for _, op := range []string{ops.OpSessionSnapshot, ops.OpSessionCapture} {
		var args any = ops.SessionSnapshotArgs{Target: target("a")}
		if op == ops.OpSessionCapture {
			args = ops.SessionCaptureArgs{Target: target("a")}
		}
		_, err := e.ex.Execute(ctx, Request{Op: op, Actions: allActions, Args: mustJSON(t, args)})
		wantAPIError(t, err, "forbidden", "not_shared")
	}
	raw, _ := e.call(ops.OpSessionsList, allActions, ops.SessionsListArgs{})
	if strings.Contains(string(raw), `"a"`) || !strings.Contains(string(raw), `"b"`) {
		t.Fatalf("sessions_list = %s", raw)
	}

	// all and none keep the chosen names for a later select.
	e.mustShare(ops.OpShareSet, shareSetArgs("none"))
	if got := e.saved(); got.Mode != store.ShareNone || !reflect.DeepEqual(got.Sessions, []string{"b"}) {
		t.Fatalf("after none: %+v", got)
	}
	e.mustShare(ops.OpShareSet, shareSetArgs("all"))
	if res := e.mustShare(ops.OpShareGet, shareGetArgs()); res.Rule.Mode != "all" || len(res.Unshared) != 0 {
		t.Fatalf("after all: %+v", res)
	}

	// A new executor over the same state (a restart) finds the choice.
	pol2 := NewPolicy(e.st)
	pol2.SetWorkerID(shareWorkerID) // a restart reads the id from the stored enrollment
	e2, err := NewExecutor(Config{Host: e.host, Classifier: e.cls, State: e.st, Policy: pol2})
	if err != nil {
		t.Fatal(err)
	}
	raw, err = e2.Execute(ctx, Request{Op: ops.OpShareGet, Actions: operateActions, Args: mustJSON(t, shareGetArgs())})
	if err != nil || !strings.Contains(string(raw), `"all"`) {
		t.Fatalf("after a restart: %s %v", raw, err)
	}
}

// Both ops need operate; read, message or nothing do not do.
func TestShareOpsNeedOperate(t *testing.T) {
	e := shareEnv(t, store.ShareSelection{Mode: store.ShareNone}, "a")
	for _, held := range [][]ops.Action{nil, {ops.ActionRead}, {ops.ActionMessage}, {ops.ActionRead, ops.ActionMessage}} {
		for op, args := range map[string]any{ops.OpShareGet: shareGetArgs(), ops.OpShareSet: shareSetArgs("all")} {
			if _, err := e.share(op, held, args); err == nil {
				t.Fatalf("%s with %v was allowed", op, held)
			} else {
				wantAPIError(t, err, "forbidden", "")
			}
		}
	}
	if got := e.saved(); got.Mode != store.ShareNone {
		t.Fatalf("a refused share_set changed the selection: %+v", got)
	}
}

// The local share lock and preview-only refuse share_set, and the Worker's
// selection stays; share_get stays answerable under the lock and says it.
func TestShareSetRefusedByTheLocalLockAndByPreviewOnly(t *testing.T) {
	e := shareEnv(t, store.ShareSelection{Mode: store.ShareNone}, "a")
	e.policy.SetShareLock(true)
	_, err := e.share(ops.OpShareSet, operateActions, shareSetArgs("all"))
	ae := apiErrorOf(t, err, "forbidden", "")
	if !strings.Contains(ae.Message, "share_lock") {
		t.Fatalf("message %q does not say why", ae.Message)
	}
	if got := e.saved(); got.Mode != store.ShareNone {
		t.Fatalf("the lock did not hold: %+v", got)
	}
	if res := e.mustShare(ops.OpShareGet, shareGetArgs()); !res.Locked || res.Rule.Mode != "none" {
		t.Fatalf("share_get under the lock = %+v", res)
	}
	e.policy.SetShareLock(false)
	e.mustShare(ops.OpShareSet, shareSetArgs("all"))

	e.policy.SetPreviewOnly(true)
	_, err = e.share(ops.OpShareSet, operateActions, shareSetArgs("none"))
	wantAPIError(t, err, "forbidden", "preview_only")
	e.policy.SetPreviewOnly(false)
	e.policy.SetHubPreviewOnly(true)
	_, err = e.share(ops.OpShareSet, operateActions, shareSetArgs("none"))
	wantAPIError(t, err, "forbidden", "preview_only")
	if got := e.saved(); got.Mode != store.ShareAll {
		t.Fatalf("preview-only did not hold: %+v", got)
	}
}

// Invalid arguments are refused before anything is saved.
func TestShareSetValidatesItsArguments(t *testing.T) {
	e := shareEnv(t, store.ShareSelection{Mode: store.ShareNone}, "a")
	for _, a := range []ops.ShareSetArgs{
		shareSetArgs("select"),
		shareSetArgs("all", "a"),
		shareSetArgs("later"),
		shareSetArgs("select", "bad:name"),
		shareSetArgs("select", ""),
		{WorkerID: "x", Mode: "all"},
	} {
		_, err := e.share(ops.OpShareSet, operateActions, a)
		wantAPIError(t, err, "invalid_argument", "")
	}
	if got := e.saved(); got.Mode != store.ShareNone || len(got.Sessions) != 0 {
		t.Fatalf("a refused call changed the selection: %+v", got)
	}
}

// The executor announces both ops (the hello lists them).
func TestShareOpsAreAnnounced(t *testing.T) {
	e := newEnv(t)
	have := map[string]bool{}
	for _, n := range e.ex.Ops() {
		have[n] = true
	}
	for _, n := range ops.ShareWorkerOpNames {
		if !have[n] {
			t.Fatalf("%s is not announced: %v", n, e.ex.Ops())
		}
	}
}
