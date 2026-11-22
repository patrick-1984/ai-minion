package server

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/patrick-1984/ai-minion/internal/client"
	"github.com/patrick-1984/ai-minion/internal/hub/api"
	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/ops"
)

func usageMetersSchema(t *testing.T) *jsonschema.Resolved {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "docs", "schemas", "usage-meters.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	var s jsonschema.Schema
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatal(err)
	}
	r, err := s.Resolve(nil)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// decodeMeters validates a produced document against the published schema as
// encoded on the wire, then decodes it.
func decodeMeters(t *testing.T, body string) ops.UsageMeters {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(body), &v); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, body)
	}
	if err := usageMetersSchema(t).Validate(v); err != nil {
		t.Fatalf("document does not match minion.usage-meters/1: %v\n%s", err, body)
	}
	if len(body) > ops.UsageMetersMaxBytes {
		t.Fatalf("document is %d bytes", len(body))
	}
	var doc ops.UsageMeters
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatal(err)
	}
	return doc
}

func slotOf(t *testing.T, doc ops.UsageMeters, scope, kind string) ops.UsageSlotDoc {
	t.Helper()
	for _, s := range doc.Slots {
		if s.Slot.Scope == scope && s.Slot.Kind == kind {
			return s
		}
	}
	t.Fatalf("no %s.%s slot in %+v", scope, kind, doc.Slots)
	return ops.UsageSlotDoc{}
}

func meterOf(t *testing.T, s ops.UsageSlotDoc, metric string) ops.UsageMeter {
	t.Helper()
	for _, m := range s.Meters {
		if m.Metric == metric {
			return m
		}
	}
	t.Fatalf("no meter %s in %+v", metric, s.Meters)
	return ops.UsageMeter{}
}

func rp(v float64) *float64 { return &v }
func rip(v int64) *int64    { return &v }

func TestUsageReadbackPreviewHTTPAndMCP(t *testing.T) {
	dataDir := t.TempDir()
	d, keys := startUsageHTTPHubAt(t, dataDir)
	ctx := context.Background()
	bobID := d.user("bob")
	rootID := d.userID("root")
	root, _ := d.login("root")
	bob, _ := d.login("bob")
	rootWorker := d.enrol("secret-root-host", rootID, store.WorkerKindPrivate)
	bobWorker := d.enrol("secret-bob-host", bobID, store.WorkerKindPrivate)

	now := time.Now()
	sample := now.UnixMilli()
	reset := now.Add(time.Hour).UnixMilli()
	five := int64(5 * time.Hour / time.Millisecond)
	general := func(used float64) ops.UsageWindow {
		return ops.UsageWindow{Scope: "general", Period: "rolling", DurationMS: &five, ResetsAt: &reset, UsedPercent: rp(used)}
	}
	acct := func(name, provider string, observed int64, ws ...ops.UsageWindow) ops.UsageAccount {
		return ops.UsageAccount{ID: ops.UsageID("account", name), Provider: provider, Label: "PRIVATE-LABEL-" + name, AuthMode: "subscription", ObservedAt: observed, Source: "claude-statusline", Windows: ws}
	}
	sess := func(worker, name, account string, in, out *int64) ops.UsageSession {
		return ops.UsageSession{ID: ops.UsageID("session", worker, name), Name: "PRIVATE-SESSION-" + name, Folder: "/private/folder/" + name, AuthMode: "subscription", AccountID: account,
			StartedAt: &sample, ObservedAt: sample, Source: "test-usage", UsageCounters: ops.UsageCounters{InputTokens: in, OutputTokens: out}}
	}
	shared := acct("shared", "Anthropic", sample, general(70))
	claudeRoot := acct("claude-root", "Anthropic", sample, general(40))
	claudeBob := acct("claude-bob", "Anthropic", sample, general(30))
	gemini := acct("gemini", "Google", sample, ops.UsageWindow{Scope: "requests", Period: "fixed", Unit: "requests", Remaining: rp(120)})
	zero := acct("zero", "OpenAI", sample, ops.UsageWindow{Scope: "credits", Period: "fixed", Unit: "tokens", Used: rp(0), Limit: rp(1000), Remaining: rp(1000), UsedPercent: rp(0)})
	old := acct("old", "Cursor", sample-int64(time.Hour/time.Millisecond), general(10))
	save := func(worker, owner string, accounts []ops.UsageAccount, sessions ...ops.UsageSession) {
		t.Helper()
		if err := d.st.SaveUsage(ctx, worker, owner, ops.UsageReport{Version: ops.UsageVersion, ObservedAt: sample, Complete: true, Sessions: sessions, Accounts: accounts}, now); err != nil {
			t.Fatal(err)
		}
	}
	save(rootWorker, rootID, []ops.UsageAccount{shared, claudeRoot, gemini, zero, old},
		sess(rootWorker, "linked", shared.ID, rip(100), rip(50)),
		sess(rootWorker, "unlinked", "", rip(7), nil),
		sess(rootWorker, "ended", "", rip(0), nil))
	save(bobWorker, bobID, []ops.UsageAccount{shared, claudeBob},
		sess(bobWorker, "bobs", shared.ID, rip(1000), rip(2000)))

	call := func(h map[string]string, op string, args any, want int) string {
		t.Helper()
		code, body, _ := d.hc.do(http.MethodPost, ops.RESTPrefix+op, args, h)
		if code != want {
			t.Fatalf("%s returned %d, want %d: %s", op, code, want, body)
		}
		return body
	}
	bucket := func(h map[string]string, kind, label string, members ...string) string {
		t.Helper()
		var res ops.UsageBucketSetResult
		if err := json.Unmarshal([]byte(call(h, ops.OpUsageBucketSet, ops.UsageBucketSetArgs{Action: "save", Kind: kind, Label: label}, http.StatusOK)), &res); err != nil {
			t.Fatal(err)
		}
		call(h, ops.OpUsageBucketSet, ops.UsageBucketSetArgs{Action: "assign", Kind: kind, BucketID: res.ID, Members: members}, http.StatusOK)
		return res.ID
	}
	nominate := func(h map[string]string, scope, kind, id string, want int) {
		t.Helper()
		call(h, ops.OpUsageNominationSet, ops.UsageNominationSetArgs{Action: "nominate", Scope: scope, Kind: kind, BucketID: id}, want)
	}
	hubSession := func(worker, name string) string {
		return ops.UsageID("hub-session", worker, ops.UsageID("session", worker, name))
	}

	// Nothing nominated: an empty, valid document.
	if doc := decodeMeters(t, call(root, ops.OpUsageReadback, ops.UsageReadbackArgs{}, http.StatusOK)); len(doc.Slots) != 0 {
		t.Fatalf("nothing is nominated: %+v", doc)
	}

	// The administrator publishes a session pair and a provider pair to every
	// Worker; Bob keeps a personal pair of his own. Bob's own account is in
	// the Hub-wide provider bucket only after he allows republishing.
	call(bob, ops.OpUsagePublicationSet, ops.UsagePublicationSetArgs{AllowRepublish: true}, http.StatusOK)
	domSessions := bucket(root, "session", "Team builds", hubSession(rootWorker, "linked"), hubSession(rootWorker, "unlinked"), hubSession(rootWorker, "ended"))
	domProviders := bucket(root, "provider", "Team plans", claudeRoot.ID, claudeBob.ID, gemini.ID, zero.ID, old.ID)
	rootPersonal := bucket(root, "provider", "Root private plan", shared.ID)
	bobSessions := bucket(bob, "session", "Bob sessions", hubSession(bobWorker, "bobs"))
	bobProviders := bucket(bob, "provider", "Bob plans", claudeBob.ID)
	nominate(root, "domain", "session", domSessions, http.StatusOK)
	nominate(root, "domain", "provider", domProviders, http.StatusOK)
	nominate(root, "user", "provider", rootPersonal, http.StatusOK)
	nominate(bob, "user", "session", bobSessions, http.StatusOK)
	nominate(bob, "user", "provider", bobProviders, http.StatusOK)

	rootDoc := decodeMeters(t, call(root, ops.OpUsageReadback, ops.UsageReadbackArgs{}, http.StatusOK))
	bobBody := call(bob, ops.OpUsageReadback, ops.UsageReadbackArgs{}, http.StatusOK)
	bobDoc := decodeMeters(t, bobBody)

	// Four slots at most; the caller's own pair plus the domain pair, never
	// somebody else's.
	if len(rootDoc.Slots) != 3 || len(bobDoc.Slots) != 4 {
		t.Fatalf("slots: root %d, bob %d", len(rootDoc.Slots), len(bobDoc.Slots))
	}
	var order []string
	for _, s := range bobDoc.Slots {
		order = append(order, s.Slot.Scope+"."+s.Slot.Kind)
	}
	if strings.Join(order, ",") != "domain.session,domain.provider,user.session,user.provider" {
		t.Fatalf("slot order %v", order)
	}
	if strings.Contains(bobBody, "Root private plan") {
		t.Fatal("Bob's preview shows the administrator's personal slot")
	}

	// Provider slot: the unweighted mean of two accounts that differ in
	// capacity; amount-only, known zero, stale.
	prov := slotOf(t, bobDoc, "domain", "provider")
	claude := meterOf(t, prov, "anthropic.general.5h")
	if *claude.UsedPercent != 35 || claude.Coverage.Known != 2 || !strings.Contains(claude.Label, "unweighted mean of 2") {
		t.Fatalf("mean: %+v", claude)
	}
	amt := meterOf(t, prov, "google.requests.fixed")
	if amt.Unit != "requests" || *amt.Remaining != 120 || amt.RemainingPercent != nil || amt.Used != nil {
		t.Fatalf("native amount-only window: %+v", amt)
	}
	z := meterOf(t, prov, "openai.credits.fixed")
	if z.Used == nil || *z.Used != 0 || *z.Remaining != 1000 || *z.RemainingPercent != 100 {
		t.Fatalf("known zero: %+v", z)
	}
	st := meterOf(t, prov, "cursor.general.5h")
	if st.UsedPercent != nil || st.Coverage.Stale != 1 || st.ExpiresAt > sample+1000 {
		t.Fatalf("expired meter carries a value: %+v", st)
	}
	if prov.Headline != claude.ID {
		t.Fatalf("headline %q, want the fresh lowest remaining %q", prov.Headline, claude.ID)
	}
	if prov.Slot.Label != "Team plans" {
		t.Fatalf("label %q", prov.Slot.Label)
	}

	// Session slot: measured consumption of the selected sessions only, and the
	// shared account's quota kept apart. Bob's session on the same account is
	// not in this bucket and is not counted.
	ses := slotOf(t, rootDoc, "domain", "session")
	tok := meterOf(t, ses, "sessions.tokens")
	if *tok.Used != 157 || tok.Coverage.Known != 3 || tok.Unit != "tokens" {
		t.Fatalf("tokens of the selected sessions: %+v", tok)
	}
	if ses.Headline != "" {
		t.Fatalf("account-wide quota became a session headline: %q", ses.Headline)
	}
	q := meterOf(t, ses, "linked.anthropic.general.5h")
	if *q.UsedPercent != 70 || !strings.HasPrefix(q.Label, "Account-wide:") {
		t.Fatalf("linked quota: %+v", q)
	}
	if bobSes := meterOf(t, slotOf(t, bobDoc, "user", "session"), "sessions.tokens"); *bobSes.Used != 3000 {
		t.Fatalf("Bob's own session: %+v", bobSes)
	}
	if meterOf(t, slotOf(t, bobDoc, "user", "session"), "linked.anthropic.general.5h").Coverage.Known != 1 {
		t.Fatal("the shared account is not reported for Bob's session")
	}

	// Nothing private leaves the Hub.
	for _, doc := range []string{bobBody, call(root, ops.OpUsageReadback, ops.UsageReadbackArgs{}, http.StatusOK)} {
		for _, secret := range []string{"PRIVATE-LABEL", "PRIVATE-SESSION", "/private/folder", "secret-root-host", "secret-bob-host", rootWorker, bobWorker, rootID, bobID,
			shared.ID, claudeRoot.ID, hubSession(rootWorker, "linked"), "key_hint", "worker_id", "account_id", "member"} {
			if strings.Contains(doc, secret) {
				t.Fatalf("the document leaks %q: %s", secret, doc)
			}
		}
	}

	// Authorization over the real HTTP stack.
	call(nil, ops.OpUsageReadback, ops.UsageReadbackArgs{}, http.StatusUnauthorized)
	readKey, restrictedKey := identity.NewAPIKey().String(), identity.NewAPIKey().String()
	for token, scope := range map[string]nominationScope{
		readKey:       {actions: []ops.Action{ops.ActionRead}, all: true},
		restrictedKey: {actions: []ops.Action{ops.ActionRead}, all: false},
	} {
		keys.mu.Lock()
		keys.known[token] = api.KeyIdentity{KeyID: token, Principal: authz.NewAPIKeyPrincipal(bobID, scope, "")}
		keys.mu.Unlock()
	}
	if keyDoc := call(asKey(readKey), ops.OpUsageReadback, ops.UsageReadbackArgs{}, http.StatusOK); decodeMeters(t, keyDoc).Slots == nil || len(decodeMeters(t, keyDoc).Slots) != 4 {
		t.Fatalf("a full-scope read key sees its owner's document: %s", keyDoc)
	}
	call(asKey(restrictedKey), ops.OpUsageReadback, ops.UsageReadbackArgs{}, http.StatusForbidden)
	call(root, ops.OpActAsStart, map[string]any{"request_id": ops.NewUUIDv7(), "user_id": bobID, "duration_minutes": 5}, http.StatusOK)
	call(root, ops.OpUsageReadback, ops.UsageReadbackArgs{}, http.StatusForbidden)
	call(root, ops.OpActAsStop, map[string]any{}, http.StatusOK)

	// MCP over the Hub backend, with the same keys: the tool exists, answers
	// the owner and refuses the restricted key.
	caFile := filepath.Join(dataDir, TLSDir, TLSCertFile)
	mcpFor := func(token string) *mcpSession {
		kf := keyFile(t, token)
		b, err := client.NewHub(client.HubConfig{URL: "https://" + d.h.srv.Addr(), APIKeyFile: kf, CAFile: caFile})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(b.Close)
		return newMCPSession(t, b, false)
	}
	text, isErr := callMCP(t, mcpFor(readKey), ops.OpUsageReadback, map[string]any{})
	if isErr || len(decodeMeters(t, text).Slots) != 4 {
		t.Fatalf("MCP preview: err=%v %s", isErr, text)
	}
	if text, isErr = callMCP(t, mcpFor(restrictedKey), ops.OpUsageReadback, map[string]any{}); !isErr {
		t.Fatalf("MCP served a restricted key: %s", text)
	}

	// The cache keeps the aggregation for 30 s but never the authority behind
	// it: a withdrawn nomination and a withdrawn permission act at once.
	first := call(bob, ops.OpUsageReadback, ops.UsageReadbackArgs{}, http.StatusOK)
	if first != call(bob, ops.OpUsageReadback, ops.UsageReadbackArgs{}, http.StatusOK) {
		t.Fatal("an unchanged document was rebuilt inside the cache window")
	}
	call(bob, ops.OpUsagePublicationSet, ops.UsagePublicationSetArgs{AllowRepublish: false}, http.StatusOK)
	after := decodeMeters(t, call(bob, ops.OpUsageReadback, ops.UsageReadbackArgs{}, http.StatusOK))
	if m := meterOf(t, slotOf(t, after, "domain", "provider"), "anthropic.general.5h"); *m.UsedPercent != 40 || m.Coverage.Known != 1 {
		t.Fatalf("Bob withdrew republishing but his observation still counts: %+v", m)
	}
	call(root, ops.OpUsageNominationSet, ops.UsageNominationSetArgs{Action: "clear", Scope: "domain", Kind: "provider"}, http.StatusOK)
	after = decodeMeters(t, call(bob, ops.OpUsageReadback, ops.UsageReadbackArgs{}, http.StatusOK))
	if len(after.Slots) != 3 {
		t.Fatalf("a withdrawn domain slot is still published: %+v", after.Slots)
	}
}

func TestUsageReadbackMoreThanSixteenGroups(t *testing.T) {
	d, _ := startUsageHTTPHub(t)
	ctx := context.Background()
	bobID := d.user("bob")
	bob, _ := d.login("bob")
	worker := d.enrol("bob-host", bobID, store.WorkerKindPrivate)
	now := time.Now()
	sample := now.UnixMilli()
	reset := now.Add(time.Hour).UnixMilli()
	five := int64(5 * time.Hour / time.Millisecond)
	var ws []ops.UsageWindow
	for i := 0; i < 20; i++ {
		// The last scope by name has the least remaining.
		ws = append(ws, ops.UsageWindow{Scope: string(rune('a'+i)) + "-scope", Period: "rolling", DurationMS: &five, ResetsAt: &reset, UsedPercent: rp(float64(10 + 4*i))})
	}
	a := ops.UsageAccount{ID: ops.UsageID("account", "many"), Provider: "Anthropic", AuthMode: "subscription", ObservedAt: sample, Source: "claude-statusline", Windows: ws}
	if err := d.st.SaveUsage(ctx, worker, bobID, ops.UsageReport{Version: ops.UsageVersion, ObservedAt: sample, Complete: true, Accounts: []ops.UsageAccount{a}}, now); err != nil {
		t.Fatal(err)
	}
	call := func(op string, args any) string {
		t.Helper()
		code, body, _ := d.hc.do(http.MethodPost, ops.RESTPrefix+op, args, bob)
		if code != http.StatusOK {
			t.Fatalf("%s: %d %s", op, code, body)
		}
		return body
	}
	var res ops.UsageBucketSetResult
	_ = json.Unmarshal([]byte(call(ops.OpUsageBucketSet, ops.UsageBucketSetArgs{Action: "save", Kind: "provider", Label: "Many"})), &res)
	call(ops.OpUsageBucketSet, ops.UsageBucketSetArgs{Action: "assign", Kind: "provider", BucketID: res.ID, Members: []string{a.ID}})
	call(ops.OpUsageNominationSet, ops.UsageNominationSetArgs{Action: "nominate", Scope: "user", Kind: "provider", BucketID: res.ID})
	doc := decodeMeters(t, call(ops.OpUsageReadback, ops.UsageReadbackArgs{}))
	s := slotOf(t, doc, "user", "provider")
	if len(s.Meters) != 16 || s.OmittedGroups != 4 || s.Headline == "" || s.Meters[0].ID != s.Headline {
		t.Fatalf("%d meters, omitted %d, headline %q", len(s.Meters), s.OmittedGroups, s.Headline)
	}
	if !strings.Contains(s.Headline, "t-scope") || *s.Meters[0].RemainingPercent != 14 {
		t.Fatalf("the most exhausted allowance was truncated away: %+v", s.Meters[0])
	}
}
