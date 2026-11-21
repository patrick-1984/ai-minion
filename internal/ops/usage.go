package ops

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// UsageVersion identifies the metadata-only Worker report. Times are UTC unix
// milliseconds, including provider resets. Counter fields are cumulative within
// a session. A missing counter is unknown; a pointer to zero is a known zero.
const UsageVersion = 1

// UsageDisclosure is shared by Worker registration and the Hub interface.
const UsageDisclosure = "Your Hub administrator and Worker owner can see usage statistics for every session on this Worker, including unshared sessions. Reports may include session names, folders, harnesses, models, account identifiers, timing, token counts, costs, limits and collection diagnostics. Console sharing and control have separate permissions."

type UsageHarness struct {
	AuthMode      string   `json:"auth_mode,omitempty"`
	Providers     []string `json:"providers,omitempty"`
	AccountStatus string   `json:"account_status,omitempty"`
	Name          string   `json:"name"`
	Version       string   `json:"version,omitempty"`
	Status        string   `json:"status"` // installed, unavailable, unsupported
}

type UsageCounters struct {
	ReasoningTokens  *int64   `json:"reasoning_tokens"`
	ContextTokens    *int64   `json:"context_tokens"`
	ContextLimit     *int64   `json:"context_limit"`
	EstimatedCostUSD *float64 `json:"estimated_cost_usd"`

	InputTokens      *int64   `json:"input_tokens"`
	OutputTokens     *int64   `json:"output_tokens"`
	CacheReadTokens  *int64   `json:"cache_read_tokens"`
	CacheWriteTokens *int64   `json:"cache_write_tokens"`
	CostUSD          *float64 `json:"cost_usd"`
	DurationMS       *int64   `json:"duration_ms"`
	Tasks            *int64   `json:"tasks"`
	Subagents        *int64   `json:"subagents"`
}

type UsageModel struct {
	Model string `json:"model"`
	UsageCounters
}

type UsageEvent struct {
	AccountID  string `json:"account_id,omitempty"`
	NativeID   string `json:"native_id,omitempty"`
	Seq        int64  `json:"seq"`
	ID         string `json:"id"`
	SessionID  string `json:"session_id"`
	Kind       string `json:"kind"`
	Source     string `json:"source"`
	ObservedAt int64  `json:"observed_at"`
}

func (e UsageEvent) Valid() bool {
	switch e.Kind {
	case "session_observed", "session_end_observed", "session_started", "session_stopped", "turn_started", "turn_finished", "task_started", "task_finished", "subagent_started", "subagent_finished", "folder_changed", "model_changed":
	default:
		return false
	}
	return (e.AccountID == "" || usageIDValid(e.AccountID)) && usageIDValid(e.ID) && (usageIDValid(e.SessionID) || e.SessionID == "" && usageIDValid(e.NativeID)) && usageText(e.Source, 64) && e.Source != "" && e.ObservedAt > 0 && e.Seq >= 0
}

type UsageSession struct {
	TimeSource string       `json:"time_source,omitempty"`
	NativeID   string       `json:"native_id,omitempty"`
	Models     []UsageModel `json:"models,omitempty"`

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
	Unit        string   `json:"unit,omitempty"`
	Scope       string   `json:"scope"`  // stable native limit ID/model scope
	Period      string   `json:"period"` // rolling, fixed, total
	DurationMS  *int64   `json:"duration_ms"`
	ResetsAt    *int64   `json:"resets_at"`
	UsedPercent *float64 `json:"used_percent"`
}

type UsageAccount struct {
	Plan            string        `json:"plan,omitempty"`
	TimeSource      string        `json:"time_source,omitempty"`
	SourceSessionID string        `json:"source_session_id,omitempty"`
	ID              string        `json:"id"` // derived locally; never a credential
	Provider        string        `json:"provider"`
	Label           string        `json:"label,omitempty"`
	AuthMode        string        `json:"auth_mode"` // subscription, api, unknown
	Source          string        `json:"source"`
	ObservedAt      int64         `json:"observed_at"`
	Windows         []UsageWindow `json:"windows"`
}

type UsageProbe struct {
	Status            string `json:"status"`
	RetryAfterSeconds int64  `json:"retry_after_seconds,omitempty"`
}

type UsageReport struct {
	InventoryAt *int64         `json:"inventory_at,omitempty"`
	Events      []UsageEvent   `json:"events,omitempty"`
	Probe       *UsageProbe    `json:"probe,omitempty"`
	Version     int            `json:"version"`
	ObservedAt  int64          `json:"observed_at"`
	Complete    bool           `json:"complete"` // successful full session enumeration
	Inventory   []UsageHarness `json:"inventory,omitempty"`
	Sessions    []UsageSession `json:"sessions"`
	Accounts    []UsageAccount `json:"accounts,omitempty"`
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

func usageClock(s string) bool {
	return s == "" || s == "native" || s == "worker_received" || s == "worker_measured"
}
func usageMode(s string) bool        { return s == "unknown" || s == "subscription" || s == "api" }
func usagePercent(v *float64) bool   { return v == nil || (*v >= 0 && *v <= 100) }
func usageTime(t, latest int64) bool { return t > 0 && t <= latest }

func (c UsageCounters) valid() bool {
	for _, n := range []*int64{c.InputTokens, c.OutputTokens, c.ReasoningTokens, c.ContextTokens, c.ContextLimit, c.CacheReadTokens, c.CacheWriteTokens, c.DurationMS, c.Tasks, c.Subagents} {
		if n != nil && *n < 0 {
			return false
		}
	}
	for _, price := range []*float64{c.CostUSD, c.EstimatedCostUSD} {
		if price != nil && (*price < 0 || math.IsInf(*price, 0) || math.IsNaN(*price)) {
			return false
		}
	}
	return true
}

// Check rejects a malformed report as a unit; the domain layer never stores
// raw wire JSON or diagnostic text from adapters.
func (r UsageReport) Check(now time.Time) error {
	if r.Probe != nil {
		switch r.Probe.Status {
		case "ready", "unavailable", "rate_limited", "account_changed", "unsupported":
		default:
			return invalid("probe", "unknown collector status")
		}
		if r.Probe.RetryAfterSeconds < 0 || r.Probe.RetryAfterSeconds > 31536000 {
			return invalid("probe", "invalid cooldown")
		}
	}
	latest := now.Add(2 * time.Minute).UnixMilli()
	if r.InventoryAt != nil && !usageTime(*r.InventoryAt, r.ObservedAt) {
		return invalid("inventory_at", "invalid inventory time")
	}
	if r.Version != UsageVersion || !usageTime(r.ObservedAt, latest) || len(r.Sessions) > 256 || len(r.Accounts) > 128 || len(r.Inventory) > 48 {
		return invalid("report", "unsupported version, timestamp or size")
	}
	if len(r.Events) > 128 {
		return invalid("events", "batch too large")
	}
	for _, e := range r.Events {
		if !e.Valid() || e.ObservedAt > r.ObservedAt {
			return invalid("events", "invalid event metadata")
		}
	}
	seen := map[string]bool{}
	for _, s := range r.Sessions {
		if !usageIDValid(s.ID) || seen[s.ID] || !usageText(s.Name, 256) || s.Name == "" || !usageText(s.Folder, 1024) || !usageText(s.Model, 128) || !usageText(s.Harness, 64) || !usageText(s.Version, 96) || !usageClock(s.TimeSource) || !usageText(s.Provider, 64) || !usageMode(s.AuthMode) || !usageText(s.Source, 64) || s.Source == "" || !usageTime(s.ObservedAt, latest) || s.ObservedAt > r.ObservedAt || s.AccountID != "" && !usageIDValid(s.AccountID) || !s.UsageCounters.valid() {
			return invalid("sessions", "invalid usage metadata")
		}
		if s.StartedAt != nil && !usageTime(*s.StartedAt, s.ObservedAt) || s.StoppedAt != nil && (!usageTime(*s.StoppedAt, s.ObservedAt) || s.StartedAt != nil && *s.StoppedAt < *s.StartedAt) {
			return invalid("sessions", "invalid lifecycle timestamps")
		}
		if s.NativeID != "" && !usageIDValid(s.NativeID) || len(s.Models) > 32 {
			return invalid("sessions", "invalid native/model identity")
		}
		for _, m := range s.Models {
			if m.Model == "" || !usageText(m.Model, 128) || !m.UsageCounters.valid() {
				return invalid("models", "invalid model counters")
			}
		}
		seen[s.ID] = true
	}
	for _, a := range r.Accounts {
		key := "account:" + a.ID + "|" + a.SourceSessionID
		if !usageIDValid(a.ID) || a.SourceSessionID != "" && !usageIDValid(a.SourceSessionID) || seen[key] || a.Provider == "" || !usageText(a.Provider, 64) || !usageText(a.Plan, 64) || !usageClock(a.TimeSource) || !usageMode(a.AuthMode) || !usageText(a.Label, 96) || a.Source == "" || !usageText(a.Source, 64) || !usageTime(a.ObservedAt, r.ObservedAt) || len(a.Windows) > 32 || a.AuthMode != "subscription" && len(a.Windows) > 0 {
			return invalid("accounts", "invalid account metadata")
		}
		seen[key] = true
		windowKeys := map[string]bool{}
		for _, w := range a.Windows {
			key := w.Scope + "\x00" + w.Period + "\x00" + w.Unit
			if w.DurationMS != nil {
				key += "\x00" + strconv.FormatInt(*w.DurationMS, 10)
			}
			if windowKeys[key] || w.Scope == "" || !usageText(w.Scope, 96) || !usageText(w.Unit, 32) || (w.Period != "rolling" && w.Period != "fixed" && w.Period != "total") || !usagePercent(w.UsedPercent) || w.DurationMS != nil && *w.DurationMS <= 0 || w.ResetsAt != nil && *w.ResetsAt <= 0 {
				return invalid("windows", "invalid or duplicate usage window")
			}
			windowKeys[key] = true
		}
	}
	for _, h := range r.Inventory {
		if len(h.Providers) > 32 || (h.AuthMode != "" && !usageMode(h.AuthMode) && h.AuthMode != "mixed") || !usageText(h.AccountStatus, 32) {
			return invalid("inventory", "invalid authentication metadata")
		}
		for _, p := range h.Providers {
			if !usageText(p, 64) {
				return invalid("inventory", "invalid provider name")
			}
		}
		if h.Name == "" || !usageText(h.Name, 64) || !usageText(h.Version, 96) || h.Status != "installed" && h.Status != "unavailable" && h.Status != "unsupported" {
			return invalid("inventory", "invalid harness entry")
		}
	}
	encoded, err := json.Marshal(r)
	if err != nil || len(encoded) > 768<<10 {
		return invalid("report", "encoded report exceeds the usage transport limit")
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
