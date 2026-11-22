package ops

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// The nominated usage document (minion.usage-meters/1, docs/schemas). The Hub
// builds it from buckets that people deliberately published; a Worker copies
// it verbatim for local readers. The types here match the schema exactly: an
// unknown quantity is an absent field, an observed zero is a pointer to 0.
const (
	OpUsageReadback = "usage_readback"

	UsageMetersSchema = "minion.usage-meters/1"
	// UsageMetersMaxBytes caps the final encoded document.
	UsageMetersMaxBytes = 32 << 10
	// UsageMetersMaxPerSlot caps the meters of one slot.
	UsageMetersMaxPerSlot = 16
	// UsageMeterMaxAge is the source-age deadline: an observation older than
	// this is stale and a meter built from it expires.
	UsageMeterMaxAge = 15 * time.Minute

	usageLinkedPrefix = "linked."
)

// UsageMeters is the whole document.
type UsageMeters struct {
	Schema      string         `json:"schema"`
	GeneratedAt int64          `json:"generated_at"`
	Slots       []UsageSlotDoc `json:"slots"`
}

type UsageSlotID struct {
	Scope string `json:"scope"` // user or domain
	Kind  string `json:"kind"`  // session or provider
	Label string `json:"label"` // the label the nominator published; untrusted text
}

type UsageSlotDoc struct {
	Slot UsageSlotID `json:"slot"`
	// Headline is the id of the meter a progress bar shows.
	Headline      string       `json:"headline,omitempty"`
	OmittedGroups int          `json:"omitted_groups"`
	Meters        []UsageMeter `json:"meters"`
}

type UsageMeterCoverage struct {
	Known   int `json:"known"`
	Stale   int `json:"stale"`
	Unknown int `json:"unknown"`
}

type UsageMeter struct {
	ID               string             `json:"id"`
	Metric           string             `json:"metric"`
	Label            string             `json:"label"`
	Unit             string             `json:"unit"` // percent, tokens, requests, usd, ms
	Used             *float64           `json:"used,omitempty"`
	Remaining        *float64           `json:"remaining,omitempty"`
	Limit            *float64           `json:"limit,omitempty"`
	UsedPercent      *float64           `json:"used_percent,omitempty"`
	RemainingPercent *float64           `json:"remaining_percent,omitempty"`
	ResetsAt         *int64             `json:"resets_at,omitempty"`
	DurationMS       *int64             `json:"duration_ms,omitempty"`
	Pace             string             `json:"pace,omitempty"`
	ObservedAt       int64              `json:"observed_at"`
	ExpiresAt        int64              `json:"expires_at"`
	Coverage         UsageMeterCoverage `json:"coverage"`
}

type UsageReadbackArgs struct{}

func init() {
	Register(Op{Name: OpUsageReadback, Summary: "Preview the nominated usage document your Workers receive: the Hub-wide pair and your own pair. Labels are plain untrusted text; no session names, folders, account labels or member ids.", Scope: ScopeHub, Actions: MustActionSet(ActionRead), Args: reflect.TypeFor[UsageReadbackArgs](), Result: reflect.TypeFor[UsageMeters]()})
}

// UsageLabel makes producer text safe to show: invalid UTF-8, control and
// format characters (including bidirectional overrides) are dropped and the
// result is cut on a rune boundary to max bytes. It never returns "".
func UsageLabel(s string, max int) string {
	s = strings.ToValidUTF8(s, "")
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || unicode.Is(unicode.Zl, r) || unicode.Is(unicode.Zp, r) {
			return -1
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	for len(s) > max {
		_, n := utf8.DecodeLastRuneInString(s)
		s = s[:len(s)-n]
	}
	s = strings.TrimSpace(s)
	if s == "" {
		return "Usage"
	}
	return s
}

// usageSlug maps producer text to a public name made of [a-z0-9._-].
func usageSlug(s string, max int) string {
	var b strings.Builder
	dash := true
	for _, r := range strings.ToLower(s) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '.' || r == '_' {
			b.WriteRune(r)
			dash = false
		} else if !dash {
			b.WriteByte('-')
			dash = true
		}
	}
	out := strings.Trim(b.String(), "-._")
	if len(out) > max {
		out = strings.Trim(out[:max], "-._")
	}
	if out == "" {
		return "x"
	}
	return out
}

func usageRound(v float64) float64 { return math.Round(v*1e6) / 1e6 }
func usagePct(v float64) float64   { return math.Round(v*100) / 100 }

func usageDurationShort(ms int64) string {
	switch {
	case ms%86400000 == 0:
		return fmt.Sprintf("%dd", ms/86400000)
	case ms%3600000 == 0:
		return fmt.Sprintf("%dh", ms/3600000)
	case ms%60000 == 0:
		return fmt.Sprintf("%dm", ms/60000)
	case ms%1000 == 0:
		return fmt.Sprintf("%ds", ms/1000)
	}
	return fmt.Sprintf("%dms", ms)
}

// usageFamily maps a provider to the fixed public vocabulary: the slug used
// in ids and metrics and the display name. Anything the vocabulary does not
// know is "other": a custom namespace is producer-defined text and never
// leaves the Hub.
func usageFamily(provider string) (slug, name string) {
	switch provider {
	case "OpenAI":
		return "openai", "OpenAI"
	case "Anthropic":
		return "anthropic", "Anthropic"
	case "Google":
		return "google", "Google"
	case "OpenRouter":
		return "openrouter", "OpenRouter"
	case "DeepSeek":
		return "deepseek", "DeepSeek"
	case "Amazon Bedrock":
		return "bedrock", "Amazon Bedrock"
	case "OpenCode Go":
		return "opencode-go", "OpenCode Go"
	case "OpenCode Zen":
		return "opencode-zen", "OpenCode Zen"
	}
	return "other", "Other provider"
}

// usageScopeToken names the producer-defined limit scope (a model, a
// deployment, a native limit id) without revealing it: "account" for the
// provider's default scope, otherwise an opaque stable digest of the complete
// (provider, scope) pair. Unknown providers always get a digest so two of
// them never share an id.
func usageScopeToken(provider, scope string) string {
	if _, name := usageFamily(provider); scope == "" && name != "Other provider" {
		return "account"
	}
	h := sha256.Sum256([]byte(provider + "\x00" + scope))
	return "g" + hex.EncodeToString(h[:6])
}

// usageUnitNative accepts the native spellings of a unit, case-insensitively.
func usageUnitNative(unit string) string {
	switch u := strings.ToLower(strings.TrimSpace(unit)); u {
	case "percent", "%":
		return "percent"
	case "token", "tokens":
		return "tokens"
	case "request", "requests":
		return "requests"
	case "usd":
		return "usd"
	default:
		return u
	}
}

// usagePeriod keeps the fixed period vocabulary; anything else is "other".
func usagePeriod(p string) string {
	switch p {
	case "rolling", "fixed", "total":
		return p
	}
	return "other"
}

func usageUnitPublic(unit string) bool {
	switch unit {
	case "tokens", "requests", "usd":
		return true
	}
	return false
}

func usagePaceRank(p string) int {
	switch p {
	case "ahead":
		return 3
	case "on_pace":
		return 2
	case "below":
		return 1
	}
	return 0
}

type usageContribution struct {
	w     UsageWindow
	obs   int64
	fresh bool
}

func (c usageContribution) deadline() int64 {
	d := c.obs + UsageMeterMaxAge.Milliseconds()
	if c.w.ResetsAt != nil && *c.w.ResetsAt < d {
		d = *c.w.ResetsAt
	}
	return d
}

func usageWindowUsable(w UsageWindow) bool {
	if w.Unit == "" {
		return w.UsedPercent != nil
	}
	return w.Used != nil || w.Limit != nil || w.Remaining != nil || w.UsedPercent != nil
}

// allReport reports whether every contribution has the quantity and sums it.
func usageSumAll(cs []usageContribution, get func(UsageWindow) *float64) *float64 {
	if len(cs) == 0 {
		return nil
	}
	var sum float64
	for _, c := range cs {
		v := get(c.w)
		if v == nil {
			return nil
		}
		sum += *v
	}
	sum = usageRound(sum)
	return &sum
}

// ProviderMeters derives one meter per window group (provider, scope, period,
// unit, duration) from the authorized raw account observations. The caller
// must already have removed what may not be published; observations are only
// deduplicated here (newest per account). Each quantity is derived on its own
// from the accounts that are fresh, in the window's own unit: a percentage is
// an unweighted mean (never a capacity-weighted figure) and is not derived
// from amounts, an amount is summed only when every fresh account reports it
// in that unit, and nothing is inferred from anything else. Groups whose unit
// the document cannot express are counted in the second result.
func ProviderMeters(accounts []UsageAccount, now time.Time) ([]UsageMeter, int) {
	return providerMeters(accounts, now, false)
}

type usageGroupKey struct {
	provider, scope, period, unit string
	duration                      int64
	hasDuration                   bool
}

func providerMeters(accounts []UsageAccount, now time.Time, linked bool) ([]UsageMeter, int) {
	latest := map[string]UsageAccount{}
	for _, a := range accounts {
		if prev, ok := latest[a.ID]; !ok || prev.ObservedAt < a.ObservedAt {
			latest[a.ID] = a
		}
	}
	totals := map[string]int{}
	type group struct {
		key         usageGroupKey
		known, aged []usageContribution
	}
	groups := map[usageGroupKey]*group{}
	nowMS := now.UnixMilli()
	for _, a := range latest {
		if a.AuthMode != "subscription" {
			continue
		}
		provider := UsageProvider(a.Provider)
		totals[provider]++
		for _, w := range a.Windows {
			w.Unit = usageUnitNative(w.Unit)
			if w.Unit == "percent" {
				w.Unit = ""
			}
			if w.Unit != "" && !usageUnitPublic(w.Unit) && w.UsedPercent != nil {
				// The amount cannot be expressed, but an independently known
				// percentage still can.
				w.Unit, w.Used, w.Limit, w.Remaining = "", nil, nil, nil
			}
			k := usageGroupKey{provider: provider, scope: w.Scope, period: usagePeriod(w.Period), unit: w.Unit}
			if w.DurationMS != nil {
				k.duration, k.hasDuration = *w.DurationMS, true
			}
			g := groups[k]
			if g == nil {
				g = &group{key: k}
				groups[k] = g
			}
			if !usageWindowUsable(w) {
				continue
			}
			c := usageContribution{w: w, obs: a.ObservedAt}
			if nowMS-a.ObservedAt > UsageMeterMaxAge.Milliseconds() || w.ResetsAt != nil && *w.ResetsAt <= nowMS {
				g.aged = append(g.aged, c)
			} else {
				c.fresh = true
				g.known = append(g.known, c)
			}
		}
	}
	keys := make([]usageGroupKey, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, func(a, b usageGroupKey) int {
		return cmp.Or(cmp.Compare(a.provider, b.provider), cmp.Compare(a.scope, b.scope), cmp.Compare(a.period, b.period), cmp.Compare(a.unit, b.unit), cmp.Compare(a.duration, b.duration))
	})
	var out []UsageMeter
	unsupported := 0
	for _, k := range keys {
		g := groups[k]
		if k.unit != "" && !usageUnitPublic(k.unit) {
			unsupported++
			continue
		}
		contributors := g.known
		if len(contributors) == 0 {
			contributors = g.aged
		}
		if len(contributors) == 0 {
			continue // the window exists but no account reports a quantity
		}
		m := UsageMeter{Unit: "percent"}
		if k.unit != "" {
			m.Unit = k.unit
		}
		if len(g.known) > 0 {
			if k.unit != "" {
				m.Used = usageSumAll(g.known, func(w UsageWindow) *float64 { return w.Used })
				m.Remaining = usageSumAll(g.known, func(w UsageWindow) *float64 { return w.Remaining })
				m.Limit = usageSumAll(g.known, func(w UsageWindow) *float64 { return w.Limit })
			}
			if mean := usageSumAll(g.known, func(w UsageWindow) *float64 { return w.UsedPercent }); mean != nil {
				used := usagePct(*mean / float64(len(g.known)))
				used = math.Min(100, math.Max(0, used))
				rem := usagePct(100 - used)
				m.UsedPercent, m.RemainingPercent = &used, &rem
			}
			worst := ""
			for _, c := range g.known {
				if c.w.ResetsAt != nil && (m.ResetsAt == nil || *c.w.ResetsAt < *m.ResetsAt) {
					r := *c.w.ResetsAt
					m.ResetsAt = &r
				}
				if p := c.w.Pace(now); usagePaceRank(p) > usagePaceRank(worst) {
					worst = p
				}
			}
			m.Pace = worst
		}
		if k.hasDuration {
			d := k.duration
			m.DurationMS = &d
		}
		m.ObservedAt = contributors[0].obs
		m.ExpiresAt = contributors[0].deadline()
		for _, c := range contributors[1:] {
			m.ObservedAt = min(m.ObservedAt, c.obs)
			m.ExpiresAt = min(m.ExpiresAt, c.deadline())
		}
		m.Coverage = UsageMeterCoverage{Known: len(g.known), Stale: len(g.aged), Unknown: max(0, totals[k.provider]-len(g.known)-len(g.aged))}

		span := k.period
		if k.hasDuration {
			span = usageDurationShort(k.duration)
		}
		// Public names only: the provider family, an opaque scope token and
		// the generic window kind. Never the producer's own spelling.
		fam, famName := usageFamily(k.provider)
		token := usageScopeToken(k.provider, k.scope)
		spanSlug := usageSlug(span, 12)
		metric := fam + "." + token + "." + spanSlug
		id := strings.Join([]string{fam, token, usageSlug(k.period, 12), spanSlug, m.Unit}, "/")
		if linked {
			metric = usageLinkedPrefix + metric
			id = "linked/" + id
		}
		m.ID, m.Metric = id, metric

		suffix := ""
		if m.UsedPercent != nil && len(g.known) > 1 {
			suffix = fmt.Sprintf(" (unweighted mean of %d)", len(g.known))
		}
		base := famName + " quota, " + span
		if token != "account" {
			base += " (limit " + token[1:7] + ")"
		}
		if linked {
			base = "Account-wide: " + base
		}
		m.Label = UsageLabel(base, 80-len(suffix)) + suffix
		out = append(out, m)
	}
	return out, unsupported
}

// UsageSessionObservation is the latest canonical observation of one selected
// session: its cumulative counters and whether it has ended.
type UsageSessionObservation struct {
	UsageSession
	Ended bool
}

// SessionMeters reports the measured consumption of the selected sessions from
// their latest cumulative counters (tokens, duration, cost, each only where
// known), and, labelled as account-wide and never as the session's own,
// the quota of the subscriptions the sessions are linked to. Sessions that
// share an account contribute that quota once.
func SessionMeters(sessions []UsageSessionObservation, linked []UsageAccount, now time.Time) ([]UsageMeter, int) {
	type contribution struct {
		v         float64
		obs       int64
		ended     bool
		estimated bool
	}
	nowMS := now.UnixMilli()
	defs := []struct {
		metric, unit, label string
		get                 func(UsageSession) (float64, bool, bool)
	}{
		{"sessions.tokens", "tokens", "Tokens used (selected sessions)", func(s UsageSession) (float64, bool, bool) {
			// A total needs every part. A session that reports only one of
			// them is partial: it counts as unknown for this meter and is
			// never read as zero.
			if s.InputTokens == nil || s.OutputTokens == nil {
				return 0, false, false
			}
			return float64(*s.InputTokens + *s.OutputTokens), true, false
		}},
		{"sessions.duration", "ms", "Session time (selected sessions)", func(s UsageSession) (float64, bool, bool) {
			if s.DurationMS == nil {
				return 0, false, false
			}
			return float64(*s.DurationMS), true, false
		}},
		{"sessions.cost", "usd", "Cost (selected sessions)", func(s UsageSession) (float64, bool, bool) {
			switch {
			case s.CostUSD != nil:
				return *s.CostUSD, true, false
			case s.EstimatedCostUSD != nil:
				return *s.EstimatedCostUSD, true, true
			}
			return 0, false, false
		}},
	}
	var out []UsageMeter
	for _, d := range defs {
		var cs []contribution
		for _, s := range sessions {
			if v, ok, est := d.get(s.UsageSession); ok {
				cs = append(cs, contribution{v, s.ObservedAt, s.Ended, est})
			}
		}
		if len(cs) == 0 {
			continue
		}
		var sum float64
		cov := UsageMeterCoverage{Unknown: len(sessions) - len(cs)}
		var live, ended []int64
		estimated := false
		for _, c := range cs {
			sum += c.v
			estimated = estimated || c.estimated
			switch {
			case c.ended:
				cov.Known++
				ended = append(ended, c.obs)
			case nowMS-c.obs > UsageMeterMaxAge.Milliseconds():
				cov.Stale++
				live = append(live, c.obs)
			default:
				cov.Known++
				live = append(live, c.obs)
			}
		}
		// Counters of a live session age; those of an ended one are final, so
		// they only set the clock when nothing is live.
		clock := live
		observed := slices.Min[[]int64]
		if len(clock) == 0 {
			clock, observed = ended, slices.Max[[]int64]
		}
		obs := observed(clock)
		used := usageRound(sum)
		label := d.label
		if estimated {
			label = "Cost, partly estimated (selected sessions)"
		}
		out = append(out, UsageMeter{
			ID: strings.ReplaceAll(d.metric, ".", "/") + "/" + d.unit, Metric: d.metric, Label: label, Unit: d.unit,
			Used: &used, ObservedAt: obs, ExpiresAt: obs + UsageMeterMaxAge.Milliseconds(), Coverage: cov,
		})
	}
	quota, unsupported := providerMeters(linked, now, true)
	return append(out, quota...), unsupported
}

// usageMeterComparable is true for a fresh meter with a known remaining
// percentage that belongs to the slot's own quota. Account-wide quota linked
// to a session is shown but never becomes the session slot's headline.
func usageMeterComparable(m UsageMeter, now time.Time) bool {
	return m.RemainingPercent != nil && m.Coverage.Known > 0 && m.ExpiresAt > now.UnixMilli() && !strings.HasPrefix(m.Metric, usageLinkedPrefix)
}

// UsageHeadline picks the lowest fresh comparable remaining percentage. Ties
// go to the earlier expiry, then the smaller id, so the choice is stable.
func UsageHeadline(meters []UsageMeter, now time.Time) string {
	best := -1
	for i, m := range meters {
		if !usageMeterComparable(m, now) {
			continue
		}
		if best < 0 {
			best = i
			continue
		}
		b := meters[best]
		if cmp.Or(cmp.Compare(*m.RemainingPercent, *b.RemainingPercent), cmp.Compare(m.ExpiresAt, b.ExpiresAt), cmp.Compare(m.ID, b.ID)) < 0 {
			best = i
		}
	}
	if best < 0 {
		return ""
	}
	return meters[best].ID
}

// usageKeepOrder ranks meters for truncation, most important first: the
// headline, then the other comparable meters from the least remaining, then
// the rest by id.
func usageKeepOrder(meters []UsageMeter, headline string, now time.Time) []UsageMeter {
	ranked := slices.Clone(meters)
	slices.SortStableFunc(ranked, func(a, b UsageMeter) int {
		rank := func(m UsageMeter) int {
			switch {
			case m.ID == headline && headline != "":
				return 0
			case usageMeterComparable(m, now):
				return 1
			case strings.HasPrefix(m.Metric, usageLinkedPrefix):
				return 3 // account-wide quota linked to a session goes last
			}
			return 2 // measured session consumption and the slot's own quota
		}
		if c := cmp.Compare(rank(a), rank(b)); c != 0 {
			return c
		}
		if rank(a) == 1 {
			if c := cmp.Compare(*a.RemainingPercent, *b.RemainingPercent); c != 0 {
				return c
			}
		}
		return cmp.Compare(a.ID, b.ID)
	})
	return ranked
}

// NewUsageSlot assembles a slot: the headline is chosen from every meter
// first, truncation to 16 meters keeps it, and the number of dropped groups is
// exposed. omitted carries groups already left out by the caller.
func NewUsageSlot(scope, kind, label string, meters []UsageMeter, omitted int, now time.Time) UsageSlotDoc {
	headline := UsageHeadline(meters, now)
	kept := meters
	if len(meters) > UsageMetersMaxPerSlot {
		kept = usageKeepOrder(meters, headline, now)[:UsageMetersMaxPerSlot]
		omitted += len(meters) - UsageMetersMaxPerSlot
	}
	kept = slices.Clone(kept)
	slices.SortFunc(kept, func(a, b UsageMeter) int {
		if a.ID == headline {
			return -1
		}
		if b.ID == headline {
			return 1
		}
		return cmp.Compare(a.ID, b.ID)
	})
	if kept == nil {
		kept = []UsageMeter{}
	}
	return UsageSlotDoc{Slot: UsageSlotID{Scope: scope, Kind: kind, Label: UsageLabel(label, 80)}, Headline: headline, OmittedGroups: omitted, Meters: kept}
}

// Fit drops the least important meters, from the fullest slot, until the
// encoded document is within max bytes. A slot's headline is dropped last.
func (d *UsageMeters) Fit(max int) error {
	now := time.UnixMilli(d.GeneratedAt)
	for {
		b, err := json.Marshal(d)
		if err != nil {
			return err
		}
		if len(b) <= max {
			return nil
		}
		fullest := -1
		for i, s := range d.Slots {
			if len(s.Meters) > 0 && (fullest < 0 || len(s.Meters) > len(d.Slots[fullest].Meters)) {
				fullest = i
			}
		}
		if fullest < 0 {
			return fmt.Errorf("usage document: %d bytes without meters exceeds %d", len(b), max)
		}
		s := &d.Slots[fullest]
		order := usageKeepOrder(s.Meters, s.Headline, now)
		drop := order[len(order)-1].ID
		s.Meters = slices.DeleteFunc(slices.Clone(s.Meters), func(m UsageMeter) bool { return m.ID == drop })
		s.OmittedGroups++
		if drop == s.Headline {
			s.Headline = ""
		}
	}
}

// ContentHash identifies the stable content: the generation time is left out,
// so rebuilding an unchanged document does not change it.
func (d UsageMeters) ContentHash() string {
	d.GeneratedAt = 0
	b, _ := json.Marshal(d)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
