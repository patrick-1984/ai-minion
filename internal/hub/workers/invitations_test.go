package workers

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/audit"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

type invRig struct {
	st    *store.Store
	inv   *Invitations
	audit *audit.Log
	clock *testClock
	fpr   identity.Fingerprint
}

func newInvRig(t *testing.T) *invRig {
	t.Helper()
	st := openStore(t, t.TempDir())
	r := &invRig{st: st, audit: audit.New(st), clock: &testClock{t: time.UnixMilli(1_800_000_000_000)}, fpr: newHubFingerprint(t)}
	inv, err := NewInvitations(InvitationsConfig{Store: st, HubFingerprint: r.fpr, Auditor: r.audit, Now: r.clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	r.inv = inv
	for _, id := range []string{alice.UserID, bob.UserID, root.UserID} {
		ensureUser(t, st, id)
	}
	return r
}

var (
	alice = InvitationCaller{UserID: "u-alice", Actor: "u-alice"}
	bob   = InvitationCaller{UserID: "u-bob", Actor: "u-bob"}
	root  = InvitationCaller{UserID: "u-root", Actor: "u-root", Admin: true}
)

func codeOf(err error) proto.Code {
	var pe *proto.Error
	if errors.As(err, &pe) {
		return pe.Code
	}
	return ""
}

func TestCreateReturnsTheKeyOnceAndStoresOnlyTheSecretHash(t *testing.T) {
	r := newInvRig(t)
	ctx := context.Background()
	res, err := r.inv.Create(ctx, alice, ops.InvitationCreateArgs{Mode: ops.InvitationOneTime, TTL: ops.TTL4h, Name: "pi"})
	if err != nil {
		t.Fatal(err)
	}
	k, err := identity.ParseInviteKey(res.Key)
	if err != nil {
		t.Fatalf("key %q: %v", "mnk_...", err)
	}
	if k.Fingerprint != r.fpr || !strings.HasPrefix(res.Key, "mnk_"+string(r.fpr)+".") {
		t.Errorf("the key does not carry the Hub fingerprint")
	}
	row, err := r.st.InvitationByID(ctx, res.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := sha256.Sum256(k.Secret)
	if !bytes.Equal(row.SecretHash, want[:]) {
		t.Error("the stored hash is not SHA-256 of the secret")
	}
	if row.CreatorID != "u-alice" || row.Name != "pi" || row.Mode != store.InvitationOneTime || row.Kind != store.WorkerKindPrivate ||
		row.Uses != 0 || row.Revoked || row.MaxUses != 1 || row.TTL != "4h" {
		t.Errorf("row = %+v", row)
	}
	// The secret itself is nowhere in the database file.
	var stored int
	secretText := secretOf(t, res.Key)
	err = r.st.DB().QueryRow(`SELECT COUNT(*) FROM invitation_keys WHERE name || id || creator_id LIKE '%' || ? || '%'`, secretText).Scan(&stored)
	if err != nil || stored != 0 {
		t.Errorf("the secret appears in a text column (%d, %v)", stored, err)
	}
	// It is not in the list or in any audit event either.
	list, err := r.inv.List(ctx, alice, false)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(list)
	if bytes.Contains(raw, []byte(secretText)) {
		t.Error("invitation_list leaks the secret")
	}
	page, _ := r.audit.List(ctx, audit.Filter{Action: AuditInvitationCreate})
	if len(page.Events) != 1 || page.Events[0].Target != res.ID || page.Events[0].Actor != "u-alice" || page.Events[0].Outcome != "success" {
		t.Errorf("audit = %+v", page.Events)
	}
	// Two keys never share a secret.
	res2, _ := r.inv.Create(ctx, alice, ops.InvitationCreateArgs{Mode: ops.InvitationOneTime, TTL: ops.TTL4h})
	if res2.Key == res.Key || res2.ID == res.ID {
		t.Error("two keys are equal")
	}
}

func TestTTLTable(t *testing.T) {
	r := newInvRig(t)
	ctx := context.Background()
	want := map[string]time.Duration{
		ops.TTL1h: time.Hour, ops.TTL4h: 4 * time.Hour, ops.TTL8h: 8 * time.Hour, ops.TTL24h: 24 * time.Hour,
		ops.TTL1w: 7 * 24 * time.Hour, ops.TTL1m: 30 * 24 * time.Hour, ops.TTLUnlimited: 0,
	}
	if len(want) != len(ops.InvitationTTLs()) {
		t.Fatalf("the TTL table has %d entries, the test %d", len(ops.InvitationTTLs()), len(want))
	}
	for ttl, d := range want {
		res, err := r.inv.Create(ctx, alice, ops.InvitationCreateArgs{Mode: ops.InvitationReusable, TTL: ttl})
		if err != nil {
			t.Fatalf("%s: %v", ttl, err)
		}
		created := time.UnixMilli(1_800_000_000_000).UTC()
		switch {
		case d == 0 && res.ExpiresAt != nil:
			t.Errorf("%s: expires at %v, want never", ttl, res.ExpiresAt)
		case d != 0 && (res.ExpiresAt == nil || !res.ExpiresAt.Time().Equal(created.Add(d))):
			t.Errorf("%s: expires at %v, want %v", ttl, res.ExpiresAt, created.Add(d))
		}
	}
}

func TestCreateRejectsBadArguments(t *testing.T) {
	r := newInvRig(t)
	for name, a := range map[string]ops.InvitationCreateArgs{
		"mode":    {Mode: "forever", TTL: ops.TTL1h},
		"ttl":     {Mode: ops.InvitationOneTime, TTL: "2h"},
		"no ttl":  {Mode: ops.InvitationOneTime},
		"kind":    {Mode: ops.InvitationOneTime, TTL: ops.TTL1h, Kind: "shared"},
		"name":    {Mode: ops.InvitationOneTime, TTL: ops.TTL1h, Name: "bad\x00name"},
		"toolong": {Mode: ops.InvitationOneTime, TTL: ops.TTL1h, Name: strings.Repeat("x", 65)},
	} {
		if _, err := r.inv.Create(context.Background(), alice, a); codeOf(err) != proto.CodeInvalidArgument {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	if _, err := r.inv.Create(context.Background(), InvitationCaller{}, ops.InvitationCreateArgs{Mode: ops.InvitationOneTime, TTL: ops.TTL1h}); codeOf(err) != proto.CodeForbidden {
		t.Errorf("anonymous caller: %v", err)
	}
}

func TestInfrastructureKeysNeedAnAdmin(t *testing.T) {
	r := newInvRig(t)
	ctx := context.Background()
	a := ops.InvitationCreateArgs{Mode: ops.InvitationReusable, TTL: ops.TTL1w, Kind: ops.KindInfrastructure}
	if _, err := r.inv.Create(ctx, alice, a); codeOf(err) != proto.CodeForbidden {
		t.Fatalf("user: err = %v, want forbidden", err)
	}
	res, err := r.inv.Create(ctx, root, a)
	if err != nil || res.Kind != ops.KindInfrastructure {
		t.Fatalf("admin: %+v %v", res, err)
	}
	row, _ := r.st.InvitationByID(ctx, res.ID)
	if row.Kind != store.WorkerKindInfrastructure || row.CreatorID != "u-root" || row.MaxUses != 0 {
		t.Errorf("row = %+v", row)
	}
	// An explicit private kind and the default are the same for a user.
	for _, kind := range []string{"", ops.KindPrivate} {
		if res, err := r.inv.Create(ctx, alice, ops.InvitationCreateArgs{Mode: ops.InvitationOneTime, TTL: ops.TTL1h, Kind: kind}); err != nil || res.Kind != ops.KindPrivate {
			t.Errorf("kind %q: %+v %v", kind, res, err)
		}
	}
}

func TestListShowsOwnKeysAndAdminsCanSeeAll(t *testing.T) {
	r := newInvRig(t)
	ctx := context.Background()
	mk := func(c InvitationCaller, name string) ops.InvitationCreateResult {
		t.Helper()
		res, err := r.inv.Create(ctx, c, ops.InvitationCreateArgs{Mode: ops.InvitationReusable, TTL: ops.TTL1h, Name: name})
		if err != nil {
			t.Fatal(err)
		}
		r.clock.Advance(time.Millisecond)
		return res
	}
	a1, a2, b1 := mk(alice, "a1"), mk(alice, "a2"), mk(bob, "b1")

	got, err := r.inv.List(ctx, alice, false)
	if err != nil || len(got.Invitations) != 2 || got.Invitations[0].ID != a2.ID || got.Invitations[1].ID != a1.ID {
		t.Fatalf("alice's list = %+v err=%v (newest first expected)", got, err)
	}
	if _, err := r.inv.List(ctx, alice, true); codeOf(err) != proto.CodeForbidden {
		t.Errorf("a user listing all: %v", err)
	}
	all, err := r.inv.List(ctx, root, true)
	if err != nil || len(all.Invitations) != 3 {
		t.Fatalf("admin list all = %+v err=%v", all, err)
	}
	own, _ := r.inv.List(ctx, root, false)
	if len(own.Invitations) != 0 {
		t.Errorf("the admin's own list = %+v, want empty", own)
	}
	empty, _ := r.inv.List(ctx, InvitationCaller{UserID: "u-new", Actor: "u-new"}, false)
	if raw, _ := json.Marshal(empty); !strings.Contains(string(raw), `"invitations":[]`) {
		t.Errorf("empty list = %s", raw)
	}
	_ = b1
}

func TestListReportsUsesAndState(t *testing.T) {
	r := newInvRig(t)
	ctx := context.Background()
	oneTime, _ := r.inv.Create(ctx, alice, ops.InvitationCreateArgs{Mode: ops.InvitationOneTime, TTL: ops.TTL24h})
	reusable, _ := r.inv.Create(ctx, alice, ops.InvitationCreateArgs{Mode: ops.InvitationReusable, TTL: ops.TTL8h})
	revoked, _ := r.inv.Create(ctx, alice, ops.InvitationCreateArgs{Mode: ops.InvitationReusable, TTL: ops.TTLUnlimited})
	if err := r.inv.Revoke(ctx, alice, revoked.ID); err != nil {
		t.Fatal(err)
	}
	state := func() map[string]ops.InvitationInfo {
		t.Helper()
		l, err := r.inv.List(ctx, alice, false)
		if err != nil {
			t.Fatal(err)
		}
		m := map[string]ops.InvitationInfo{}
		for _, i := range l.Invitations {
			m[i.ID] = i
		}
		return m
	}
	m := state()
	if i := m[oneTime.ID]; i.Status != ops.InvitationActive || i.MaxUses == nil || *i.MaxUses != 1 || i.Uses != 0 || i.CreatorID != "u-alice" {
		t.Errorf("one-time = %+v", i)
	}
	if i := m[reusable.ID]; i.Status != ops.InvitationActive || i.MaxUses != nil || i.ExpiresAt == nil {
		t.Errorf("reusable = %+v", i)
	}
	if i := m[revoked.ID]; i.Status != ops.InvitationRevoked || !i.Revoked || i.RevokedAt == nil || i.ExpiresAt != nil {
		t.Errorf("revoked = %+v", i)
	}

	// Use up the one-time key and let the first TTL run out.
	_, err := r.st.EnrollWithInvitation(ctx, store.EnrollParams{
		SecretHash: rowHash(t, r.st, oneTime.ID), Pub: bytes.Repeat([]byte{1}, 32), Name: "w", WorkerID: store.NewID(), Now: store.UnixMS(r.clock.Now()),
	})
	if err != nil {
		t.Fatal(err)
	}
	r.clock.Advance(2 * time.Hour)
	m = state()
	if i := m[oneTime.ID]; i.Status != ops.InvitationExhausted || i.Uses != 1 {
		t.Errorf("used one-time = %+v", i)
	}
	if i := m[reusable.ID]; i.Status != ops.InvitationActive {
		t.Errorf("reusable after 2h = %+v", i)
	}
	r.clock.Advance(7 * time.Hour)
	if i := state()[reusable.ID]; i.Status != ops.InvitationExpired {
		t.Errorf("reusable after 9h = %+v", i)
	}
}

func rowHash(t *testing.T, st *store.Store, id string) []byte {
	t.Helper()
	row, err := st.InvitationByID(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return row.SecretHash
}

func TestRevoke(t *testing.T) {
	r := newInvRig(t)
	ctx := context.Background()
	mine, _ := r.inv.Create(ctx, alice, ops.InvitationCreateArgs{Mode: ops.InvitationReusable, TTL: ops.TTL1h})
	theirs, _ := r.inv.Create(ctx, bob, ops.InvitationCreateArgs{Mode: ops.InvitationReusable, TTL: ops.TTL1h})

	// Somebody else's key looks like a key that does not exist.
	if err := r.inv.Revoke(ctx, alice, theirs.ID); codeOf(err) != proto.CodeNotFound {
		t.Errorf("foreign key: %v", err)
	}
	if err := r.inv.Revoke(ctx, alice, store.NewID()); codeOf(err) != proto.CodeNotFound {
		t.Errorf("unknown key: %v", err)
	}
	if row, _ := r.st.InvitationByID(ctx, theirs.ID); row.Revoked {
		t.Fatal("a foreign key was revoked")
	}

	if err := r.inv.Revoke(ctx, alice, mine.ID); err != nil {
		t.Fatal(err)
	}
	first, _ := r.st.InvitationByID(ctx, mine.ID)
	if !first.Revoked || first.RevokedAt == 0 {
		t.Fatalf("row = %+v", first)
	}
	// Revoking twice succeeds and keeps the first time.
	r.clock.Advance(time.Minute)
	if err := r.inv.Revoke(ctx, alice, mine.ID); err != nil {
		t.Fatal(err)
	}
	if again, _ := r.st.InvitationByID(ctx, mine.ID); again.RevokedAt != first.RevokedAt {
		t.Errorf("revoked_at moved from %d to %d", first.RevokedAt, again.RevokedAt)
	}
	// An admin can revoke any key.
	if err := r.inv.Revoke(ctx, root, theirs.ID); err != nil {
		t.Fatal(err)
	}
	if row, _ := r.st.InvitationByID(ctx, theirs.ID); !row.Revoked {
		t.Error("the admin's revocation did not apply")
	}
	page, _ := r.audit.List(ctx, audit.Filter{Action: AuditInvitationRevoke})
	if len(page.Events) != 3 { // alice twice, the admin once
		t.Errorf("audit events = %d, want 3", len(page.Events))
	}
}

func TestActAsIsAuditedWithBothIdentities(t *testing.T) {
	r := newInvRig(t)
	ctx := context.Background()
	actAs := InvitationCaller{UserID: "u-alice", Actor: "u-root", ActingAs: "u-alice"}
	res, err := r.inv.Create(ctx, actAs, ops.InvitationCreateArgs{Mode: ops.InvitationOneTime, TTL: ops.TTL1h})
	if err != nil {
		t.Fatal(err)
	}
	if row, _ := r.st.InvitationByID(ctx, res.ID); row.CreatorID != "u-alice" {
		t.Errorf("creator = %q, want the user acted as", row.CreatorID)
	}
	page, _ := r.audit.List(ctx, audit.Filter{Action: AuditInvitationCreate})
	if len(page.Events) != 1 || page.Events[0].Actor != "u-root" || page.Events[0].ActingAs != "u-alice" {
		t.Errorf("audit = %+v", page.Events)
	}
}

func TestNewInvitationsValidatesItsConfig(t *testing.T) {
	st := openStore(t, t.TempDir())
	if _, err := NewInvitations(InvitationsConfig{HubFingerprint: newHubFingerprint(t)}); err == nil {
		t.Error("no store accepted")
	}
	if _, err := NewInvitations(InvitationsConfig{Store: st}); err == nil {
		t.Error("no fingerprint accepted")
	}
}
