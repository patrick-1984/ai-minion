package workers

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/transport"
)

// fakeWorkerHidden is a WorkerHiddenSessionsSource of fixed choices.
type fakeWorkerHidden struct {
	choices map[string]bool
	err     error
}

func (f fakeWorkerHidden) WorkerHiddenSessions(_ context.Context, id string) (*bool, error) {
	if f.err != nil {
		return nil, f.err
	}
	if v, ok := f.choices[id]; ok {
		return &v, nil
	}
	return nil, nil
}

func (f fakeWorkerHidden) ListWorkerHiddenSessions(context.Context) (map[string]bool, error) {
	return f.choices, f.err
}

func ptrBool(v bool) *bool { return &v }

// The Worker's own choice wins over the user's account-wide preference while
// the Hub setting is user-controlled; allowed and disabled still override it.
func TestHiddenSessionPolicyWorkerChoice(t *testing.T) {
	for _, tc := range []struct {
		hub    string
		user   bool
		worker *bool
		want   bool
	}{
		{config.HiddenSessionsUserControlled, false, nil, false},
		{config.HiddenSessionsUserControlled, true, nil, true},
		{config.HiddenSessionsUserControlled, false, ptrBool(true), true},
		{config.HiddenSessionsUserControlled, true, ptrBool(false), false},
		{config.HiddenSessionsAllowed, false, ptrBool(false), true},
		{config.HiddenSessionsDisabled, true, ptrBool(true), false},
	} {
		pol := HiddenSessionsPolicy{Hub: tc.hub, UserAllows: tc.user, Worker: tc.worker}
		if got := pol.Allows(); got != tc.want {
			t.Errorf("%+v: Allows = %v, want %v", tc, got, tc.want)
		}
	}
	msg := HiddenSessionRefusal("pi-1", factsHidden, HiddenSessionsPolicy{Hub: config.HiddenSessionsUserControlled, UserAllows: true, Worker: ptrBool(false)})
	if !strings.Contains(msg, "refused on this Worker") || strings.Contains(msg, "Account > Sessions") {
		t.Errorf("refusal of a Worker choice = %q", msg)
	}
}

func hiddenWorkerRig(t *testing.T, pol HiddenSessionsPolicy, src fakeWorkerHidden, facts transport.WorkerFacts) *rig {
	t.Helper()
	r := newRig(t, func(c *RouterConfig) {
		c.HiddenSessions = func(context.Context, authz.Principal) (HiddenSessionsPolicy, error) { return pol, nil }
		c.WorkerHiddenSessions = src
	})
	r.be.online[wAlicePi1.ID] = Online{Facts: facts}
	return r
}

func TestSessionCreateFollowsTheWorkerChoice(t *testing.T) {
	user := HiddenSessionsPolicy{Hub: config.HiddenSessionsUserControlled}
	// The user refuses hidden sessions account-wide, the Worker allows them.
	r := hiddenWorkerRig(t, user, fakeWorkerHidden{choices: map[string]bool{wAlicePi1.ID: true}}, factsHidden)
	r.script(newFakeStream(resultFrame(t, ops.WriteResult{Outcome: ops.OutcomeDelivered})))
	if _, err := r.rt.Call(context.Background(), createReq()); err != nil {
		t.Fatalf("session_create on a Worker that allows hidden sessions: %v", err)
	}
	// The user allows them account-wide, the Worker refuses them.
	user.UserAllows = true
	r = hiddenWorkerRig(t, user, fakeWorkerHidden{choices: map[string]bool{wAlicePi1.ID: false}}, factsHidden)
	_, err := r.rt.Call(context.Background(), createReq())
	if code(err) != proto.CodeForbidden || !strings.Contains(err.Error(), "refused on this Worker") || r.be.openCount() != 0 {
		t.Fatalf("err = %v, opens = %v; want the Worker's refusal", err, r.be.opens)
	}
	// No choice on this Worker (another Worker has one): the account default.
	r = hiddenWorkerRig(t, user, fakeWorkerHidden{choices: map[string]bool{wAlicePi2.ID: false}}, factsHidden)
	r.script(newFakeStream(resultFrame(t, ops.WriteResult{Outcome: ops.OutcomeDelivered})))
	if _, err := r.rt.Call(context.Background(), createReq()); err != nil {
		t.Fatalf("session_create with the account default: %v", err)
	}
	// A disabled Hub ignores the Worker's choice.
	r = hiddenWorkerRig(t, HiddenSessionsPolicy{Hub: config.HiddenSessionsDisabled}, fakeWorkerHidden{choices: map[string]bool{wAlicePi1.ID: true}}, factsHidden)
	if _, err := r.rt.Call(context.Background(), createReq()); code(err) != proto.CodeForbidden {
		t.Fatalf("err = %v, want forbidden under a disabled Hub", err)
	}
	// A failed read refuses (fail closed).
	r = hiddenWorkerRig(t, user, fakeWorkerHidden{err: errors.New("disk on fire")}, factsHidden)
	if _, err := r.rt.Call(context.Background(), createReq()); err == nil || r.be.openCount() != 0 {
		t.Fatalf("err = %v, opens = %v; want a refusal before sending", err, r.be.opens)
	}
}

func TestWorkersListCarriesTheWorkerChoice(t *testing.T) {
	r := hiddenWorkerRig(t, HiddenSessionsPolicy{Hub: config.HiddenSessionsUserControlled},
		fakeWorkerHidden{choices: map[string]bool{wAlicePi1.ID: true, wAlicePi2.ID: false}}, factsHidden)
	r.be.online[wAlicePi2.ID] = Online{Facts: factsHidden}
	list, err := r.rt.ListWorkers(context.Background(), routeRoot(), true)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]ops.WorkerInfo{}
	for _, w := range list {
		got[w.ID] = w
	}
	if w := got[wAlicePi1.ID]; w.AllowHiddenSessions == nil || !*w.AllowHiddenSessions || w.NewSessionRefused != "" {
		t.Errorf("allowing Worker = %+v", w)
	}
	if w := got[wAlicePi2.ID]; w.AllowHiddenSessions == nil || *w.AllowHiddenSessions || !strings.Contains(w.NewSessionRefused, "refused on this Worker") {
		t.Errorf("refusing Worker = %+v", w)
	}
	if w := got[wBobPi1.ID]; w.AllowHiddenSessions != nil {
		t.Errorf("Worker without a choice = %+v", w)
	}
}
