package web

import (
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/ops"
)

func TestUsageMidweekPaceLabelsAndNullableDisplay(t *testing.T) {
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	week := int64((7 * 24 * time.Hour) / time.Millisecond)
	midpoint := now.Add(84 * time.Hour).UnixMilli()
	page := usagePage{Rows: ops.UsageListResult{AsOf: now.UnixMilli()}}

	for _, c := range []struct {
		value                         float64
		used, free, projection, label string
	}{
		{15, "15.0%", "85.0%", "30.0%", "Within pace"},
		{50, "50.0%", "50.0%", "100.0%", "On pace"},
		{85, "85.0%", "15.0%", "170.0%", "Ahead of pace"},
		{0, "0.0%", "100.0%", "0.0%", "Within pace"},
	} {
		w := ops.UsageWindow{Scope: "general", Period: "rolling", DurationMS: &week, ResetsAt: &midpoint, UsedPercent: &c.value}
		row := ops.UsageAccountRow{UsageAccount: ops.UsageAccount{AuthMode: "subscription", ObservedAt: now.UnixMilli(), Windows: []ops.UsageWindow{w}}}
		got := page.accountCard(row).Windows[0]
		if got.Pace != c.label || got.Used != c.used || got.RemainingPercent != c.free || got.Projected != c.projection || got.Elapsed != "50.0%" {
			t.Errorf("midweek at %g%%: %+v, want %s, %s used, %s remaining, %s projected", c.value, got, c.label, c.used, c.free, c.projection)
		}
	}
	for _, c := range []struct {
		name string
		w    ops.UsageWindow
		want string
		used string
	}{
		{"absent consumption", ops.UsageWindow{DurationMS: &week, ResetsAt: &midpoint}, "Unknown pace", "Unknown"},
		{"absent duration", ops.UsageWindow{ResetsAt: &midpoint, UsedPercent: floatPointer(0)}, "Unknown pace", "0.0%"},
		{"expired window", ops.UsageWindow{DurationMS: &week, ResetsAt: intPointer(now.UnixMilli()), UsedPercent: floatPointer(50)}, "Stale window", "50.0%"},
	} {
		t.Run(c.name, func(t *testing.T) {
			c.w.Scope, c.w.Period = "general", "rolling"
			row := ops.UsageAccountRow{UsageAccount: ops.UsageAccount{AuthMode: "subscription", ObservedAt: now.UnixMilli(), Windows: []ops.UsageWindow{c.w}}}
			got := page.accountCard(row).Windows[0]
			if got.Pace != c.want || got.Used != c.used || got.Elapsed != "" || got.Projected != "" {
				t.Errorf("expected only the reported data and %s, got %+v", c.want, got)
			}
		})
	}
}

func floatPointer(v float64) *float64 { return &v }
func intPointer(v int64) *int64       { return &v }
