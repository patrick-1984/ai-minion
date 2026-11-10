package authz_test

import (
	"context"
	"errors"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/ops"
)

// memTrust is an in-memory trust table: the authz domain owns the port, so its
// tests need no storage.
type memTrust struct {
	mu     sync.Mutex
	rows   map[[3]string]int64
	owners map[string]string
	broken bool
	// onOwnerRead runs after WorkerOwner returned, to let a test change the
	// owner between the read and the insert.
	onOwnerRead func()
}

var errBroken = errors.New("the trust table is gone")

func (m *memTrust) PutTrust(_ context.Context, r, w, s string, at int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.broken {
		return errBroken
	}
	if _, ok := m.rows[[3]string{r, w, s}]; !ok {
		m.rows[[3]string{r, w, s}] = at
	}
	return nil
}

func (m *memTrust) PutTrustIfOwner(ctx context.Context, r, w, s string, at int64, owner string) error {
	m.mu.Lock()
	cur, ok := m.owners[w]
	m.mu.Unlock()
	switch {
	case !ok:
		return authz.ErrNotFound
	case cur != owner:
		return authz.ErrOwnerChanged
	}
	return m.PutTrust(ctx, r, w, s, at)
}

func (m *memTrust) WorkerOwner(_ context.Context, w string) (string, error) {
	m.mu.Lock()
	owner, ok := m.owners[w]
	hook := m.onOwnerRead
	m.mu.Unlock()
	if hook != nil {
		hook()
	}
	if !ok {
		return "", authz.ErrNotFound
	}
	return owner, nil
}

func (m *memTrust) DeleteTrust(_ context.Context, r, w, s string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.rows[[3]string{r, w, s}]
	delete(m.rows, [3]string{r, w, s})
	return ok, nil
}

func (m *memTrust) HasTrust(_ context.Context, r, w, s string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.broken {
		return false, errBroken
	}
	for k := range m.rows {
		if k[0] == r && k[1] == w && (k[2] == "" || k[2] == s) {
			return true, nil
		}
	}
	return false, nil
}

func (m *memTrust) ListTrust(_ context.Context, r string) ([]authz.TrustRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []authz.TrustRecord
	for k, at := range m.rows {
		if k[0] == r {
			out = append(out, authz.TrustRecord{WorkerID: k[1], SessionName: k[2], CreatedAt: at})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].WorkerID != out[j].WorkerID {
			return out[i].WorkerID < out[j].WorkerID
		}
		return out[i].SessionName < out[j].SessionName
	})
	return out, nil
}

func newTrust(t *testing.T) (*authz.Trust, *memTrust) {
	t.Helper()
	st := &memTrust{rows: map[[3]string]int64{}, owners: map[string]string{}}
	at := time.UnixMilli(1_700_000_000_000)
	return authz.NewTrust(st, func() time.Time { return at }), st
}

func TestTrustSetAndLookup(t *testing.T) {
	tr, _ := newTrust(t)
	ctx := context.Background()
	if err := tr.Set(ctx, "alice", "w-alice", ""); err != nil {
		t.Fatal(err)
	}
	if err := tr.Set(ctx, "alice", "w-bob", "minion-2"); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		receiver, worker, session string
		want                      bool
	}{
		{"alice", "w-alice", "minion-1", true},  // the whole Worker covers its sessions
		{"alice", "w-alice", "", true},          //
		{"alice", "w-bob", "minion-2", true},    // one session
		{"alice", "w-bob", "minion-3", false},   // not the other sessions
		{"alice", "w-bob", "", false},           // and not the Worker
		{"bob", "w-alice", "minion-1", false},   // trust is per receiver
		{"", "w-alice", "minion-1", false},      // no receiver, no trust
		{"alice", "", "minion-1", false},        //
		{"admin", "w-alice", "minion-1", false}, // an admin has no implicit trust either
	}
	for _, tc := range tests {
		got, err := tr.Trusted(ctx, tc.receiver, tc.worker, tc.session)
		if err != nil || got != tc.want {
			t.Errorf("Trusted(%q, %q, %q) = %v, %v; want %v", tc.receiver, tc.worker, tc.session, got, err, tc.want)
		}
	}
}

func TestTrustListAndRemove(t *testing.T) {
	tr, _ := newTrust(t)
	ctx := context.Background()
	_ = tr.Set(ctx, "alice", "w1", "")
	_ = tr.Set(ctx, "alice", "w1", "minion-1")
	_ = tr.Set(ctx, "bob", "w2", "")
	got, err := tr.List(ctx, "alice")
	if err != nil || len(got) != 2 || got[0].WorkerID != "w1" || got[0].SessionName != "" || got[1].SessionName != "minion-1" {
		t.Fatalf("List = %+v, %v", got, err)
	}
	if !got[0].CreatedAt.Equal(time.UnixMilli(1_700_000_000_000)) {
		t.Errorf("created_at = %v", got[0].CreatedAt)
	}
	if existed, err := tr.Remove(ctx, "alice", "w1", ""); err != nil || !existed {
		t.Fatalf("Remove = %v, %v", existed, err)
	}
	if ok, _ := tr.Trusted(ctx, "alice", "w1", "minion-1"); !ok {
		t.Error("removing the whole-Worker mark removed a session mark")
	}
	if ok, _ := tr.Trusted(ctx, "alice", "w1", "minion-9"); ok {
		t.Error("the Worker is still trusted")
	}
	if rows, _ := tr.List(ctx, "bob"); len(rows) != 1 {
		t.Errorf("bob's marks = %+v", rows)
	}
}

func TestTrustRefusesMissingIdentifiers(t *testing.T) {
	tr, _ := newTrust(t)
	ctx := context.Background()
	if err := tr.Set(ctx, "", "w", ""); !errors.Is(err, authz.ErrTrustInvalid) {
		t.Errorf("Set without receiver: %v", err)
	}
	if err := tr.Set(ctx, "u", "", ""); !errors.Is(err, authz.ErrTrustInvalid) {
		t.Errorf("Set without Worker: %v", err)
	}
	if _, err := tr.Remove(ctx, "", "w", ""); !errors.Is(err, authz.ErrTrustInvalid) {
		t.Errorf("Remove without receiver: %v", err)
	}
	if _, err := tr.List(ctx, ""); !errors.Is(err, authz.ErrTrustInvalid) {
		t.Errorf("List without receiver: %v", err)
	}
}

func TestTrustFailsClosedWhenTheStoreFails(t *testing.T) {
	tr, st := newTrust(t)
	ctx := context.Background()
	_ = tr.Set(ctx, "alice", "w1", "")
	st.broken = true
	if ok, err := tr.Trusted(ctx, "alice", "w1", ""); ok || err == nil {
		t.Errorf("Trusted on a broken store = %v, %v", ok, err)
	}
}

func TestTrustReceiverIsTheActorDuringActAs(t *testing.T) {
	exp := time.Now().Add(time.Hour)
	tests := []struct {
		name string
		p    authz.Principal
		want string
	}{
		{"session", authz.NewSessionPrincipal("alice"), "alice"},
		{"api key", authz.NewAPIKeyPrincipal("alice", stubScope{}, ""), "alice"},
		{"act-as", authz.NewActAsPrincipal("admin", "alice", exp), "admin"},
		{"worker identity", authz.NewWorkerPrincipal("w1"), ""},
	}
	for _, tc := range tests {
		if got := authz.TrustReceiver(tc.p); got != tc.want {
			t.Errorf("%s: receiver = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// Trust only tells whether a model reads output as trusted. It is not an input
// of Decide, so whatever the trust rows are, an admin keeps every permission
// and a user keeps exactly the permissions of ownership.
func TestTrustNeverRestrictsAnAdmin(t *testing.T) {
	tr, _ := newTrust(t)
	ctx := context.Background()
	st := newStore()
	az := mk(st)
	admin := authz.NewSessionPrincipal("admin")
	all := []ops.Action{ops.ActionRead, ops.ActionOperate, ops.ActionMessage, ops.ActionManage}

	decide := func() []error {
		var errs []error
		for _, a := range all {
			errs = append(errs, az.Decide(ctx, admin, ops.MustActionSet(a), authz.SessionResource{WorkerID: "w-alice", Name: "minion-1"}))
		}
		return errs
	}
	for _, err := range decide() {
		if err != nil {
			t.Fatalf("admin without any trust row: %v", err)
		}
	}
	// The admin trusts nothing, and others' trust rows exist for that Worker.
	_ = tr.Set(ctx, "alice", "w-alice", "")
	_ = tr.Set(ctx, "bob", "w-alice", "minion-1")
	if ok, _ := tr.Trusted(ctx, "admin", "w-alice", "minion-1"); ok {
		t.Fatal("the admin was trusted without a row")
	}
	for i, err := range decide() {
		if err != nil {
			t.Errorf("admin action %s denied because of trust: %v", all[i], err)
		}
	}
	// Removing every trust row changes no decision for the owner either.
	_, _ = tr.Remove(ctx, "alice", "w-alice", "")
	if err := az.Decide(ctx, authz.NewSessionPrincipal("alice"), ops.MustActionSet(ops.ActionOperate), authz.WorkerResource{ID: "w-alice"}); err != nil {
		t.Errorf("owner operate after trust removal: %v", err)
	}
}

func TestSetAuthorizedRechecksAndRefusesAMarkDecidedUnderAPreviousOwner(t *testing.T) {
	tr, st := newTrust(t)
	ctx := context.Background()
	st.owners["w1"] = "alice"

	// The recheck runs and its refusal stores nothing.
	denied := errors.New("denied")
	if err := tr.SetAuthorized(ctx, "alice", "w1", "", func(context.Context) error { return denied }); !errors.Is(err, denied) {
		t.Errorf("recheck refusal: %v", err)
	}
	if rows, _ := tr.List(ctx, "alice"); len(rows) != 0 {
		t.Errorf("a refused call stored %+v", rows)
	}
	if err := tr.SetAuthorized(ctx, "alice", "w1", "", nil); err != nil {
		t.Fatalf("under the current owner: %v", err)
	}
	// The Worker changes hands right after the owner was read: the insert is
	// refused, so the mark cannot outlive the reassignment.
	st.onOwnerRead = func() { st.owners["w1"] = "bob" }
	if err := tr.SetAuthorized(ctx, "alice", "w1", "minion-1", nil); !errors.Is(err, authz.ErrOwnerChanged) {
		t.Errorf("a mark decided under the previous owner: %v, want ErrOwnerChanged", err)
	}
	if rows, _ := tr.List(ctx, "alice"); len(rows) != 1 || rows[0].SessionName != "" {
		t.Errorf("rows = %+v, want only the first mark", rows)
	}
	if err := tr.SetAuthorized(ctx, "alice", "nope", "", nil); !errors.Is(err, authz.ErrNotFound) {
		t.Errorf("unknown Worker: %v", err)
	}
	if err := tr.SetAuthorized(ctx, "", "w1", "", nil); !errors.Is(err, authz.ErrTrustInvalid) {
		t.Errorf("no receiver: %v", err)
	}
}

func TestSetUnderIsConditionedOnTheOwnerCapturedBeforeTheFirstDecision(t *testing.T) {
	tr, st := newTrust(t)
	ctx := context.Background()
	st.owners["w1"] = "alice"

	owner, err := tr.Owner(ctx, "w1")
	if err != nil || owner != "alice" {
		t.Fatalf("Owner = %q, %v", owner, err)
	}
	if _, err := tr.Owner(ctx, "nope"); !errors.Is(err, authz.ErrNotFound) {
		t.Errorf("Owner of an unknown Worker: %v", err)
	}
	// The Worker changes hands after the capture and before anything else of
	// the request (the decision, the recheck): the recheck passes (an admin
	// passes every decision) but the insert is refused.
	st.owners["w1"] = "bob"
	if err := tr.SetUnder(ctx, "admin", "w1", "", owner, func(context.Context) error { return nil }); !errors.Is(err, authz.ErrOwnerChanged) {
		t.Errorf("SetUnder after a reassignment: %v, want ErrOwnerChanged", err)
	}
	if rows, _ := tr.List(ctx, "admin"); len(rows) != 0 {
		t.Errorf("a stale mark was stored: %+v", rows)
	}
	if err := tr.SetUnder(ctx, "admin", "w1", "", "bob", nil); err != nil {
		t.Errorf("SetUnder under the current owner: %v", err)
	}
	if err := tr.SetUnder(ctx, "admin", "w1", "", "", nil); !errors.Is(err, authz.ErrTrustInvalid) {
		t.Errorf("SetUnder without an owner: %v", err)
	}
}
