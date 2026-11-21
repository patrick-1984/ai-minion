package ops

import (
	"cmp"
	"slices"
	"time"
)

// UsageWindowGroup groups comparable provider pools; it never adds utilization
// percentages across unequal plans or double-counts a Worker reporting the
// same account. Percentages are an unweighted mean of fresh known accounts.
type UsageWindowGroup struct {
	Unit            string   `json:"unit,omitempty"`
	Provider        string   `json:"provider"`
	Scope           string   `json:"scope"`
	Period          string   `json:"period"`
	DurationMS      *int64   `json:"duration_ms"`
	MeanUsedPercent *float64 `json:"mean_used_percent"`
	Known           int      `json:"known"`
	Stale           int      `json:"stale"`
	Unknown         int      `json:"unknown"`
}

type UsageOverview struct {
	Accounts       int                `json:"accounts"`
	WithoutWindows int                `json:"without_windows"`
	Groups         []UsageWindowGroup `json:"groups"`
}

func summarizeUsage(rows []UsageAccountRow, now time.Time) UsageOverview {
	latest := map[string]UsageAccount{}
	for _, r := range rows {
		r.Provider = UsageProvider(r.Provider)
		if prev, ok := latest[r.ID]; !ok || prev.ObservedAt < r.ObservedAt {
			latest[r.ID] = r.UsageAccount
		}
	}
	type key struct {
		provider, scope, period, unit string
		duration                      int64
	}
	type group struct {
		row UsageWindowGroup
		sum float64
	}
	groups := map[key]*group{}
	totals := map[string]int{}
	out := UsageOverview{Groups: []UsageWindowGroup{}}
	for _, a := range latest {
		if a.AuthMode != "subscription" {
			continue
		}
		out.Accounts++
		totals[a.Provider]++
		if len(a.Windows) == 0 {
			out.WithoutWindows++
		}
		for _, w := range a.Windows {
			k := key{provider: a.Provider, scope: w.Scope, period: w.Period, unit: w.Unit}
			if w.DurationMS != nil {
				k.duration = *w.DurationMS
			}
			g := groups[k]
			if g == nil {
				g = &group{row: UsageWindowGroup{Provider: a.Provider, Scope: w.Scope, Period: w.Period, Unit: w.Unit, DurationMS: w.DurationMS}}
				groups[k] = g
			}
			if w.UsedPercent == nil {
				continue
			}
			if now.Sub(time.UnixMilli(a.ObservedAt)) > 15*time.Minute || w.ResetsAt != nil && *w.ResetsAt <= now.UnixMilli() {
				g.row.Stale++
				continue
			}
			g.sum += *w.UsedPercent
			g.row.Known++
		}
	}
	for _, g := range groups {
		if g.row.Known > 0 {
			mean := g.sum / float64(g.row.Known)
			g.row.MeanUsedPercent = &mean
		}
		g.row.Unknown = totals[g.row.Provider] - g.row.Known - g.row.Stale
		out.Groups = append(out.Groups, g.row)
	}
	slices.SortFunc(out.Groups, func(a, b UsageWindowGroup) int {
		if c := cmp.Compare(a.Provider, b.Provider); c != 0 {
			return c
		}
		if c := cmp.Compare(a.Scope, b.Scope); c != 0 {
			return c
		}
		if c := cmp.Compare(a.Period, b.Period); c != 0 {
			return c
		}
		if c := cmp.Compare(a.Unit, b.Unit); c != 0 {
			return c
		}
		if a.DurationMS == nil || b.DurationMS == nil {
			if a.DurationMS == nil && b.DurationMS != nil {
				return -1
			}
			if b.DurationMS == nil && a.DurationMS != nil {
				return 1
			}
			return 0
		}
		return cmp.Compare(*a.DurationMS, *b.DurationMS)
	})
	return out
}

// SummarizeUsage reports provider-specific groups and a cross-provider
// unweighted account mean only for broad coding allowances with matching
// periods. Model-specific quotas, absent durations and monetary caps never
// enter that comparison.
func SummarizeUsage(rows []UsageAccountRow, now time.Time) UsageOverview {
	out := summarizeUsage(rows, now)
	type period struct {
		kind     string
		duration int64
	}
	comparable := make([]UsageAccountRow, 0, len(rows))
	providers := map[period]map[string]bool{}
	for _, row := range rows {
		provider := UsageProvider(row.Provider)
		broad := map[period]UsageWindow{}
		ambiguous := map[period]bool{}
		for _, window := range row.Windows {
			known := provider == "Anthropic" && window.Scope == "general" || provider == "OpenAI" && (window.Scope == "codex.primary" || window.Scope == "codex.secondary")
			if row.AuthMode != "subscription" || !known || window.DurationMS == nil || window.Unit != "" {
				continue
			}
			k := period{window.Period, *window.DurationMS}
			if _, seen := broad[k]; seen {
				ambiguous[k] = true
			}
			window.Scope = "General coding allowance"
			broad[k] = window
		}
		row.Provider = "All subscriptions"
		row.Windows = nil
		for k, window := range broad {
			if !ambiguous[k] {
				row.Windows = append(row.Windows, window)
				if providers[k] == nil {
					providers[k] = map[string]bool{}
				}
				providers[k][provider] = true
			}
		}
		comparable = append(comparable, row)
	}
	for _, row := range summarizeUsage(comparable, now).Groups {
		if row.DurationMS != nil && len(providers[period{row.Period, *row.DurationMS}]) > 1 {
			out.Groups = append(out.Groups, row)
		}
	}
	return out
}
