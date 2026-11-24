package ops

import (
	"slices"
	"strings"
	"time"
)

// usageWindowKey identifies one quota window of an account across readings.
type usageWindowKey struct {
	scope, period, unit string
	duration            int64
}

func windowKey(w UsageWindow) usageWindowKey {
	k := usageWindowKey{scope: w.Scope, period: w.Period, unit: w.Unit}
	if w.DurationMS != nil {
		k.duration = *w.DurationMS
	}
	return k
}

// moreInformativeWindow reports whether reading b of a window says more about
// the current state than reading a. A later reset is a later window. Within
// the same window usage only grows, so the higher percentage is the newer
// reading. A reading that names its reset beats one that does not.
func moreInformativeWindow(a, b UsageWindow) bool {
	switch {
	case a.ResetsAt != nil && b.ResetsAt != nil && *a.ResetsAt != *b.ResetsAt:
		return *b.ResetsAt > *a.ResetsAt
	case (a.ResetsAt == nil) != (b.ResetsAt == nil):
		return b.ResetsAt != nil
	case a.UsedPercent == nil || b.UsedPercent == nil:
		return a.UsedPercent == nil && b.UsedPercent != nil
	default:
		return *b.UsedPercent > *a.UsedPercent
	}
}

// MergeUsageWindows combines readings of one subscription account's quota
// windows (several sessions, several Workers, or a stored and a new reading)
// per window: the reading with the later reset wins, and within the same reset
// the higher used percentage wins, because usage within a window only grows.
// It never takes "the latest row": an idle session that re-sends an old
// reading cannot hide a newer one. A window missing from one reading is kept
// from another. A window whose reset has passed reads as reset (0% since the
// reset, reset time unchanged); one that has been over for longer than its
// own duration says nothing any more and is dropped; a zero now skips both
// (the reader judges expiry itself). The result has at most 32 windows and a
// stable order.
func MergeUsageWindows(now time.Time, readings ...[]UsageWindow) []UsageWindow {
	best := map[usageWindowKey]UsageWindow{}
	var order []usageWindowKey
	for _, windows := range readings {
		for _, w := range windows {
			k := windowKey(w)
			old, ok := best[k]
			if !ok {
				order = append(order, k)
				best[k] = w
				continue
			}
			if moreInformativeWindow(old, w) {
				best[k] = w
			}
		}
	}
	nowMS := now.UnixMilli()
	out := make([]UsageWindow, 0, len(order))
	for _, k := range order {
		w := best[k]
		if !now.IsZero() && w.ResetsAt != nil && *w.ResetsAt <= nowMS {
			if w.DurationMS != nil && *w.DurationMS > 0 && *w.ResetsAt+*w.DurationMS <= nowMS {
				continue
			}
			zero := 0.0
			w.UsedPercent, w.Used, w.Remaining = &zero, nil, nil
		}
		out = append(out, w)
	}
	slices.SortStableFunc(out, func(a, b UsageWindow) int {
		ka, kb := windowKey(a), windowKey(b)
		if c := strings.Compare(ka.scope, kb.scope); c != 0 {
			return c
		}
		if c := strings.Compare(ka.period, kb.period); c != 0 {
			return c
		}
		if ka.duration != kb.duration {
			if ka.duration < kb.duration {
				return -1
			}
			return 1
		}
		return strings.Compare(ka.unit, kb.unit)
	})
	if len(out) > 32 {
		out = out[:32]
	}
	return out
}

// MergeableUsageAccounts reports whether two readings describe the same
// subscription quota: the same account and plan, both from the Claude status
// line. Another plan's limits no longer apply.
func MergeableUsageAccounts(a, b UsageAccount) bool {
	return a.ID == b.ID && a.AuthMode == "subscription" && b.AuthMode == "subscription" &&
		a.Source == "claude-statusline" && b.Source == "claude-statusline" && a.Plan == b.Plan
}
