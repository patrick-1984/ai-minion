package ops

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"net/url"
	"slices"
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

// UsageExporterNotice discloses the automatic native exporter installs. It is part of
// UsageDisclosure and mirrored in the installers and the Android strings.
const UsageExporterNotice = "Minion adds a status-line exporter and lifecycle hooks to Claude Code for tokens, cost, limits and session activity. Your existing status line keeps working. Control the integration with usage_claude_exporter=off. Minion adds lifecycle hooks to Codex profiles on Linux, macOS and Android for session activity. Review and trust them in Codex with /hooks. Disable recording with usage_codex_exporter=off. Lifecycle hooks report event types and timing without prompt or response content."

// UsageAccessNotice states what a report says about how a session signs in. It
// is part of UsageDisclosure and mirrored in the installers and the Android strings.
const UsageAccessNotice = "How a session signs in is reported as the access kind only (subscription, API key or custom endpoint); no key, URL or host."

// UsageDisclosure is shared by Worker registration and the Hub interface.
const UsageDisclosure = "Your Hub administrator and Worker owner can see usage statistics for every session on this Worker, including unshared sessions. Reports may include session names, folders, harnesses, models, account identifiers, timing, token counts, costs, limits and collection diagnostics. Console sharing and control have separate permissions. " + UsageAccessNotice + " " + UsageExporterNotice

// Claude usage exporter states of a Worker's Claude profile.
const (
	UsageExporterInstalled  = "installed"
	UsageExporterAbsent     = "absent"
	UsageExporterOverridden = "overridden" // hooks disabled or managed settings win
	UsageExporterOff        = "off"        // the Worker owner turned the automatic install off
	UsageExporterConflict   = "conflict"   // the existing settings could not be kept
)

type UsageHarness struct {
	AuthMode      string   `json:"auth_mode,omitempty"`
	Providers     []string `json:"providers,omitempty"`
	AccountStatus string   `json:"account_status,omitempty"`
	// Exporter is the state of the Claude Code usage exporter in the Worker's
	// Claude profile: installed, absent or overridden. Only the state, no paths.
	Exporter string `json:"exporter,omitempty"`
	Name     string `json:"name"`
	Version  string `json:"version,omitempty"`
	Status   string `json:"status"` // installed, unavailable, unsupported
}

type UsageCounters struct {
	APIDurationMS *int64   `json:"api_duration_ms"`
	LinesAdded    *int64   `json:"lines_added"`
	LinesRemoved  *int64   `json:"lines_removed"`
	CacheRequests *int64   `json:"cache_requests"`
	CacheMisses   *int64   `json:"cache_misses"`
	CacheHitRatio *float64 `json:"cache_hit_ratio"`

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

// UsageSpendBudget is a gateway's monetary allowance, separate from
// subscription capacity and from the cost of a session.
type UsageSpendBudget struct {
	Source           string   `json:"source"`
	ObservedAt       int64    `json:"observed_at"`
	Period           string   `json:"period,omitempty"`
	UsedPercent      *float64 `json:"used_percent"`
	ResetsAt         *int64   `json:"resets_at"`
	EstimatedUsedUSD *float64 `json:"estimated_used_usd"`
	LimitUSD         *float64 `json:"limit_usd"`
}

// UsageResponseTokens contains token counts for one native API response.
// InputTokens excludes the cache reads and writes reported separately here.
type UsageResponseTokens struct {
	InputTokens      *int64 `json:"input_tokens"`
	OutputTokens     *int64 `json:"output_tokens"`
	CacheReadTokens  *int64 `json:"cache_read_tokens"`
	CacheWriteTokens *int64 `json:"cache_write_tokens"`
}

type UsageSession struct {
	GatewaySpend      *UsageSpendBudget    `json:"gateway_spend,omitempty"`
	LastResponseUsage *UsageResponseTokens `json:"last_response_usage,omitempty"`
	// Windows are the upstream limits observed by a custom endpoint session.
	Windows    []UsageWindow `json:"windows,omitempty"`
	TimeSource string        `json:"time_source,omitempty"`
	NativeID   string        `json:"native_id,omitempty"`
	Models     []UsageModel  `json:"models,omitempty"`

	ID        string `json:"id"` // opaque identity of this session incarnation on this Worker
	Name      string `json:"name"`
	Folder    string `json:"folder,omitempty"`
	Harness   string `json:"harness,omitempty"`
	Version   string `json:"version,omitempty"`
	Model     string `json:"model,omitempty"`
	Provider  string `json:"provider,omitempty"`
	AccountID string `json:"account_id,omitempty"`
	AuthMode  string `json:"auth_mode"`
	// CustomEndpoint marks a session that reaches its provider through a
	// custom endpoint (a gateway or proxy). Only the access kind is
	// reported, never the host or URL.
	CustomEndpoint bool `json:"custom_endpoint,omitempty"`
	// Endpoint is deprecated: v0.20-dev Workers sent the endpoint host. The
	// Hub ignores it (a non-empty value counts as CustomEndpoint) and never
	// stores or shows it.
	Endpoint   string `json:"endpoint,omitempty"`
	StartedAt  *int64 `json:"started_at"`
	StoppedAt  *int64 `json:"stopped_at"`
	ObservedAt int64  `json:"observed_at"`
	Source     string `json:"source"`
	UsageCounters
}

// DropEndpoint discards the deprecated endpoint host of a v0.20-dev Worker. A
// session that had one still uses a custom endpoint.
func (s *UsageSession) DropEndpoint() {
	if s.Endpoint != "" {
		s.CustomEndpoint = true
	}
	s.Endpoint = ""
}

type UsageWindow struct {
	Used        *float64 `json:"used"`
	Limit       *float64 `json:"limit"`
	Remaining   *float64 `json:"remaining"`
	Unit        string   `json:"unit,omitempty"`
	Scope       string   `json:"scope"`  // stable native limit ID/model scope
	Period      string   `json:"period"` // rolling, fixed, total
	DurationMS  *int64   `json:"duration_ms"`
	ResetsAt    *int64   `json:"resets_at"`
	UsedPercent *float64 `json:"used_percent"`
}

type UsageAccount struct {
	// CustomEndpoint marks an API account reached through a custom endpoint.
	// The Hub shows only this access kind; EndpointOrigin is accepted from
	// older Workers but never displayed.
	CustomEndpoint  bool          `json:"custom_endpoint,omitempty"`
	EndpointOrigin  string        `json:"endpoint_origin,omitempty"`
	KeyHint         string        `json:"key_hint,omitempty"`
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
	for _, n := range []*int64{c.APIDurationMS, c.LinesAdded, c.LinesRemoved, c.CacheRequests, c.CacheMisses, c.InputTokens, c.OutputTokens, c.ReasoningTokens, c.ContextTokens, c.ContextLimit, c.CacheReadTokens, c.CacheWriteTokens, c.DurationMS, c.Tasks, c.Subagents} {
		if n != nil && *n < 0 {
			return false
		}
	}
	if c.CacheHitRatio != nil && !(*c.CacheHitRatio >= 0 && *c.CacheHitRatio <= 1) {
		return false
	}
	for _, price := range []*float64{c.CostUSD, c.EstimatedCostUSD} {
		if price != nil && (*price < 0 || math.IsInf(*price, 0) || math.IsNaN(*price)) {
			return false
		}
	}
	return true
}

// UsageLabelMax bounds an account label: a full e-mail address (254 bytes,
// RFC 5321) plus a short suffix.
const UsageLabelMax = 320

// DropBadLabels returns the report with an invalid optional account label
// emptied, so one bad display field never invalidates the whole report.
func (r UsageReport) DropBadLabels() UsageReport {
	accounts := slices.Clone(r.Accounts)
	for i := range accounts {
		if !usageText(accounts[i].Label, UsageLabelMax) {
			accounts[i].Label = ""
		}
	}
	r.Accounts = accounts
	return r
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
	if r.Version != UsageVersion || !usageTime(r.ObservedAt, latest) || len(r.Sessions) > 256 || len(r.Accounts) > UsageAccountsMax || len(r.Inventory) > 48 {
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
		if !usageIDValid(s.ID) ||
			seen[s.ID] ||
			!usageText(s.Name, 256) ||
			s.Name == "" ||
			!usageText(s.Folder, 1024) ||
			!usageText(s.Model, 128) ||
			!usageText(s.Harness, 64) ||
			!usageText(s.Version, 96) ||
			!usageClock(s.TimeSource) ||
			!usageText(s.Provider, 64) ||
			!usageMode(s.AuthMode) ||
			!usageText(s.Source, 64) ||
			s.Source == "" ||
			!usageTime(s.ObservedAt, latest) ||
			s.ObservedAt > r.ObservedAt ||
			s.AccountID != "" && !usageIDValid(s.AccountID) ||
			!s.valid() {
			return invalid("sessions", "invalid usage metadata")
		}
		if b := s.GatewaySpend; b != nil {
			if b.Source != "claude-statusline" || !usageTime(b.ObservedAt, r.ObservedAt) || b.Period != "" && b.Period != "daily" && b.Period != "weekly" && b.Period != "monthly" || b.ResetsAt != nil && *b.ResetsAt <= 0 {
				return invalid("gateway_spend", "invalid source, timestamp or period")
			}
			for _, v := range []*float64{b.UsedPercent, b.EstimatedUsedUSD, b.LimitUSD} {
				if v != nil && (*v < 0 || math.IsNaN(*v) || math.IsInf(*v, 0)) {
					return invalid("gateway_spend", "invalid monetary observation")
				}
			}
		}
		if len(s.Windows) > 0 {
			if s.AuthMode != "api" || !s.CustomEndpoint {
				return invalid("windows", "session limits require a custom endpoint")
			}
			if err := checkUsageWindows(s.Windows); err != nil {
				return err
			}
		}
		if v := s.LastResponseUsage; v != nil {
			for _, n := range []*int64{v.InputTokens, v.OutputTokens, v.CacheReadTokens, v.CacheWriteTokens} {
				if n != nil && *n < 0 {
					return invalid("last_response_usage", "invalid token count")
				}
			}
		}
		if s.StartedAt != nil && !usageTime(*s.StartedAt, s.ObservedAt) ||
			s.StoppedAt != nil && (!usageTime(*s.StoppedAt, s.ObservedAt) ||
				s.StartedAt != nil && *s.StoppedAt < *s.StartedAt) {
			return invalid("sessions", "invalid lifecycle timestamps")
		}
		if s.NativeID != "" && !usageIDValid(s.NativeID) || len(s.Models) > 32 {
			return invalid("sessions", "invalid native/model identity")
		}
		for _, m := range s.Models {
			if m.Model == "" || !usageText(m.Model, 128) || !m.valid() {
				return invalid("models", "invalid model counters")
			}
		}
		seen[s.ID] = true
	}
	for _, a := range r.Accounts {
		key := "account:" + a.ID + "|" + a.SourceSessionID
		if seen[key] {
			return invalid("accounts", "invalid account metadata")
		}
		if err := a.check(r.ObservedAt); err != nil {
			return err
		}
		seen[key] = true
	}
	for _, h := range r.Inventory {
		if len(h.Providers) > 32 || (h.AuthMode != "" && !usageMode(h.AuthMode) && h.AuthMode != "mixed") || !usageText(h.AccountStatus, 32) || h.Exporter != "" && h.Exporter != UsageExporterInstalled && h.Exporter != UsageExporterAbsent && h.Exporter != UsageExporterOverridden && h.Exporter != UsageExporterOff && h.Exporter != UsageExporterConflict {
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
	if err != nil || len(encoded) > UsageReportMaxBytes {
		return invalid("report", "encoded report exceeds the usage transport limit")
	}
	return nil
}

// check validates one account's metadata and windows.
func (a UsageAccount) check(observedAt int64) error {
	if !usageIDValid(a.ID) ||
		a.SourceSessionID != "" && !usageIDValid(a.SourceSessionID) ||
		a.Provider == "" ||
		!usageText(a.Provider, 64) ||
		!usageText(a.Plan, 64) ||
		!usageClock(a.TimeSource) ||
		!usageMode(a.AuthMode) ||
		!usageText(a.Label, UsageLabelMax) ||
		a.Source == "" ||
		!usageText(a.Source, 64) ||
		!usageTime(a.ObservedAt, observedAt) ||
		len(a.Windows) > 0 && a.AuthMode != "subscription" && (a.AuthMode != "api" || !a.CustomEndpoint) {
		return invalid("accounts", "invalid account metadata")
	}
	if a.EndpointOrigin != "" || a.KeyHint != "" {
		if a.AuthMode != "api" || !safeUsageOrigin(a.EndpointOrigin) || !validKeyHint(a.KeyHint, a.ID) {
			return invalid("accounts", "invalid API identity display")
		}
	}
	return checkUsageWindows(a.Windows)
}

func checkUsageWindows(windows []UsageWindow) error {
	if len(windows) > 32 {
		return invalid("windows", "too many usage windows")
	}
	windowKeys := map[string]bool{}
	for _, w := range windows {
		key := w.Scope + "\x00" + w.Period + "\x00" + w.Unit
		if w.DurationMS != nil {
			key += "\x00" + strconv.FormatInt(*w.DurationMS, 10)
		}
		if windowKeys[key] ||
			w.Scope == "" ||
			!usageText(w.Scope, 96) ||
			!usageText(w.Unit, 32) ||
			(w.Period != "rolling" && w.Period != "fixed" && w.Period != "total") ||
			!usagePercent(w.UsedPercent) ||
			w.DurationMS != nil && *w.DurationMS <= 0 ||
			w.ResetsAt != nil && *w.ResetsAt <= 0 {
			return invalid("windows", "invalid or duplicate usage window")
		}
		for _, n := range []*float64{w.Used, w.Limit, w.Remaining} {
			if n != nil && (*n < 0 || math.IsNaN(*n) || math.IsInf(*n, 0)) {
				return invalid("windows", "invalid native quantity")
			}
		}
		windowKeys[key] = true
	}
	return nil
}

// UsageAccountsMax bounds the account projection of one report. It exceeds
// the 256 sessions of a report plus the inventory and native rows.
const UsageAccountsMax = 512

// UsageReportMaxBytes caps the encoded usage report of one transport message.
const UsageReportMaxBytes = 768 << 10

// Sanitize returns the report with every optional display field or account
// that cannot be accepted dropped, so one bad or surplus account never
// invalidates the session telemetry of the whole report. A report that is
// too large only because of quota windows loses the session copies of them
// and keeps everything else; subscription account limits are never trimmed.
func (r UsageReport) Sanitize() UsageReport {
	r = r.DropLegacyEndpoints().DropBadLabels()
	seen := map[string]bool{}
	kept := make([]UsageAccount, 0, len(r.Accounts))
	for _, a := range r.Accounts {
		a.Windows = SubscriptionWindows(a)
		key := a.ID + "|" + a.SourceSessionID
		if len(kept) >= UsageAccountsMax || seen[key] || a.check(r.ObservedAt) != nil {
			continue
		}
		seen[key] = true
		kept = append(kept, a)
	}
	r.Accounts = kept
	if r.tooLarge() {
		r.Sessions = slices.Clone(r.Sessions)
		for i := range r.Sessions {
			r.Sessions[i].Windows = nil
		}
	}
	return r
}

// SubscriptionWindows returns the account's quota windows only for a
// subscription. Custom endpoints are not told apart (one aggregate account per
// provider and kind), so upstream windows seen through one stay on the session
// and never describe an API-key account.
func SubscriptionWindows(a UsageAccount) []UsageWindow {
	if a.AuthMode != "subscription" {
		return nil
	}
	return a.Windows
}

func (r UsageReport) tooLarge() bool {
	b, err := json.Marshal(r)
	return err == nil && len(b) > UsageReportMaxBytes
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

func safeUsageOrigin(value string) bool {
	if value == "" {
		return true
	}
	u, err := url.Parse(value)
	return err == nil && len(value) <= 300 && (u.Scheme == "https" || u.Scheme == "http") && u.Hostname() != "" && u.User == nil && u.Path == "" && u.RawQuery == "" && !u.ForceQuery && !strings.Contains(value, "#")
}

// UsageEndpointHost is deprecated with UsageSession.Endpoint; it accepts an empty value or a bare host name or address,
// optionally with a port.
func UsageEndpointHost(h string) bool {
	return usageText(h, 253) && !strings.ContainsAny(h, "/@?# \t\\")
}

func validKeyHint(hint, id string) bool {
	if hint == "" {
		return true
	}
	if !strings.HasSuffix(hint, " / "+id[:10]) {
		return false
	}
	value := strings.TrimSuffix(hint, " / "+id[:10])
	return value == "Credential" || len(value) == 13 && strings.HasSuffix(value, "...") && usageText(value, 13)
}

// DropEndpoint discards the deprecated endpoint origin of an older Worker. An
// account that had one still uses a custom endpoint, and a label that embeds
// the host of that origin (or of any host in also) becomes the generic one.
func (a *UsageAccount) DropEndpoint(also ...string) {
	hosts := append([]string(nil), also...)
	if a.EndpointOrigin != "" {
		a.CustomEndpoint = true
		if u, err := url.Parse(a.EndpointOrigin); err == nil {
			hosts = append(hosts, u.Hostname())
		}
		hosts = append(hosts, a.EndpointOrigin)
	}
	a.EndpointOrigin = ""
	if a.Label == "" {
		return
	}
	lower := strings.ToLower(a.Label)
	for _, h := range hosts {
		if h = strings.ToLower(strings.TrimSpace(h)); h != "" && strings.Contains(lower, h) {
			p := a.Provider
			if p == "" {
				p = "Provider"
			}
			a.Label = p + " custom endpoint (not told apart)"
			a.CustomEndpoint = true
			return
		}
	}
}

// DropLegacyEndpoints returns a copy of the report without the deprecated
// endpoint hosts and origins of v0.20-dev Workers: the session and account
// fields are emptied (the access kind is kept) and a label that contains such
// a host is replaced by a generic one. The receiver is not modified.
func (r UsageReport) DropLegacyEndpoints() UsageReport {
	var hosts []string
	r.Sessions = append([]UsageSession(nil), r.Sessions...)
	for i := range r.Sessions {
		if h := r.Sessions[i].Endpoint; h != "" {
			hosts = append(hosts, h)
			if u, err := url.Parse("//" + h); err == nil && u.Hostname() != "" {
				hosts = append(hosts, u.Hostname())
			}
		}
		r.Sessions[i].DropEndpoint()
	}
	r.Accounts = append([]UsageAccount(nil), r.Accounts...)
	for i := range r.Accounts {
		r.Accounts[i].DropEndpoint(hosts...)
	}
	return r
}
