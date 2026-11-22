package server

import (
	"context"
	"encoding/json"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/hub/workers"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// Against the real store: an owner who loses their grant keeps the connection,
// the Worker is told to clear what it holds, and a restored grant brings the
// document back.
func TestUsageReadbackOwnerLosingTheGrantGetsTheClearNotADisconnect(t *testing.T) {
	d, _ := startUsageHTTPHub(t)
	alice, bob := d.user("alice"), d.user("bob")
	wid := d.enrol("alice-laptop", alice, store.WorkerKindPrivate)
	w := d.connectReadbackWorker(wid, alice, rbFeatures, false, nil)
	d.connectReadbackWorker(d.enrol("bob-laptop", bob, store.WorkerKindPrivate), bob, rbFeatures, false, nil)
	rbEventually(t, "the first document", func() bool { return len(w.got()) == 1 })

	// Any usage change wakes every loop; Bob changes his consent.
	bobHdr, _ := d.login("bob")
	wake := func() {
		t.Helper()
		code, body, _ := d.hc.do(http.MethodPost, ops.RESTPrefix+ops.OpUsagePublicationSet, ops.UsagePublicationSetArgs{AllowRepublish: false}, bobHdr)
		if code != http.StatusOK {
			t.Fatalf("%d %s", code, body)
		}
	}
	if _, err := d.st.DB().Exec(`DELETE FROM user_roles WHERE user_id=?`, alice); err != nil {
		t.Fatal(err)
	}
	wake()
	rbEventually(t, "the clear", func() bool { return len(w.got()) == 2 })
	if f := w.got()[1]; !f.Clear || f.OwnerID != alice || (len(f.Doc) != 0 && string(f.Doc) != "null") {
		t.Fatalf("clear frame = %+v", f)
	}
	// The connection lives on: the restored grant gets a document again.
	if _, err := d.st.DB().Exec(`INSERT INTO user_roles(user_id,role,source) VALUES(?, 'user','local')`, alice); err != nil {
		t.Fatal(err)
	}
	wake()
	rbEventually(t, "the document after the grant is back", func() bool { return len(w.got()) == 3 })
	if f := w.got()[2]; f.Clear || len(f.Doc) < 10 {
		t.Fatalf("resumed frame = %+v", f)
	}
}

type readbackAuthorityFixture struct {
	d          *dashHub
	rb         *usageReadback
	alice, bob string
	wa, wb     string
}

// alice publishes a bucket of her own account and bob's consenting one.
func newReadbackAuthorityFixture(t *testing.T) *readbackAuthorityFixture {
	t.Helper()
	d, _ := startUsageHTTPHub(t)
	f := &readbackAuthorityFixture{d: d, rb: newUsageReadback(d.st), alice: d.user("alice"), bob: d.user("bob")}
	f.wa = d.enrol("a", f.alice, store.WorkerKindPrivate)
	f.wb = d.enrol("b", f.bob, store.WorkerKindPrivate)
	ctx := context.Background()
	now := time.Now()
	reset := now.Add(time.Hour).UnixMilli()
	five := int64(5 * time.Hour / time.Millisecond)
	acct := func(name string) ops.UsageAccount {
		return ops.UsageAccount{ID: ops.UsageID("account", name), Provider: "Anthropic", AuthMode: "subscription", ObservedAt: now.UnixMilli(), Source: "claude-statusline",
			Windows: []ops.UsageWindow{{Scope: "general", Period: "rolling", DurationMS: &five, ResetsAt: &reset, UsedPercent: rp(10)}}}
	}
	for w, o := range map[string]string{f.wa: f.alice, f.wb: f.bob} {
		name := "mine"
		if o == f.bob {
			name = "his"
		}
		if err := d.st.SaveUsage(ctx, w, o, ops.UsageReport{Version: ops.UsageVersion, ObservedAt: now.UnixMilli(), Complete: true, Accounts: []ops.UsageAccount{acct(name)}}, now); err != nil {
			t.Fatal(err)
		}
	}
	if err := d.st.SetUsagePublication(ctx, f.bob, true); err != nil {
		t.Fatal(err)
	}
	both := store.BucketAuthority{Actor: f.alice, Restrict: true, Visible: []string{f.wa, f.wb}}
	res, err := d.st.SetUsageBucketAs(ctx, f.alice, []ops.WorkerInfo{{ID: f.wa, OwnerID: f.alice}, {ID: f.wb, OwnerID: f.bob}}, ops.UsageBucketSetArgs{Action: "save", Kind: "provider", Label: "P"}, both)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.st.SetUsageBucketAs(ctx, f.alice, []ops.WorkerInfo{{ID: f.wa, OwnerID: f.alice}, {ID: f.wb, OwnerID: f.bob}}, ops.UsageBucketSetArgs{Action: "assign", Kind: "provider", BucketID: res.ID, All: true}, both); err != nil {
		t.Fatal(err)
	}
	if _, err := d.st.SetUsageNomination(ctx, f.alice, both, ops.UsageNominationSetArgs{Action: "nominate", Scope: "user", Kind: "provider", BucketID: res.ID}); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *readbackAuthorityFixture) known(t *testing.T) int {
	t.Helper()
	doc, _, err := f.rb.Document(context.Background(), f.alice)
	if err != nil || len(doc.Slots) != 1 || len(doc.Slots[0].Meters) != 1 {
		t.Fatalf("%+v %v", doc, err)
	}
	return doc.Slots[0].Meters[0].Coverage.Known
}

// A publisher whose view of another user's Worker ends while the document is
// built (a demotion after the lookup) never gets that user's new data in.
func TestUsageReadbackResolvesPublisherVisibilityAgainAfterTheSnapshot(t *testing.T) {
	f := newReadbackAuthorityFixture(t)
	var calls atomic.Int32
	f.rb.access = func(context.Context, string) ([]string, error) {
		if calls.Add(1) <= 2 { // the lookups before the snapshot
			return []string{f.wa, f.wb}, nil
		}
		return []string{f.wa}, nil // the publisher lost the view meanwhile
	}
	if n := f.known(t); n != 1 {
		t.Fatalf("known %d: the former view still reached the document", n)
	}
}

// A consent withdrawn while the document is being built does not leave with
// the document: it is built again.
func TestUsageReadbackDiscardsADocumentBuiltAcrossAConsentChange(t *testing.T) {
	f := newReadbackAuthorityFixture(t)
	var calls atomic.Int32
	f.rb.access = func(context.Context, string) ([]string, error) {
		if calls.Add(1) == 3 { // after the snapshot transaction
			f.rb.bump()
			if err := f.d.st.SetUsagePublication(context.Background(), f.bob, false); err != nil {
				t.Error(err)
			}
			f.rb.bump()
		}
		return []string{f.wa, f.wb}, nil
	}
	if n := f.known(t); n != 1 {
		t.Fatalf("known %d: a withdrawn consent outlasted the build", n)
	}
}

// Over HTTP: a withdrawal that names a bucket which no longer holds the slot
// is refused with a conflict and leaves the replacement published.
func TestUsageNominationWithdrawalOfAReplacedBucketIsRefused(t *testing.T) {
	d, _ := startUsageHTTPHub(t)
	d.user("alice")
	hdr, _ := d.login("alice")
	call := func(op string, args any, want int) string {
		t.Helper()
		code, body, _ := d.hc.do(http.MethodPost, ops.RESTPrefix+op, args, hdr)
		if code != want {
			t.Fatalf("%s: %d, want %d: %s", op, code, want, body)
		}
		return body
	}
	mk := func(label string) string {
		var res ops.UsageBucketSetResult
		if err := json.Unmarshal([]byte(call(ops.OpUsageBucketSet, ops.UsageBucketSetArgs{Action: "save", Kind: "session", Label: label}, http.StatusOK)), &res); err != nil {
			t.Fatal(err)
		}
		return res.ID
	}
	a, b := mk("A"), mk("B")
	set := func(action, bucket string, want int) {
		call(ops.OpUsageNominationSet, ops.UsageNominationSetArgs{Action: action, Scope: "user", Kind: "session", BucketID: bucket}, want)
	}
	set("nominate", a, http.StatusOK)
	set("nominate", b, http.StatusOK) // replaces A
	set("clear", a, http.StatusConflict)
	set("clear", b, http.StatusOK)
}

// The clock that picks the headline is read after the snapshot, so a meter
// that reset while the data was read is not offered as the headline.
func TestUsageReadbackSamplesTheClockAfterTheSnapshot(t *testing.T) {
	f := newReadbackAuthorityFixture(t)
	start := time.Now()
	var read, calls atomic.Int32
	f.rb.now = func() time.Time {
		if read.Load() == 0 {
			return start
		}
		return start.Add(2 * time.Hour) // after the read: the 1-hour windows have reset
	}
	f.rb.access = func(context.Context, string) ([]string, error) {
		if calls.Add(1) == 3 { // the lookup after the snapshot transaction
			read.Store(1)
		}
		return []string{f.wa, f.wb}, nil
	}
	doc, _, err := f.rb.Document(context.Background(), f.alice)
	if err != nil || len(doc.Slots) != 1 {
		t.Fatalf("%+v %v", doc, err)
	}
	if doc.GeneratedAt != start.Add(2*time.Hour).UnixMilli() || doc.Slots[0].Headline != "" {
		t.Fatalf("generated %d headline %q", doc.GeneratedAt, doc.Slots[0].Headline)
	}
}

// A document whose stream opened after the recipient lost its entitlement is
// not written; the Worker gets the clear instead.
func TestDeliverySendsTheClearWhenTheGuardRefusesAnOpenedStream(t *testing.T) {
	r := newDeliveryRig(t)
	r.send.script = func(n int, f proto.ReadbackApply) error {
		if !f.Clear {
			return workers.ErrReadbackIneligible
		}
		return nil
	}
	r.start()
	rbWait(t, "the clear", func() bool { return len(r.send.got()) >= 2 })
	got := r.send.got()
	if got[0].Clear || !got[1].Clear || got[1].Doc != nil || got[1].Revision <= got[0].Revision {
		t.Fatalf("frames %+v", got)
	}
}
