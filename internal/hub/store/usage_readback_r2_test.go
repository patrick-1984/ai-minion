package store

import (
	"context"
	"errors"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/ops"
)

// The owner is returned with the verdict, also when the recipient lost its
// eligibility: delivery needs it to tell a lost grant from a reassignment.
func TestReadbackRecipientKeepsItsOwnerWhenIneligible(t *testing.T) {
	f := nomSetup(t)
	ctx := context.Background()
	for _, change := range []string{`UPDATE users SET active=0 WHERE id='bob'`, `UPDATE users SET active=1 WHERE id='bob'`, `DELETE FROM user_roles WHERE user_id='bob'`, `INSERT INTO user_roles(user_id,role,source) VALUES('bob','user','local')`, `UPDATE workers SET approval='pending' WHERE id='w2'`} {
		if _, err := f.s.db.Exec(change); err != nil {
			t.Fatal(err)
		}
		owner, ok, err := f.s.ReadbackRecipientEligible(ctx, "w2")
		if err != nil || owner != "bob" {
			t.Fatalf("after %q: owner %q ok %v err %v", change, owner, ok, err)
		}
	}
	if owner, ok, _ := f.s.ReadbackRecipientEligible(ctx, "nope"); owner != "" || ok {
		t.Fatalf("unknown Worker: %q %v", owner, ok)
	}
}

// A withdrawal that names a bucket clears the slot only while that bucket
// still holds it (personal and Hub-wide); the slot is left alone otherwise.
func TestNominationClearIsConditionalOnTheNamedBucket(t *testing.T) {
	f := nomSetup(t)
	a := f.bucket(t, "alice", "session", "a")
	b := f.bucket(t, "alice", "session", "b")
	for _, scope := range []string{"user", "domain"} {
		if err := f.nominate("alice", scope, "session", b, scope == "domain"); err != nil {
			t.Fatal(err)
		}
		clear := func(bucket, actor string) (ops.UsageNominationSetResult, error) {
			return f.s.SetUsageNomination(context.Background(), "alice", BucketAuthority{Actor: actor, Domain: scope == "domain"},
				ops.UsageNominationSetArgs{Action: "clear", Scope: scope, Kind: "session", BucketID: bucket})
		}
		if _, err := clear(a, "alice"); !errors.Is(err, ErrNominationMoved) {
			t.Fatalf("%s: stale clear: %v", scope, err)
		}
		if n := f.count(t, `scope='`+scope+`' AND bucket_id='`+b+`'`); n != 1 {
			t.Fatalf("%s: the replacement was cleared", scope)
		}
		if res, err := clear(b, "alice"); err != nil || !res.Changed {
			t.Fatalf("%s: matching clear: %+v %v", scope, res, err)
		}
		// An empty slot with a named bucket is simply unchanged.
		if res, err := clear(a, "alice"); err != nil || res.Changed {
			t.Fatalf("%s: clear of an empty slot: %+v %v", scope, res, err)
		}
	}
}

// Another user's invisible copy of a shared subscription does not block the
// publisher's own copy.
func TestNominationIgnoresInvisibleDuplicateOfASharedAccount(t *testing.T) {
	f := nomSetup(t)
	ctx := context.Background()
	b := f.bucket(t, "alice", "provider", "p")
	if _, err := f.s.SetUsageBucketAs(ctx, "alice", f.workers[:1], ops.UsageBucketSetArgs{Action: "assign", Kind: "provider", BucketID: b, All: true}, BucketAuthority{Actor: "alice", Restrict: true, Visible: []string{"w1"}}); err != nil {
		t.Fatal(err)
	}
	// bob has the same subscription on w2 and has not consented.
	_, err := f.s.SetUsageNomination(ctx, "alice", BucketAuthority{Actor: "alice", Restrict: true, Visible: []string{"w1"}},
		ops.UsageNominationSetArgs{Action: "nominate", Scope: "user", Kind: "provider", BucketID: b})
	if err != nil {
		t.Fatalf("an invisible copy blocked the visible one: %v", err)
	}
	// The published document never reads bob's copy.
	src, err := f.s.UsagePublishableSources(ctx, "alice", "provider", b)
	if err != nil || len(src) != 1 || src[0].WorkerID != "w1" {
		t.Fatalf("sources %+v %v", src, err)
	}
}
