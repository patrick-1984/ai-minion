package ops

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
)

func usageMetersSchema(t *testing.T) *jsonschema.Resolved {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "docs", "schemas", "usage-meters.schema.json"))
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

// validateMeters checks the document as encoded, the way a reader sees it.
func validateMeters(t *testing.T, doc UsageMeters) {
	t.Helper()
	b, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if len(b) > UsageMetersMaxBytes {
		t.Fatalf("document is %d bytes", len(b))
	}
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	if err := usageMetersSchema(t).Validate(v); err != nil {
		t.Fatalf("document does not match minion.usage-meters/1: %v\n%s", err, b)
	}
}

func fp(v float64) *float64 { return &v }
func ip(v int64) *int64     { return &v }

func rbAccount(id, provider string, observed time.Time, ws ...UsageWindow) UsageAccount {
	return UsageAccount{ID: UsageID("account", id), Provider: provider, AuthMode: "subscription", ObservedAt: observed.UnixMilli(), Windows: ws}
}

// rbMetric is the public metric name of a provider's window: the fixed family,
// the opaque scope token and the span.
func rbMetric(provider, scope, span string) string {
	fam, _ := usageFamily(UsageProvider(provider))
	return fam + "." + usageScopeToken(UsageProvider(provider), scope) + "." + span
}

func meterByMetric(ms []UsageMeter, metric string) *UsageMeter {
	for i := range ms {
		if ms[i].Metric == metric {
			return &ms[i]
		}
	}
	return nil
}

func TestProviderMetersEachQuantityIndependently(t *testing.T) {
	now := time.Now()
	reset := now.Add(time.Hour).UnixMilli()
	five := int64(5 * time.Hour / time.Millisecond)
	pct := func(v float64) UsageWindow {
		return UsageWindow{Scope: "general", Period: "rolling", DurationMS: &five, ResetsAt: &reset, UsedPercent: fp(v)}
	}
	t.Run("unequal capacities are an unweighted mean", func(t *testing.T) {
		// A small and a large plan: the mean is not weighted by capacity and no
		// token count is ever derived from the percentages.
		ms, un := ProviderMeters([]UsageAccount{
			rbAccount("small", "anthropic", now, pct(10)), rbAccount("large", "Anthropic", now, pct(30)),
		}, now)
		if un != 0 || len(ms) != 1 {
			t.Fatalf("%+v %d", ms, un)
		}
		m := ms[0]
		if *m.UsedPercent != 20 || *m.RemainingPercent != 80 || m.Unit != "percent" || m.Used != nil || m.Remaining != nil || m.Limit != nil {
			t.Fatalf("%+v", m)
		}
		if !strings.Contains(m.Label, "unweighted mean of 2") || m.Coverage != (UsageMeterCoverage{Known: 2}) {
			t.Fatalf("label/coverage: %q %+v", m.Label, m.Coverage)
		}
		if *m.ResetsAt != reset || *m.DurationMS != five || m.Metric != rbMetric("Anthropic", "general", "5h") {
			t.Fatalf("%+v", m)
		}
	})
	t.Run("native amount-only window, no percentage", func(t *testing.T) {
		w := UsageWindow{Scope: "requests", Period: "fixed", Unit: "requests", Remaining: fp(120)}
		ms, _ := ProviderMeters([]UsageAccount{rbAccount("g", "google", now, w)}, now)
		m := ms[0]
		if m.Unit != "requests" || *m.Remaining != 120 || m.RemainingPercent != nil || m.UsedPercent != nil || m.Used != nil || m.Limit != nil {
			t.Fatalf("%+v", m)
		}
		if UsageHeadline(ms, now) != "" {
			t.Fatal("an amount without a percentage became a headline")
		}
	})
	t.Run("known zero is kept, unknown is absent", func(t *testing.T) {
		w := UsageWindow{Scope: "requests", Period: "fixed", Unit: "tokens", Used: fp(0), Remaining: fp(0), UsedPercent: fp(0)}
		ms, _ := ProviderMeters([]UsageAccount{rbAccount("z", "x", now, w)}, now)
		b, _ := json.Marshal(ms[0])
		for _, want := range []string{`"used":0`, `"remaining":0`, `"used_percent":0`, `"remaining_percent":100`} {
			if !strings.Contains(string(b), want) {
				t.Fatalf("%s lacks %s", b, want)
			}
		}
		if strings.Contains(string(b), `"limit"`) {
			t.Fatalf("limit invented: %s", b)
		}
	})
	t.Run("amounts need every account, limits are never inferred", func(t *testing.T) {
		a := UsageWindow{Scope: "q", Period: "fixed", Unit: "tokens", Used: fp(10), Limit: fp(100), Remaining: fp(90)}
		b := UsageWindow{Scope: "q", Period: "fixed", Unit: "tokens", Used: fp(20), Limit: fp(900)}
		ms, _ := ProviderMeters([]UsageAccount{rbAccount("a", "x", now, a), rbAccount("b", "x", now, b)}, now)
		m := ms[0]
		if *m.Used != 30 || *m.Limit != 1000 || m.Remaining != nil || m.RemainingPercent != nil {
			t.Fatalf("pooled capacity: %+v", m)
		}
	})
	t.Run("units are never mixed", func(t *testing.T) {
		a := UsageWindow{Scope: "q", Period: "fixed", Unit: "tokens", Remaining: fp(5)}
		b := UsageWindow{Scope: "q", Period: "fixed", Unit: "requests", Remaining: fp(7)}
		ms, _ := ProviderMeters([]UsageAccount{rbAccount("a", "x", now, a), rbAccount("b", "x", now, b)}, now)
		if len(ms) != 2 || ms[0].ID == ms[1].ID || *ms[0].Remaining+*ms[1].Remaining != 12 {
			t.Fatalf("%+v", ms)
		}
		for _, m := range ms {
			if m.Coverage.Known != 1 || m.Coverage.Unknown != 0 {
				t.Fatalf("each unit group covers the account that reports it: %+v", m.Coverage)
			}
		}
	})
	t.Run("unsupported unit is counted not guessed", func(t *testing.T) {
		w := UsageWindow{Scope: "q", Period: "fixed", Unit: "credits", Remaining: fp(5)}
		ms, un := ProviderMeters([]UsageAccount{rbAccount("a", "x", now, w)}, now)
		if len(ms) != 0 || un != 1 {
			t.Fatalf("%+v %d", ms, un)
		}
	})
	t.Run("partial coverage", func(t *testing.T) {
		ms, _ := ProviderMeters([]UsageAccount{
			rbAccount("a", "anthropic", now, pct(40)),
			rbAccount("b", "anthropic", now), // no windows
			rbAccount("c", "anthropic", now.Add(-time.Hour), pct(90)),
		}, now)
		m := ms[0]
		if m.Coverage != (UsageMeterCoverage{Known: 1, Stale: 1}) || *m.UsedPercent != 40 {
			t.Fatalf("%+v", m)
		}
	})
	t.Run("expiry is the earlier of source age and reset", func(t *testing.T) {
		soon := now.Add(2 * time.Minute).UnixMilli()
		w := pct(10)
		w.ResetsAt = &soon
		observed := now.Add(-time.Minute)
		ms, _ := ProviderMeters([]UsageAccount{rbAccount("a", "x", observed, w)}, now)
		if ms[0].ExpiresAt != soon || ms[0].ObservedAt != observed.UnixMilli() {
			t.Fatalf("reset deadline: %+v", ms[0])
		}
		late := now.Add(10 * time.Hour).UnixMilli()
		w.ResetsAt = &late
		ms, _ = ProviderMeters([]UsageAccount{rbAccount("a", "x", observed, w)}, now)
		if ms[0].ExpiresAt != observed.Add(UsageMeterMaxAge).UnixMilli() {
			t.Fatalf("age deadline: %+v", ms[0])
		}
		// An observation 14m59s old gets no extra freshness: the meter expires
		// one second later, not 15 minutes after generation.
		old := now.Add(-UsageMeterMaxAge + time.Second)
		ms, _ = ProviderMeters([]UsageAccount{rbAccount("a", "x", old, pct(10))}, now)
		if ms[0].ExpiresAt-now.UnixMilli() != 1000 {
			t.Fatalf("expires in %d ms", ms[0].ExpiresAt-now.UnixMilli())
		}
		// Past the deadline the account is stale and the meter carries no value.
		ms, _ = ProviderMeters([]UsageAccount{rbAccount("a", "x", now.Add(-time.Hour), pct(10))}, now)
		if ms[0].UsedPercent != nil || ms[0].Coverage.Stale != 1 || ms[0].ExpiresAt > now.UnixMilli() {
			t.Fatalf("%+v", ms[0])
		}
		if UsageHeadline(ms, now) != "" {
			t.Fatal("an expired meter became the headline")
		}
	})
	t.Run("a reset in the past makes the account stale", func(t *testing.T) {
		past := now.Add(-time.Second).UnixMilli()
		w := pct(10)
		w.ResetsAt = &past
		ms, _ := ProviderMeters([]UsageAccount{rbAccount("a", "x", now, w)}, now)
		if ms[0].Coverage.Stale != 1 || ms[0].UsedPercent != nil {
			t.Fatalf("%+v", ms[0])
		}
	})
	t.Run("worst pace and earliest reset", func(t *testing.T) {
		later := now.Add(4 * time.Hour).UnixMilli()
		a, b := pct(10), pct(95)
		b.ResetsAt = &later
		ms, _ := ProviderMeters([]UsageAccount{rbAccount("a", "x", now, a), rbAccount("b", "x", now, b)}, now)
		if ms[0].Pace != "ahead" || *ms[0].ResetsAt != reset {
			t.Fatalf("%+v", ms[0])
		}
	})
	t.Run("identity separates period, unit, scope and duration", func(t *testing.T) {
		ws := []UsageWindow{
			{Scope: "general", Period: "rolling", DurationMS: &five, UsedPercent: fp(1)},
			{Scope: "general", Period: "fixed", DurationMS: &five, UsedPercent: fp(2)},
			{Scope: "general", Period: "rolling", UsedPercent: fp(3)},
			{Scope: "opus", Period: "rolling", DurationMS: &five, UsedPercent: fp(4)},
			{Scope: "general", Period: "rolling", DurationMS: &five, Unit: "tokens", Used: fp(1)},
		}
		ms, _ := ProviderMeters([]UsageAccount{rbAccount("a", "anthropic", now, ws...)}, now)
		seen := map[string]bool{}
		for _, m := range ms {
			if seen[m.ID] {
				t.Fatalf("duplicate id %s", m.ID)
			}
			seen[m.ID] = true
		}
		if len(ms) != 5 {
			t.Fatalf("%d meters", len(ms))
		}
	})
	t.Run("hostile producer text stays plain", func(t *testing.T) {
		w := UsageWindow{Scope: "g\x1b[31m<b>/../\u202eevil\n" + strings.Repeat("s", 300), Period: "rolling", UsedPercent: fp(1)}
		ms, _ := ProviderMeters([]UsageAccount{rbAccount("a", "pro\x00vider\u202e", now, w)}, now)
		m := ms[0]
		for _, s := range []string{m.Label, m.Metric, m.ID} {
			if strings.ContainsAny(s, "\x1b\x00\n\u202e") {
				t.Fatalf("control text survived: %q", s)
			}
		}
		if len(m.Label) > 80 || len(m.ID) > 128 || len(m.Metric) > 64 {
			t.Fatalf("lengths: %d %d %d", len(m.Label), len(m.ID), len(m.Metric))
		}
		validateMeters(t, UsageMeters{Schema: UsageMetersSchema, GeneratedAt: now.UnixMilli(), Slots: []UsageSlotDoc{NewUsageSlot("user", "provider", "L", ms, 0, now)}})
	})
}

func TestSessionMetersMeasuredConsumptionAndSeparateQuota(t *testing.T) {
	now := time.Now()
	reset := now.Add(time.Hour).UnixMilli()
	five := int64(5 * time.Hour / time.Millisecond)
	shared := rbAccount("shared", "Anthropic", now, UsageWindow{Scope: "general", Period: "rolling", DurationMS: &five, ResetsAt: &reset, UsedPercent: fp(70)})
	sess := func(in, out *int64, dur *int64, cost *float64, ended bool, account string) UsageSessionObservation {
		s := UsageSessionObservation{Ended: ended}
		s.InputTokens, s.OutputTokens, s.DurationMS, s.CostUSD = in, out, dur, cost
		s.AccountID, s.ObservedAt = account, now.UnixMilli()
		return s
	}
	t.Run("shared account is labelled account-wide and never the session's own", func(t *testing.T) {
		ms, _ := SessionMeters([]UsageSessionObservation{
			sess(ip(100), ip(50), ip(60000), fp(0.5), false, shared.ID),
			sess(ip(1), ip(2), nil, nil, false, shared.ID),
		}, []UsageAccount{shared, shared}, now)
		tok := meterByMetric(ms, "sessions.tokens")
		if tok == nil || *tok.Used != 153 || tok.Coverage != (UsageMeterCoverage{Known: 2}) {
			t.Fatalf("%+v", tok)
		}
		if d := meterByMetric(ms, "sessions.duration"); d == nil || d.Unit != "ms" || *d.Used != 60000 || d.Coverage.Unknown != 1 {
			t.Fatalf("%+v", d)
		}
		q := meterByMetric(ms, "linked."+rbMetric("Anthropic", "general", "5h"))
		if q == nil || *q.UsedPercent != 70 || !strings.HasPrefix(q.Label, "Account-wide:") || q.Coverage.Known != 1 {
			t.Fatalf("one shared account must appear once: %+v", ms)
		}
		slot := NewUsageSlot("domain", "session", "Builds", ms, 0, now)
		if slot.Headline != "" {
			t.Fatalf("account-wide quota became the session headline: %q", slot.Headline)
		}
		validateMeters(t, UsageMeters{Schema: UsageMetersSchema, GeneratedAt: now.UnixMilli(), Slots: []UsageSlotDoc{slot}})
	})
	t.Run("unlinked and api-only session still reports consumption, no quota", func(t *testing.T) {
		ms, _ := SessionMeters([]UsageSessionObservation{sess(ip(0), ip(0), nil, nil, true, "")}, nil, now)
		if len(ms) != 1 || ms[0].Metric != "sessions.tokens" || ms[0].Used == nil || *ms[0].Used != 0 {
			t.Fatalf("known zero lost: %+v", ms)
		}
		if ms[0].ExpiresAt != now.UnixMilli()+UsageMeterMaxAge.Milliseconds() {
			t.Fatalf("%+v", ms[0])
		}
	})
	t.Run("nothing measured gives no meter", func(t *testing.T) {
		ms, _ := SessionMeters([]UsageSessionObservation{sess(nil, nil, nil, nil, false, "")}, nil, now)
		if len(ms) != 0 {
			t.Fatalf("%+v", ms)
		}
	})
	t.Run("an old live session makes the meter stale", func(t *testing.T) {
		old := sess(ip(5), ip(1), nil, nil, false, "")
		old.ObservedAt = now.Add(-time.Hour).UnixMilli()
		ms, _ := SessionMeters([]UsageSessionObservation{old, sess(ip(5), ip(1), nil, nil, false, "")}, nil, now)
		if ms[0].Coverage.Stale != 1 || ms[0].ExpiresAt > now.UnixMilli() {
			t.Fatalf("%+v", ms[0])
		}
	})
}

func TestUsageSlotHeadlineAndTruncation(t *testing.T) {
	now := time.Now()
	mk := func(id string, rem float64, exp time.Time) UsageMeter {
		return UsageMeter{ID: id, Metric: "m." + id, Label: id, Unit: "percent", RemainingPercent: fp(rem), UsedPercent: fp(100 - rem),
			ObservedAt: now.UnixMilli(), ExpiresAt: exp.UnixMilli(), Coverage: UsageMeterCoverage{Known: 1}}
	}
	t.Run("tie break is deterministic", func(t *testing.T) {
		a, b := mk("b", 40, now.Add(time.Hour)), mk("a", 40, now.Add(time.Hour))
		c := mk("c", 40, now.Add(time.Minute))
		for _, order := range [][]UsageMeter{{a, b, c}, {c, b, a}, {b, c, a}} {
			if h := UsageHeadline(order, now); h != "c" {
				t.Fatalf("earlier expiry wins: %s", h)
			}
		}
		if h := UsageHeadline([]UsageMeter{a, b}, now); h != "a" {
			t.Fatalf("id tie-break: %s", h)
		}
	})
	t.Run("expired and uncovered meters are not headlines", func(t *testing.T) {
		old := mk("old", 1, now.Add(-time.Minute))
		unk := mk("unk", 2, now.Add(time.Hour))
		unk.Coverage = UsageMeterCoverage{Stale: 1}
		if h := UsageHeadline([]UsageMeter{old, unk, mk("ok", 50, now.Add(time.Hour))}, now); h != "ok" {
			t.Fatal(h)
		}
	})
	t.Run("more than 16 groups keep the most exhausted allowance", func(t *testing.T) {
		var ms []UsageMeter
		for i := 0; i < 25; i++ {
			ms = append(ms, mk(fmt.Sprintf("g%02d", i), float64(90-i), now.Add(time.Hour)))
		}
		// The least remaining sorts last by id: truncation by id would drop it.
		slot := NewUsageSlot("user", "provider", "Big", ms, 0, now)
		if len(slot.Meters) != 16 || slot.OmittedGroups != 9 || slot.Headline != "g24" || slot.Meters[0].ID != "g24" {
			t.Fatalf("%d meters, omitted %d, headline %s", len(slot.Meters), slot.OmittedGroups, slot.Headline)
		}
		validateMeters(t, UsageMeters{Schema: UsageMetersSchema, GeneratedAt: now.UnixMilli(), Slots: []UsageSlotDoc{slot}})
	})
	t.Run("size cap keeps each headline", func(t *testing.T) {
		doc := UsageMeters{Schema: UsageMetersSchema, GeneratedAt: now.UnixMilli(), Slots: []UsageSlotDoc{}}
		for s := 0; s < 4; s++ {
			var ms []UsageMeter
			for i := 0; i < 16; i++ {
				m := mk(fmt.Sprintf("%d/%02d/%s", s, i, strings.Repeat("x", 100)), float64(90-i), now.Add(time.Hour))
				m.Label = strings.Repeat("L", 80)
				m.Metric = strings.Repeat("m", 60)
				m.Used, m.Remaining, m.Limit, m.ResetsAt, m.DurationMS, m.Pace = fp(1234567.125), fp(7654321.5), fp(8888888.625), ip(1795003600000), ip(18000000), "on_pace"
				ms = append(ms, m)
			}
			doc.Slots = append(doc.Slots, NewUsageSlot([]string{"domain", "domain", "user", "user"}[s], []string{"session", "provider"}[s%2], "S", ms, 0, now))
		}
		before, _ := json.Marshal(doc)
		if len(before) <= UsageMetersMaxBytes {
			t.Fatalf("fixture too small: %d", len(before))
		}
		heads := []string{}
		for _, s := range doc.Slots {
			heads = append(heads, s.Headline)
		}
		if err := doc.Fit(UsageMetersMaxBytes); err != nil {
			t.Fatal(err)
		}
		validateMeters(t, doc)
		for i, s := range doc.Slots {
			if s.Headline != heads[i] || s.Meters[0].ID != heads[i] || s.OmittedGroups == 0 {
				t.Fatalf("slot %d lost its headline or omitted nothing", i)
			}
		}
	})
}

func TestUsageMetersHashIgnoresGenerationTime(t *testing.T) {
	a := UsageMeters{Schema: UsageMetersSchema, GeneratedAt: 1, Slots: []UsageSlotDoc{}}
	b := a
	b.GeneratedAt = 2
	if a.ContentHash() != b.ContentHash() {
		t.Fatal("hash depends on generated_at")
	}
	b.Slots = []UsageSlotDoc{{Slot: UsageSlotID{Scope: "user", Kind: "session", Label: "x"}, Meters: []UsageMeter{}}}
	if a.ContentHash() == b.ContentHash() {
		t.Fatal("hash ignores content")
	}
}

// A statistic added tomorrow uses the existing meter shape. A reader that only
// renders meters (label, unit, amounts, percent, expiry) works on it unchanged.
func TestGenericReaderRendersANewMetric(t *testing.T) {
	now := time.Now()
	doc := UsageMeters{Schema: UsageMetersSchema, GeneratedAt: now.UnixMilli(), Slots: []UsageSlotDoc{NewUsageSlot("domain", "provider", "Team", []UsageMeter{{
		ID: "gpu/hours/fixed/1d/requests", Metric: "gpu.hours.1d", Label: "GPU queue slots", Unit: "requests",
		Used: fp(3), Remaining: fp(9), Limit: fp(12), UsedPercent: fp(25), RemainingPercent: fp(75),
		ObservedAt: now.UnixMilli(), ExpiresAt: now.Add(time.Minute).UnixMilli(), Coverage: UsageMeterCoverage{Known: 1},
	}}, 0, now)}}
	validateMeters(t, doc)
	b, _ := json.Marshal(doc)
	// The reader knows only the document shape, nothing about any metric.
	var generic struct {
		Slots []struct {
			Headline string `json:"headline"`
			Meters   []struct {
				ID, Label, Unit  string
				RemainingPercent *float64 `json:"remaining_percent"`
				ExpiresAt        int64    `json:"expires_at"`
			}
		}
	}
	if err := json.Unmarshal(b, &generic); err != nil {
		t.Fatal(err)
	}
	m := generic.Slots[0].Meters[0]
	if generic.Slots[0].Headline != m.ID || m.Label != "GPU queue slots" || m.Unit != "requests" || *m.RemainingPercent != 75 || m.ExpiresAt <= now.UnixMilli() {
		t.Fatalf("%+v", generic)
	}
}

func TestUsageLabelIsPlainText(t *testing.T) {
	for in, want := range map[string]string{
		"a\x1b[2Jb":       "a[2Jb",
		"x\u202ey":        "xy",
		"  many   spaces": "many spaces",
		"\x00\x01":        "Usage",
		"bad\xffutf":      "badutf",
	} {
		if got := UsageLabel(in, 80); got != want {
			t.Errorf("%q: got %q, want %q", in, got, want)
		}
	}
	if got := UsageLabel(strings.Repeat("\u00e9", 100), 80); len(got) != 80 || !strings.HasSuffix(got, "\u00e9") {
		t.Errorf("cut on a rune boundary: %d", len(got))
	}
}

func TestReadbackNoProducerIdentifiersInPublicFields(t *testing.T) {
	now := time.Now()
	reset := now.Add(time.Hour).UnixMilli()
	five := int64(5 * time.Hour / time.Millisecond)
	const sentinel = "Acme-Secret-Project-42"
	ms, _ := ProviderMeters([]UsageAccount{
		rbAccount("a", "gemini-"+sentinel, now, UsageWindow{Scope: "models/" + sentinel, Period: "rolling", DurationMS: &five, ResetsAt: &reset, UsedPercent: fp(10)}),
		rbAccount("b", "Anthropic", now, UsageWindow{Scope: "codex_" + sentinel, Period: "weekly-" + sentinel, UsedPercent: fp(20)}),
	}, now)
	if len(ms) != 2 {
		t.Fatalf("%+v", ms)
	}
	for _, m := range ms {
		for _, f := range []string{m.ID, m.Metric, m.Label} {
			if strings.Contains(strings.ToLower(f), strings.ToLower("secret")) || strings.Contains(strings.ToLower(f), "acme") || strings.Contains(strings.ToLower(f), "gemini") || strings.Contains(strings.ToLower(f), "codex") {
				t.Fatalf("producer identifier reached a public field: %q", f)
			}
		}
	}
	// The same group keeps the same id however the other groups change.
	one, _ := ProviderMeters([]UsageAccount{rbAccount("b", "Anthropic", now, UsageWindow{Scope: "codex_" + sentinel, Period: "rolling", UsedPercent: fp(20)})}, now)
	two, _ := ProviderMeters([]UsageAccount{
		rbAccount("b", "Anthropic", now, UsageWindow{Scope: "codex_" + sentinel, Period: "rolling", UsedPercent: fp(20)}),
		rbAccount("c", "Anthropic", now, UsageWindow{Scope: "codex_" + sentinel[:3], Period: "rolling", UsedPercent: fp(20)}),
	}, now)
	var found bool
	for _, m := range two {
		found = found || m.ID == one[0].ID
	}
	if !found || len(two) != 2 || two[0].ID == two[1].ID {
		t.Fatalf("ids depend on the other groups: %+v %+v", one, two)
	}
}

func TestReadbackNativeUnitsAndPartialTokens(t *testing.T) {
	now := time.Now()
	w := UsageWindow{Scope: "requests", Period: "fixed", Unit: "REQUESTS", Remaining: fp(120)}
	ms, un := ProviderMeters([]UsageAccount{rbAccount("g", "google", now, w)}, now)
	if un != 0 || len(ms) != 1 || ms[0].Unit != "requests" || *ms[0].Remaining != 120 {
		t.Fatalf("native spelling dropped: %+v %d", ms, un)
	}
	odd := UsageWindow{Scope: "x", Period: "fixed", Unit: "credits", Remaining: fp(3), UsedPercent: fp(40)}
	ms, _ = ProviderMeters([]UsageAccount{rbAccount("g", "google", now, odd)}, now)
	if len(ms) != 1 || ms[0].Unit != "percent" || ms[0].Remaining != nil || *ms[0].UsedPercent != 40 {
		t.Fatalf("known percentage lost with an unsupported amount unit: %+v", ms)
	}

	s := UsageSessionObservation{}
	s.InputTokens, s.ObservedAt = ip(100), now.UnixMilli()
	full := UsageSessionObservation{}
	full.InputTokens, full.OutputTokens, full.ObservedAt = ip(1), ip(2), now.UnixMilli()
	got, _ := SessionMeters([]UsageSessionObservation{s, full}, nil, now)
	tok := meterByMetric(got, "sessions.tokens")
	if tok == nil || *tok.Used != 3 || tok.Coverage != (UsageMeterCoverage{Known: 1, Unknown: 1}) {
		t.Fatalf("a missing counter was read as zero: %+v", tok)
	}
	got, _ = SessionMeters([]UsageSessionObservation{s}, nil, now)
	if len(got) != 0 {
		t.Fatalf("partial counters published a total: %+v", got)
	}
}

func TestReadbackMeasuredSessionOutranksLinkedQuota(t *testing.T) {
	now := time.Now()
	reset := now.Add(time.Hour).UnixMilli()
	var linked []UsageAccount
	for i := 0; i < 20; i++ {
		linked = append(linked, rbAccount(fmt.Sprintf("a%d", i), "Anthropic", now, UsageWindow{Scope: fmt.Sprintf("s%d", i), Period: "rolling", ResetsAt: &reset, UsedPercent: fp(50)}))
	}
	s := UsageSessionObservation{}
	s.InputTokens, s.OutputTokens, s.DurationMS, s.ObservedAt = ip(1), ip(2), ip(1000), now.UnixMilli()
	s.CostUSD = fp(1)
	ms, _ := SessionMeters([]UsageSessionObservation{s}, linked, now)
	slot := NewUsageSlot("user", "session", "L", ms, 0, now)
	for _, metric := range []string{"sessions.tokens", "sessions.duration", "sessions.cost"} {
		if meterByMetric(slot.Meters, metric) == nil {
			t.Fatalf("%s was truncated away by linked quota", metric)
		}
	}
	doc := UsageMeters{Schema: UsageMetersSchema, GeneratedAt: now.UnixMilli(), Slots: []UsageSlotDoc{slot}}
	if err := doc.Fit(2500); err != nil {
		t.Fatal(err)
	}
	if meterByMetric(doc.Slots[0].Meters, "sessions.tokens") == nil {
		t.Fatal("the byte cap dropped measured consumption before linked quota")
	}
}

// An account label holds the full e-mail for the Worker owner and Hub
// administrators; the readback document never carries it.
func TestReadbackNeverCarriesAnAccountLabel(t *testing.T) {
	now := time.Now()
	reset := now.Add(time.Hour).UnixMilli()
	five := int64(5 * time.Hour / time.Millisecond)
	a := rbAccount("mail", "anthropic", now, UsageWindow{Scope: "general", Period: "rolling", DurationMS: &five, ResetsAt: &reset, UsedPercent: fp(10)})
	a.Label = "person@example.com / abc123"
	ms, _ := ProviderMeters([]UsageAccount{a}, now)
	if len(ms) == 0 {
		t.Fatal("no meters")
	}
	b, err := json.Marshal(ms)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "person") || strings.Contains(string(b), "example.com") {
		t.Fatalf("an account label reached the readback meters: %s", b)
	}
}

func TestUsageLimitNameIsHumanAndMetersStayUnique(t *testing.T) {
	ms := func(h int64) *int64 { v := h * 3600000; return &v }
	for _, c := range []struct {
		ms   *int64
		kind string
		want string
	}{
		{ms(5), "rolling", "5-hour limit"}, {ms(24), "rolling", "Daily limit"}, {ms(168), "rolling", "Weekly limit"},
		{ms(720), "fixed", "Monthly limit"}, {ms(72), "rolling", "3-day limit"}, {nil, "rolling", "Rolling limit"}, {nil, "", "Limit"},
	} {
		if got := UsageLimitName(c.ms, c.kind); got != c.want {
			t.Errorf("%v %q: %q, want %q", c.ms, c.kind, got, c.want)
		}
	}
	now := time.Now()
	reset := now.Add(time.Hour).UnixMilli()
	week := *ms(168)
	w := func(scope string) UsageWindow {
		return UsageWindow{Scope: scope, Period: "rolling", DurationMS: &week, ResetsAt: &reset, UsedPercent: fp(10)}
	}
	out, _ := ProviderMeters([]UsageAccount{rbAccount("a", "OpenAI", now, w("codex.primary"), w("gpt-x"))}, now)
	if len(out) != 2 {
		t.Fatalf("%+v", out)
	}
	labels := map[string]bool{}
	for _, m := range out {
		labels[m.Label] = true
		if strings.Contains(m.Label, "limit g") || strings.Contains(m.Label, "codex") {
			t.Errorf("opaque label %q", m.Label)
		}
	}
	if len(labels) != 2 || !labels["OpenAI Weekly limit"] && !labels["OpenAI Weekly limit (2)"] {
		t.Fatalf("labels %v", labels)
	}
}
