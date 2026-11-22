package ops

import (
	"testing"
	"time"
)

// An account contributes once to a group even when its windows spell the unit
// differently (REQUESTS / requests): coverage and amounts are not doubled.
func TestProviderMetersCountsAnAccountOncePerGroup(t *testing.T) {
	now := time.Now()
	reset := now.Add(time.Hour).UnixMilli()
	w := func(unit string) UsageWindow {
		return UsageWindow{Scope: "s", Period: "fixed", Unit: unit, Remaining: fp(10), ResetsAt: &reset}
	}
	ms, _ := ProviderMeters([]UsageAccount{rbAccount("a", "google", now, w("REQUESTS"), w("requests"))}, now)
	if len(ms) != 1 || ms[0].Coverage.Known != 1 || *ms[0].Remaining != 10 {
		t.Fatalf("%+v", ms)
	}
}

// Two durations that share a long decimal prefix keep distinct meter ids.
func TestProviderMeterIDsDifferForDistinctDurations(t *testing.T) {
	now := time.Now()
	reset := now.Add(time.Hour).UnixMilli()
	w := func(d int64) UsageWindow {
		return UsageWindow{Scope: "s", Period: "rolling", DurationMS: ip(d), ResetsAt: &reset, UsedPercent: fp(10)}
	}
	ms, _ := ProviderMeters([]UsageAccount{rbAccount("a", "anthropic", now, w(1234567890123), w(1234567890124))}, now)
	if len(ms) != 2 || ms[0].ID == ms[1].ID {
		t.Fatalf("%+v", ms)
	}
}
