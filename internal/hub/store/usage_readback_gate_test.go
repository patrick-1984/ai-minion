package store

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/ops"
)

func TestReadbackRecipientMustBeActiveWithAGrant(t *testing.T) {
	f := nomSetup(t)
	ctx := context.Background()
	b := f.bucket(t, "alice", "session", "mine")
	if err := f.nominate("alice", "user", "session", b, false); err != nil {
		t.Fatal(err)
	}
	if err := f.nominate("alice", "domain", "session", b, true); err != nil {
		t.Fatal(err)
	}
	owner, ok, err := f.s.ReadbackRecipientEligible(ctx, "w2")
	if err != nil || !ok || owner != "bob" {
		t.Fatalf("approved Worker of an active account with a grant: %q %v %v", owner, ok, err)
	}
	if specs, _ := f.s.UsageReadbackSlots(ctx, "bob"); len(specs) != 1 || specs[0].Scope != "domain" {
		t.Fatalf("bob receives the Hub-wide slot: %+v", specs)
	}
	// Offboarded: the account is inactive. Nothing is delivered any more.
	if _, err := f.s.db.Exec(`UPDATE users SET active=0 WHERE id='bob'`); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := f.s.ReadbackRecipientEligible(ctx, "w2"); ok {
		t.Fatal("an inactive owner's Worker is still eligible")
	}
	if specs, _ := f.s.UsageReadbackSlots(ctx, "bob"); len(specs) != 0 {
		t.Fatalf("an inactive recipient still has slots: %+v", specs)
	}
	// Active again but without any grant.
	if _, err := f.s.db.Exec(`UPDATE users SET active=1 WHERE id='bob'`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.db.Exec(`DELETE FROM user_roles WHERE user_id='bob'`); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := f.s.ReadbackRecipientEligible(ctx, "w2"); ok {
		t.Fatal("an owner without a grant is eligible")
	}
	if specs, _ := f.s.UsageReadbackSlots(ctx, "bob"); len(specs) != 0 {
		t.Fatalf("a recipient without a grant has slots: %+v", specs)
	}
	// A Worker that is not approved is never eligible.
	if _, err := f.s.db.Exec(`INSERT INTO user_roles(user_id,role,source) VALUES('bob','user','local')`); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := f.s.ReadbackRecipientEligible(ctx, "w2"); !ok {
		t.Fatal("restored grant")
	}
	if _, err := f.s.db.Exec(`UPDATE workers SET approval='pending' WHERE id='w2'`); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := f.s.ReadbackRecipientEligible(ctx, "w2"); ok {
		t.Fatal("a pending Worker is eligible")
	}
	if _, ok, _ := f.s.ReadbackRecipientEligible(ctx, "nope"); ok {
		t.Fatal("an unknown Worker is eligible")
	}
}

func TestReadbackSnapshotFollowsConsentAndPublisherAccess(t *testing.T) {
	f := nomSetup(t)
	ctx := context.Background()
	b := f.bucket(t, "alice", "session", "mixed")
	for _, m := range f.rows {
		if _, err := f.s.SetUsageBucket(ctx, "alice", f.workers, ops.UsageBucketSetArgs{Action: "assign", Kind: "session", BucketID: b, Members: []string{m}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.s.SetUsagePublication(ctx, "bob", true); err != nil {
		t.Fatal(err)
	}
	if err := f.nominate("alice", "user", "session", b, false); err != nil {
		t.Fatal(err)
	}
	read := func(visible map[string][]string) []ReadbackSlot {
		t.Helper()
		slots, err := f.s.UsageReadbackSnapshot(ctx, "alice", visible, true)
		if err != nil {
			t.Fatal(err)
		}
		return slots
	}
	// With access to bob's Worker and his consent, the foreign session is in.
	if got := read(map[string][]string{"alice": {"w1", "w2"}}); len(got) != 1 || len(got[0].Sources) != 2 || len(got[0].Data.Sessions) != 2 {
		t.Fatalf("%+v", got)
	}
	// The publisher lost access to bob's Worker (or nobody resolved it): the
	// foreign source and its observations are gone, the own source stays.
	for _, visible := range []map[string][]string{{"alice": {"w1"}}, {"alice": nil}, {}} {
		got := read(visible)
		if len(got) != 1 || len(got[0].Sources) != 1 || got[0].Sources[0].WorkerID != "w1" || len(got[0].Data.Sessions) != 1 {
			t.Fatalf("foreign source survived lost access %v: %+v", visible, got)
		}
	}
	// Consent withdrawn: the sources and the observations come from the same
	// snapshot, so none of bob's rows is read.
	if err := f.s.SetUsagePublication(ctx, "bob", false); err != nil {
		t.Fatal(err)
	}
	if got := read(map[string][]string{"alice": {"w1", "w2"}}); len(got[0].Sources) != 1 || len(got[0].Data.Sessions) != 1 {
		t.Fatalf("withdrawn consent: %+v", got)
	}
	// Nominating validates with the same rule.
	if err := f.s.SetUsagePublication(ctx, "bob", true); err != nil {
		t.Fatal(err)
	}
	c := f.bucket(t, "alice", "session", "again")
	if _, err := f.s.SetUsageBucket(ctx, "alice", f.workers, ops.UsageBucketSetArgs{Action: "assign", Kind: "session", BucketID: c, Members: []string{f.rows[1]}}); err != nil {
		t.Fatal(err)
	}
	set := func(visible []string) error {
		_, err := f.s.SetUsageNomination(ctx, "alice", BucketAuthority{Actor: "alice", Restrict: true, Visible: visible},
			ops.UsageNominationSetArgs{Action: "nominate", Scope: "user", Kind: "session", BucketID: c})
		return err
	}
	if err := set([]string{"w1"}); err == nil {
		t.Fatal("nominated a bucket whose foreign source the publisher cannot view")
	}
	if err := set([]string{"w1", "w2"}); err != nil {
		t.Fatal(err)
	}
}

func TestReadbackUnchangedAdminRoleKeepsTheDomainNomination(t *testing.T) {
	f := nomSetup(t)
	ctx := context.Background()
	b := f.bucket(t, "alice", "session", "a")
	if err := f.nominate("alice", "domain", "session", b, true); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if eff, err := f.s.SetLocalRole(ctx, SetLocalRoleParams{UserID: "alice", Role: "admin", NowMS: 5}); err != nil || eff != "admin" {
			t.Fatalf("%q %v", eff, err)
		}
	}
	if n := f.count(t, `scope='domain' AND owner_id='alice'`); n != 1 {
		t.Fatalf("an unchanged admin role retired the nomination (%d left)", n)
	}
	// A real loss still retires it.
	if _, err := f.s.SetLocalRole(ctx, SetLocalRoleParams{UserID: "alice", Role: "user", NowMS: 6}); err != nil {
		t.Fatal(err)
	}
	if n := f.count(t, `scope='domain' AND owner_id='alice'`); n != 0 {
		t.Fatalf("demotion left %d nomination(s)", n)
	}
}

func TestReadbackDiscoveryRowDoesNotEclipseTheCollector(t *testing.T) {
	f := nomSetup(t)
	var id string
	if err := f.s.db.QueryRow(`SELECT account_id FROM usage_accounts WHERE worker_id='w1'`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	five := int64(18000000)
	pct := 30.0
	withWindows := ops.UsageAccount{ID: id, Provider: "Anthropic", AuthMode: "subscription", Source: "claude-statusline", Windows: []ops.UsageWindow{{Scope: "general", Period: "rolling", DurationMS: &five, UsedPercent: &pct}}}
	discovery := ops.UsageAccount{ID: id, Provider: "Anthropic", AuthMode: "subscription", Source: "native-account", Windows: []ops.UsageWindow{}}
	put := func(worker string, a ops.UsageAccount, at int64) {
		t.Helper()
		body, _ := json.Marshal(a)
		if _, err := f.s.db.Exec(`UPDATE usage_accounts SET body=?,observed_at=? WHERE worker_id=? AND account_id=?`, string(body), at, worker, id); err != nil {
			t.Fatal(err)
		}
	}
	put("w1", withWindows, 1000)
	put("w2", discovery, 9000) // newer, but only discovery
	got, err := readUsageAccounts(context.Background(), f.s.db, []usageSourcePair{{id, "w1"}, {id, "w2"}})
	if err != nil || len(got) != 1 || len(got[0].Windows) != 1 || got[0].ObservedAt != 1000 {
		t.Fatalf("the newer discovery-only row won: %+v %v", got, err)
	}
}

func TestReadbackAuditNamesTheKey(t *testing.T) {
	f := nomSetup(t)
	ctx := context.Background()
	b := f.bucket(t, "alice", "session", "a")
	if _, err := f.s.SetUsageNomination(ctx, "alice", BucketAuthority{Actor: "alice", Credential: "k123"},
		ops.UsageNominationSetArgs{Action: "nominate", Scope: "user", Kind: "session", BucketID: b}); err != nil {
		t.Fatal(err)
	}
	if err := f.s.SetUsagePublicationAs(ctx, "alice", "k456", true); err != nil {
		t.Fatal(err)
	}
	if err := f.s.SetUsagePublicationAs(ctx, "alice", "", false); err != nil {
		t.Fatal(err)
	}
	rows, err := f.s.db.Query(`SELECT action,actor FROM audit_events WHERE action LIKE 'usage.%' ORDER BY ts,id`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var got []string
	for rows.Next() {
		var a, actor string
		_ = rows.Scan(&a, &actor)
		got = append(got, a+"="+actor)
	}
	want := "usage.nomination.set=key:k123,usage.publication.set=key:k456,usage.publication.set=alice"
	if strings.Join(got, ",") != want {
		t.Fatalf("audit actors %v", got)
	}
}
