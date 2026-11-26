package workers

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// fakeCodes maps codes to (worker, session name) and numbers sightings in order.
type fakeCodes struct {
	workers map[string]int64
	byCode  map[ops.SessionCode][2]string
	seen    []store.SessionSighting
}

func (f *fakeCodes) SessionCodes(_ context.Context, workerID string, seen []store.SessionSighting, _ time.Time) (int64, []int64, error) {
	f.seen = append(f.seen, seen...)
	out := make([]int64, len(seen))
	for i := range seen {
		out[i] = int64(i + 1)
	}
	return f.workers[workerID], out, nil
}

func (f *fakeCodes) WorkerNumbers(context.Context) (map[string]int64, error) { return f.workers, nil }

func (f *fakeCodes) ResolveSessionCode(_ context.Context, w, s int64) (string, string, bool, error) {
	v, ok := f.byCode[ops.SessionCode{Worker: w, Session: s}]
	return v[0], v[1], ok, nil
}

func TestResolveSessionCode(t *testing.T) {
	fc := &fakeCodes{
		workers: map[string]int64{wAlicePi1.ID: 5, wBobBox.ID: 6},
		byCode: map[ops.SessionCode][2]string{
			{Worker: 5, Session: 667}: {wAlicePi1.ID, "minion-1"},
			{Worker: 6, Session: 1}:   {wBobBox.ID, "secret"},
		},
	}
	r := newRig(t, func(c *RouterConfig) { c.Codes = &SessionCodes{Store: fc} })
	sessionOnlyKey := authz.NewAPIKeyPrincipal(idAlice, scopeFunc(func(res authz.ResolvedResource) []ops.Action {
		if res.Kind == authz.ResourceSession && res.WorkerID == wAlicePi1.ID && res.SessionName == "other" {
			return []ops.Action{ops.ActionRead}
		}
		return nil
	}), "")
	ctx := context.Background()
	for _, in := range []string{"5-667", "#5-667", "s5-667", "S5-667"} {
		got, err := r.rt.Resolve(ctx, routeAlice(), in, false)
		if err != nil || got != (ops.Target{WorkerID: wAlicePi1.ID, SessionName: "minion-1"}) {
			t.Fatalf("%s: %+v %v", in, got, err)
		}
	}
	got, err := r.rt.Resolve(ctx, routeAlice(), "5-667:0.1", false)
	if err != nil || got.Pane != "0.1" || got.SessionName != "minion-1" {
		t.Fatalf("with pane: %+v %v", got, err)
	}
	// Unknown, foreign and out-of-scope codes all give the same answer.
	var msgs []string
	for _, c := range []struct {
		p  authz.Principal
		in string
	}{{routeAlice(), "5-668"}, {routeAlice(), "9-1"}, {routeAlice(), "6-1"}, {sessionOnlyKey, "5-667"}} {
		_, err := r.rt.Resolve(ctx, c.p, c.in, true)
		var pe *proto.Error
		if !errors.As(err, &pe) || pe.Code != proto.CodeTargetNotFound {
			t.Fatalf("%s: err = %v, want target_not_found", c.in, err)
		}
		msgs = append(msgs, pe.Message)
	}
	for _, m := range msgs[1:] {
		if m != msgs[0] {
			t.Fatalf("answers differ: %q", msgs)
		}
	}
	// The admin with all sees Bob's session by its code.
	if got, err := r.rt.Resolve(ctx, routeRoot(), "6-1", true); err != nil || got.WorkerID != wBobBox.ID {
		t.Fatalf("admin all: %+v %v", got, err)
	}
	// Without codes configured a code is no target.
	plain := newRig(t, nil)
	if _, err := plain.rt.Resolve(ctx, routeAlice(), "5-667", false); !errors.Is(err, ops.ErrInvalidArgument) {
		t.Fatalf("no codes: %v", err)
	}
}

func TestSessionCodesAnnotateUsesInstanceIDs(t *testing.T) {
	fc := &fakeCodes{workers: map[string]int64{"w": 5}}
	c := &SessionCodes{Store: fc}
	list := []ops.SessionInfo{{Name: "a", InstanceID: "t1-a"}, {Name: "b"}}
	c.Annotate(context.Background(), "w", list)
	if list[0].Code != "5-1" || list[1].Code != "5-2" {
		t.Fatalf("codes = %q %q", list[0].Code, list[1].Code)
	}
	if fc.seen[0].InstanceID != "t1-a" || fc.seen[1].InstanceID != "" {
		t.Fatalf("sightings = %+v", fc.seen)
	}
	if m := c.Sightings(context.Background(), "w", []store.SessionSighting{{Name: "x", InstanceID: "t9"}}); m["x"] != "5-1" {
		t.Fatalf("sightings = %v", m)
	}
	if fc.seen[2].InstanceID != "t9" {
		t.Fatalf("sightings = %+v", fc.seen)
	}
}

// A code a Worker sends in its sessions_list never passes: it is cleared when
// the Hub has no code store, when the store fails and when it gives no number.
func TestSessionCodesAnnotateClearsWorkerCodes(t *testing.T) {
	forged := func() []ops.SessionInfo { return []ops.SessionInfo{{Name: "a", Code: "9-9"}, {Name: "", Code: "9-8"}} }
	for name, c := range map[string]*SessionCodes{
		"nil":      nil,
		"no store": {},
		"failing":  {Store: &failingCodes{}},
		"numbered": {Store: &fakeCodes{workers: map[string]int64{"w": 5}}},
		"unknown":  {Store: &fakeCodes{}}, // Worker number 0: no code
	} {
		list := forged()
		c.Annotate(context.Background(), "w", list)
		want := ""
		if name == "numbered" {
			want = "5-1"
		}
		if list[0].Code != want || (list[1].Code != "" && list[1].Code != "5-2") {
			t.Fatalf("%s: codes = %q %q", name, list[0].Code, list[1].Code)
		}
	}
}

type failingCodes struct{ fakeCodes }

func (*failingCodes) SessionCodes(context.Context, string, []store.SessionSighting, time.Time) (int64, []int64, error) {
	return 0, nil, errors.New("disk full")
}

// The routed sessions_list (Annotate) and the event-stream list (Sightings)
// give a session the same code, whichever runs first: after a rename and
// after the name is created again by another terminal.
func TestSessionCodesBothListsAgree(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	c := &SessionCodes{Store: st}
	ctx := context.Background()
	routed := func(name, iid string) string {
		list := []ops.SessionInfo{{Name: name, InstanceID: iid}}
		c.Annotate(ctx, "w1", list)
		return list[0].Code
	}
	stream := func(name, iid string) string {
		return c.Sightings(ctx, "w1", []store.SessionSighting{{Name: name, InstanceID: iid}})[name]
	}
	both := func(step, name, iid string, streamFirst bool) string {
		t.Helper()
		var a, b string
		if streamFirst {
			b, a = stream(name, iid), routed(name, iid)
		} else {
			a, b = routed(name, iid), stream(name, iid)
		}
		if a == "" || a != b {
			t.Fatalf("%s: sessions_list %q, event stream %q", step, a, b)
		}
		return a
	}
	first := both("first sighting", "a", "$1", true)
	if again := both("listed again", "a", "$1", false); again != first {
		t.Fatalf("listed again: %q, want %q", again, first)
	}
	recreated := both("same-name recreate", "a", "$2", true)
	if recreated == first {
		t.Fatalf("a recreated session kept the code %q", first)
	}
	if renamed := both("rename", "b", "$2", true); renamed != recreated {
		t.Fatalf("rename: %q, want %q", renamed, recreated)
	}
	if renamed := both("rename, listed again", "b", "$2", false); renamed != recreated {
		t.Fatalf("rename listed again: %q, want %q", renamed, recreated)
	}
	if _, name, ok, err := st.ResolveSessionCode(ctx, 1, 2); err != nil || !ok || name != "b" {
		t.Fatalf("resolve %s = %q %v %v", recreated, name, ok, err)
	}
}

func TestWorkersListCarriesNumbers(t *testing.T) {
	fc := &fakeCodes{workers: map[string]int64{wAlicePi1.ID: 5}}
	r := newRig(t, func(c *RouterConfig) { c.Codes = &SessionCodes{Store: fc} })
	list, err := r.rt.ListWorkers(context.Background(), routeAlice(), false)
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range list {
		want := int64(0)
		if w.ID == wAlicePi1.ID {
			want = 5
		}
		if w.Number != want {
			t.Fatalf("%s: number %d, want %d", w.Name, w.Number, want)
		}
	}
}

func TestParseSessionCodeRejectsOtherTargets(t *testing.T) {
	for _, in := range []string{"pi-1:minion-1", "5", "5-", "-5", "5-6-7", "0-1", "5-0", "x5-6", "5-6:a", "5-6:0.1:2", "5 - 6", "1234567890123-1"} {
		if _, _, ok := ops.ParseSessionCode(in); ok {
			t.Errorf("%q read as a code", in)
		}
	}
}
