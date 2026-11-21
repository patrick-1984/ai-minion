package ops

import (
	"crypto/sha256"
	"encoding/hex"
	"math"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// UsageVersion identifies the metadata-only Worker report. Times are UTC unix
// milliseconds, including provider resets. Counter fields are cumulative within
// a session. A missing counter is unknown; a pointer to zero is a known zero.
const UsageVersion = 1

type UsageHarness struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
	Status  string `json:"status"` // installed, unavailable, unsupported
}

type UsageCounters struct {
	InputTokens      *int64   `json:"input_tokens"`
	OutputTokens     *int64   `json:"output_tokens"`
	CacheReadTokens  *int64   `json:"cache_read_tokens"`
	CacheWriteTokens *int64   `json:"cache_write_tokens"`
	CostUSD          *float64 `json:"cost_usd"`
	DurationMS       *int64   `json:"duration_ms"`
	Tasks            *int64   `json:"tasks"`
	Subagents        *int64   `json:"subagents"`
}

type UsageSession struct {
	ID         string `json:"id"` // opaque identity of this session incarnation on this Worker
	Name       string `json:"name"`
	Folder     string `json:"folder,omitempty"`
	Harness    string `json:"harness,omitempty"`
	Version    string `json:"version,omitempty"`
	Model      string `json:"model,omitempty"`
	Provider   string `json:"provider,omitempty"`
	AccountID  string `json:"account_id,omitempty"`
	AuthMode   string `json:"auth_mode"`
	StartedAt  *int64 `json:"started_at"`
	StoppedAt  *int64 `json:"stopped_at"`
	ObservedAt int64  `json:"observed_at"`
	Source     string `json:"source"`
	UsageCounters
}

type UsageWindow struct {
	Scope       string   `json:"scope"`  // stable native limit ID/model scope
	Period      string   `json:"period"` // rolling, fixed, total
	DurationMS  *int64   `json:"duration_ms"`
	ResetsAt    *int64   `json:"resets_at"`
	UsedPercent *float64 `json:"used_percent"`
}

type UsageAccount struct {
	ID         string        `json:"id"` // derived locally; never a credential
	Provider   string        `json:"provider"`
	Label      string        `json:"label,omitempty"`
	AuthMode   string        `json:"auth_mode"` // subscription, api, unknown
	Source     string        `json:"source"`
	ObservedAt int64         `json:"observed_at"`
	Windows    []UsageWindow `json:"windows"`
}

type UsageReport struct {
	Version    int            `json:"version"`
	ObservedAt int64          `json:"observed_at"`
	Complete   bool           `json:"complete"` // successful full session enumeration
	Inventory  []UsageHarness `json:"inventory,omitempty"`
	Sessions   []UsageSession `json:"sessions"`
	Accounts   []UsageAccount `json:"accounts,omitempty"`
}

// UsageID domain-separates opaque identifiers. Each component is terminated;
// callers must supply normalized components with no NUL. Only the digest is
// eligible for transmission. High-entropy API keys are fingerprinted locally.
func UsageID(domain string, components ...string) string {
	h := sha256.New()
	for _, s := range append([]string{domain}, components...) {
		h.Write([]byte(s))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

func usageIDValid(s string) bool {
	if len(s) != 64 || s != strings.ToLower(s) {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

func usageText(s string, max int) bool {
	return utf8.ValidString(s) && len(s) <= max && strings.IndexFunc(s, unicode.IsControl) < 0
}

func usageMode(s string) bool        { return s == "unknown" || s == "subscription" || s == "api" }
func usagePercent(v *float64) bool   { return v == nil || (*v >= 0 && *v <= 100) }
func usageTime(t, latest int64) bool { return t > 0 && t <= latest }

func (c UsageCounters) valid() bool {
	for _, n := range []*int64{c.InputTokens, c.OutputTokens, c.CacheReadTokens, c.CacheWriteTokens, c.DurationMS, c.Tasks, c.Subagents} {
		if n != nil && *n < 0 {
			return false
		}
	}
	return c.CostUSD == nil || *c.CostUSD >= 0 && !math.IsInf(*c.CostUSD, 0)
}

// Check rejects a malformed report as a unit; the domain layer never stores
// raw wire JSON or diagnostic text from adapters.
func (r UsageReport) Check(now time.Time) error {
	latest := now.Add(2 * time.Minute).UnixMilli()
	if r.Version != UsageVersion || !usageTime(r.ObservedAt, latest) || len(r.Sessions) > 256 || len(r.Accounts) > 128 || len(r.Inventory) > 48 {
		return invalid("report", "unsupported version, timestamp or size")
	}
	seen := map[string]bool{}
	for _, s := range r.Sessions {
		if !usageIDValid(s.ID) || seen[s.ID] || !usageText(s.Name, 256) || s.Name == "" || !usageText(s.Folder, 1024) || !usageText(s.Model, 128) || !usageText(s.Harness, 64) || !usageText(s.Version, 96) || !usageText(s.Provider, 64) || !usageMode(s.AuthMode) || !usageText(s.Source, 64) || s.Source == "" || !usageTime(s.ObservedAt, latest) || s.ObservedAt > r.ObservedAt || s.AccountID != "" && !usageIDValid(s.AccountID) || !s.UsageCounters.valid() {
			return invalid("sessions", "invalid usage metadata")
		}
		if s.StartedAt != nil && !usageTime(*s.StartedAt, s.ObservedAt) || s.StoppedAt != nil && (!usageTime(*s.StoppedAt, s.ObservedAt) || s.StartedAt != nil && *s.StoppedAt < *s.StartedAt) {
			return invalid("sessions", "invalid lifecycle timestamps")
		}
		seen[s.ID] = true
	}
	for _, a := range r.Accounts {
		if !usageIDValid(a.ID) || seen[a.ID] || a.Provider == "" || !usageText(a.Provider, 64) || !usageMode(a.AuthMode) || !usageText(a.Label, 96) || a.Source == "" || !usageText(a.Source, 64) || !usageTime(a.ObservedAt, r.ObservedAt) || len(a.Windows) > 32 || a.AuthMode != "subscription" && len(a.Windows) > 0 {
			return invalid("accounts", "invalid account metadata")
		}
		seen[a.ID] = true
		windowKeys := map[string]bool{}
		for _, w := range a.Windows {
			key := w.Scope + "\x00" + w.Period
			if w.DurationMS != nil {
				key += "\x00" + time.Duration(*w.DurationMS).String()
			}
			if windowKeys[key] || w.Scope == "" || !usageText(w.Scope, 96) || (w.Period != "rolling" && w.Period != "fixed" && w.Period != "total") || !usagePercent(w.UsedPercent) || w.DurationMS != nil && *w.DurationMS <= 0 || w.ResetsAt != nil && *w.ResetsAt <= 0 {
				return invalid("windows", "invalid or duplicate usage window")
			}
			windowKeys[key] = true
		}
	}
	for _, h := range r.Inventory {
		if h.Name == "" || !usageText(h.Name, 64) || !usageText(h.Version, 96) || h.Status != "installed" && h.Status != "unavailable" && h.Status != "unsupported" {
			return invalid("inventory", "invalid harness entry")
		}
	}
	return nil
}

// Pace compares consumed and elapsed percentages only when a reported
// duration and future reset define the current interval. Five percentage
// points either side of elapsed time count as on pace.
func (w UsageWindow) Pace(now time.Time) string {
	if w.Period == "total" || w.DurationMS == nil || *w.DurationMS <= 0 || w.ResetsAt == nil || w.UsedPercent == nil || !usagePercent(w.UsedPercent) {
		return "unknown"
	}
	remaining := *w.ResetsAt - now.UnixMilli()
	if remaining <= 0 || remaining > *w.DurationMS {
		return "unknown"
	}
	elapsed := 100 * (1 - float64(remaining)/float64(*w.DurationMS))
	switch {
	case *w.UsedPercent > elapsed+5:
		return "ahead"
	case *w.UsedPercent < elapsed-5:
		return "below"
	default:
		return "on_pace"
	}
}
