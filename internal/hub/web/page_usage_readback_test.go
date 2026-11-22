package web

import (
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/ops"
)

func readbackFixtures(buckets []ops.UsageBucket, allow bool, doc *ops.UsageMeters, slots []ops.UsageDomainSlot) *fakeOps {
	f := newFakeOps().
		reply(ops.OpUsageBucketList, ops.UsageBucketListResult{Buckets: buckets, AllowRepublish: allow}).
		reply(ops.OpUsageNominationList, ops.UsageNominationListResult{Domain: slots}).
		reply(ops.OpUsageNominationSet, ops.UsageNominationSetResult{Changed: true}).
		reply(ops.OpUsagePublicationSet, ops.UsagePublicationSetArgs{})
	if doc != nil {
		f.reply(ops.OpUsageReadback, *doc)
	}
	return f
}

func fp(v float64) *float64 { return &v }

func TestUsageBucketsShowNominationControlsAndReplacement(t *testing.T) {
	f := readbackFixtures([]ops.UsageBucket{
		{ID: "a", Kind: "provider", Label: "Mine", Nominated: []string{"user"}},
		{ID: "b", Kind: "provider", Label: "Other"},
	}, true, &ops.UsageMeters{}, []ops.UsageDomainSlot{{Kind: "provider", BucketID: "x", Label: "Team pool", PublisherName: "alice", UpdatedAt: 1700000000000}})
	connect(t, f)
	body := mainOf(t, onbGet(t, onbAdmin(t), "/usage?view=buckets").Body.String())
	mustHave(t, body, "Show on my Workers", "Show on all Workers", "Withdraw", "Publishing replaces Mine, which currently holds the provider slot.",
		"Publishing replaces Team pool", "Team pool", "Published by", "alice", "Clear this slot", "No bucket is shown on all Workers for this kind.",
		"Your publication consent", "Allowed", "Stop allowing my usage", "separate from Worker monitoring",
		"every approved Worker of this Hub", "your own Workers", "Every local account on a computer that runs a root Worker can read that Worker&#39;s copy.",
		"aggregate meters", "Nothing is published to your Workers yet")
	user := mainOf(t, onbGet(t, onbUser(t), "/usage?view=buckets").Body.String())
	mustHave(t, user, "Show on my Workers")
	mustNotHave(t, user, "Show on all Workers", "Clear this slot")
}

func TestUsageAdminSlotSummaryWithoutBuckets(t *testing.T) {
	connect(t, readbackFixtures(nil, false, &ops.UsageMeters{}, nil))
	body := mainOf(t, onbGet(t, onbAdmin(t), "/usage?view=buckets").Body.String())
	mustHave(t, body, "Provider slot", "Session slot", "Empty", "No token buckets yet.", "Not allowed", "Allow my usage in other people")
}

func TestUsageSecondAdminClearsDomainSlot(t *testing.T) {
	f := readbackFixtures(nil, false, &ops.UsageMeters{}, nil)
	connect(t, f)
	r := onbPost(t, onbAdmin(t), "/usage/nominate", url.Values{"for_user": {onbUserID}, "view": {"buckets"}, "action": {"clear"}, "scope": {"domain"}, "kind": {"session"}, "bucket": {"somebody-elses"}})
	if r.Header().Get("HX-Redirect") != "/usage?view=buckets" {
		t.Fatalf("redirect %q body %s", r.Header().Get("HX-Redirect"), r.Body.String())
	}
	c := f.called(ops.OpUsageNominationSet)
	if len(c) != 1 || c[0].Args["action"] != "clear" || c[0].Args["scope"] != "domain" || c[0].Args["kind"] != "session" || c[0].Args["bucket_id"] != nil {
		t.Fatalf("calls %+v", c)
	}
}

func TestUsageNominationNeedsConfirmationAdminAndOwnAccount(t *testing.T) {
	f := readbackFixtures(nil, false, &ops.UsageMeters{}, nil)
	connect(t, f)
	form := func(scope, confirm string) url.Values {
		v := url.Values{"for_user": {onbUserID}, "view": {"buckets"}, "action": {"nominate"}, "scope": {scope}, "kind": {"provider"}, "bucket": {"a"}}
		if confirm != "" {
			v.Set("confirm", confirm)
		}
		return v
	}
	if r := onbPost(t, onbAdmin(t), "/usage/nominate", form("user", "")); !strings.Contains(r.Body.String(), "confirm what will be published") {
		t.Fatalf("unconfirmed publish: %s", r.Body.String())
	}
	if r := onbPost(t, onbUser(t), "/usage/nominate", form("domain", "yes")); !strings.Contains(r.Body.String(), "Only administrators") {
		t.Fatalf("user domain publish: %s", r.Body.String())
	}
	bad := form("user", "yes")
	bad.Set("for_user", "someone-else")
	onbPost(t, onbUser(t), "/usage/nominate", bad)
	act := onbServer(t, onbSessions{role: authz.RoleAdmin, target: "alice"})
	onbPost(t, act, "/usage/nominate", form("user", "yes"))
	onbPost(t, act, "/usage/publication", url.Values{"for_user": {onbUserID}, "allow_republish": {"yes"}})
	if n := len(f.called(ops.OpUsageNominationSet)); n != 0 || len(f.called(ops.OpUsagePublicationSet)) != 0 {
		t.Fatalf("rejected posts reached the op: %d", n)
	}
	if r := onbPost(t, onbUser(t), "/usage/nominate", form("user", "yes")); r.Header().Get("HX-Redirect") == "" {
		t.Fatalf("confirmed publish: %s", r.Body.String())
	}
	onbPost(t, onbUser(t), "/usage/publication", url.Values{"for_user": {onbUserID}, "allow_republish": {"yes"}})
	if c := f.called(ops.OpUsagePublicationSet); len(c) != 1 || c[0].Args["allow_republish"] != true {
		t.Fatalf("consent %+v", c)
	}
	if c := f.called(ops.OpUsageNominationSet); len(c) != 1 || c[0].Args["bucket_id"] != "a" {
		t.Fatalf("nominate %+v", c)
	}
}

func TestUsageActAsHidesPublication(t *testing.T) {
	f := readbackFixtures(nil, false, &ops.UsageMeters{}, nil)
	connect(t, f)
	body := mainOf(t, onbGet(t, onbServer(t, onbSessions{role: authz.RoleAdmin, target: "alice"}), "/usage?view=buckets").Body.String())
	mustNotHave(t, body, "Show on my Workers", "Shown on all Workers", "Worker read-back preview")
	if len(f.called(ops.OpUsageReadback))+len(f.called(ops.OpUsageNominationList)) != 0 {
		t.Fatal("act-as read private publication state")
	}
}

func TestUsagePreviewRendersMetersAndEscapes(t *testing.T) {
	gen := time.Date(2026, 11, 21, 14, 30, 0, 0, time.UTC)
	reset := gen.Add(2 * time.Hour).UnixMilli()
	doc := ops.UsageMeters{Schema: ops.UsageMetersSchema, GeneratedAt: gen.UnixMilli(), Slots: []ops.UsageSlotDoc{{
		Slot: ops.UsageSlotID{Scope: "user", Kind: "provider", Label: `<script>alert(1)</script>`}, Headline: "m1", OmittedGroups: 2,
		Meters: []ops.UsageMeter{
			{ID: "m1", Label: `<img src=x onerror=1>`, Unit: "requests", Used: fp(30), Remaining: fp(70), Limit: fp(100), ResetsAt: &reset, Pace: "ahead", ObservedAt: gen.UnixMilli(), ExpiresAt: gen.Add(time.Hour).UnixMilli(), Coverage: ops.UsageMeterCoverage{Known: 2, Stale: 1}},
			{ID: "m2", Label: "Old", Unit: "percent", UsedPercent: fp(10), RemainingPercent: fp(90), ObservedAt: gen.Add(-time.Hour).UnixMilli(), ExpiresAt: gen.Add(time.Hour).UnixMilli()},
			{ID: "m3", Label: "Gone", Unit: "tokens", ObservedAt: gen.UnixMilli(), ExpiresAt: gen.UnixMilli()},
		}}}}
	connect(t, readbackFixtures([]ops.UsageBucket{{ID: "a", Kind: "provider", Label: `<b>evil</b>"`, Nominated: []string{"user"}}}, false, &doc, []ops.UsageDomainSlot{{Kind: "session", BucketID: "z", Label: "<i>x</i>", PublisherName: "<u>p</u>"}}))
	body := mainOf(t, onbGet(t, onbAdmin(t), "/usage?view=buckets").Body.String())
	mustHave(t, body, "Worker read-back preview", "Remaining", "70 of 100", "30", "requests", "Ahead of pace", "Fresh", "Stale", "Expired", "90.0%", "2 fresh · 1 stale · 0 unknown", "2 more limit groups", "(headline)", "Unknown pace", "Nov 21, 2026 16:30 UTC")
	mustNotHave(t, body, "<script>alert", "<img src=x", "<b>evil", "<i>x</i>", "<u>p</u>")
	mustHave(t, body, "&lt;script&gt;alert(1)&lt;/script&gt;", "&lt;i&gt;x&lt;/i&gt;", "&lt;u&gt;p&lt;/u&gt;")
}

func TestUsagePreviewEmptyAndUnavailable(t *testing.T) {
	connect(t, readbackFixtures(nil, false, &ops.UsageMeters{Schema: ops.UsageMetersSchema, Slots: []ops.UsageSlotDoc{{Slot: ops.UsageSlotID{Scope: "user", Kind: "session", Label: "Empty one"}}}}, nil))
	body := mainOf(t, onbGet(t, onbUser(t), "/usage?view=buckets").Body.String())
	mustHave(t, body, "Empty one", "No meters in this slot.")
	connect(t, readbackFixtures(nil, false, &ops.UsageMeters{Schema: ops.UsageMetersSchema}, nil))
	mustHave(t, mainOf(t, onbGet(t, onbUser(t), "/usage?view=buckets").Body.String()), "Nothing is published to your Workers yet")
	connect(t, readbackFixtures(nil, false, nil, nil)) // readback op missing: unavailable
	r := onbGet(t, onbUser(t), "/usage?view=buckets")
	if r.Code != 200 {
		t.Fatalf("status %d", r.Code)
	}
	mustHave(t, mainOf(t, r.Body.String()), "The preview is not available right now")
}
