package web

import (
	"fmt"
	"strings"
	"time"

	"github.com/patrick-1984/ai-minion/internal/ops"
)

// usageSlotKinds are the two published kinds, in display order.
var usageSlotKinds = []string{"provider", "session"}

// usageNominate is the model of one publish or withdraw control.
type usageNominate struct {
	Scope, Kind, BucketID, Confirm, ForUser, Holder string
	Active                                          bool
}

type usageBucketCard struct {
	Bucket       ops.UsageBucket
	User, Domain usageNominate
	KindLabel    string
}

type usageDomainSlotView struct {
	Kind, Title string
	Held        bool
	Slot        ops.UsageDomainSlot
	Updated     string
}

type usagePreviewMeter struct {
	Label, Remaining, Used, Unit, Reset, Pace, Freshness, Coverage string
	Headline                                                       bool
}

type usagePreviewSlot struct {
	Title, Scope string
	Omitted      int
	Meters       []usagePreviewMeter
}

type usagePreview struct {
	Unavailable bool
	Generated   string
	Slots       []usagePreviewSlot
	Empty       bool
}

// usageConfirmation names the data, the receivers and the local-read exposure.
func usageConfirmation(label, scope string) string {
	receivers := "your own Workers"
	if scope == ops.UsageScopeDomain {
		receivers = "every approved Worker of this Hub"
	}
	return fmt.Sprintf("This publishes the bucket label “%s” and its aggregate meters (remaining, used, limit, reset, pace and freshness). "+
		"It does not publish session names, folders, account labels or member ids. It is shown on %s. "+
		"Every local account on a computer that runs a root Worker can read that Worker's copy.", label, receivers)
}

func (p *usagePage) buildBuckets(domain []ops.UsageDomainSlot) {
	held := map[string]string{}
	for _, b := range p.Buckets {
		for _, s := range b.Nominated {
			if s == ops.UsageScopeUser {
				held[b.Kind] = b.Label
			}
		}
	}
	bySlot := map[string]ops.UsageDomainSlot{}
	for _, s := range domain {
		bySlot[s.Kind] = s
	}
	for _, b := range p.Buckets {
		mk := func(scope string) usageNominate {
			return usageNominate{Scope: scope, Kind: b.Kind, BucketID: b.ID, ForUser: p.ForUser, Confirm: usageConfirmation(b.Label, scope)}
		}
		c := usageBucketCard{Bucket: b, KindLabel: usageLabel(b.Kind), User: mk(ops.UsageScopeUser), Domain: mk(ops.UsageScopeDomain)}
		for _, s := range b.Nominated {
			switch s {
			case ops.UsageScopeUser:
				c.User.Active = true
			case ops.UsageScopeDomain:
				c.Domain.Active = true
			}
		}
		if h, ok := held[b.Kind]; ok && !c.User.Active {
			c.User.Holder = h
		}
		if s, ok := bySlot[b.Kind]; ok {
			if s.BucketID == b.ID {
				c.Domain.Active = true
			} else {
				c.Domain.Holder = s.Label
			}
		}
		p.BucketCards = append(p.BucketCards, c)
	}
	for _, kind := range usageSlotKinds {
		v := usageDomainSlotView{Kind: kind, Title: usageLabel(kind) + " slot"}
		if s, ok := bySlot[kind]; ok {
			v.Held, v.Slot = true, s
			v.Updated = p.Millis(s.UpdatedAt)
		}
		p.DomainSlots = append(p.DomainSlots, v)
	}
}

func usageMeterValue(unit string, v *float64) string {
	if v == nil {
		return "Unknown"
	}
	switch unit {
	case "percent":
		return fmt.Sprintf("%.1f%%", *v)
	case "usd":
		return fmt.Sprintf("$%.2f", *v)
	case "ms":
		return (time.Duration(*v) * time.Millisecond).Round(time.Second).String()
	default:
		return fmt.Sprintf("%.0f", *v)
	}
}

func usagePaceLabel(pace string) string {
	switch pace {
	case "ahead":
		return "Ahead of pace"
	case "below":
		return "Within pace"
	case "on_pace":
		return "On pace"
	default:
		return "Unknown pace"
	}
}

func buildUsagePreview(doc ops.UsageMeters) usagePreview {
	gen := time.UnixMilli(doc.GeneratedAt)
	out := usagePreview{Generated: usagePage{}.Millis(doc.GeneratedAt)}
	for _, s := range doc.Slots {
		slot := usagePreviewSlot{Title: usageLabel(s.Slot.Kind) + " · " + s.Slot.Label, Scope: map[string]string{ops.UsageScopeDomain: "Every Worker", ops.UsageScopeUser: "Your Workers"}[s.Slot.Scope], Omitted: s.OmittedGroups}
		if slot.Scope == "" {
			slot.Scope = usageLabel(s.Slot.Scope)
		}
		for _, m := range s.Meters {
			pm := usagePreviewMeter{Label: m.Label, Unit: m.Unit, Headline: m.ID == s.Headline, Pace: usagePaceLabel(m.Pace), Reset: "Unknown"}
			pm.Used = usageMeterValue(m.Unit, m.Used)
			if m.Used == nil && m.UsedPercent != nil {
				pm.Used = usageMeterValue("percent", m.UsedPercent)
			}
			pm.Remaining = usageMeterValue(m.Unit, m.Remaining)
			if m.Remaining == nil && m.RemainingPercent != nil {
				pm.Remaining = usageMeterValue("percent", m.RemainingPercent)
			}
			if m.Limit != nil {
				pm.Remaining += " of " + usageMeterValue(m.Unit, m.Limit)
			}
			if m.ResetsAt != nil {
				pm.Reset = usagePage{}.Millis(*m.ResetsAt)
			}
			switch {
			case m.ExpiresAt != 0 && !gen.Before(time.UnixMilli(m.ExpiresAt)):
				pm.Freshness = "Expired"
			case gen.Sub(time.UnixMilli(m.ObservedAt)) > ops.UsageMeterMaxAge:
				pm.Freshness = "Stale"
			default:
				pm.Freshness = "Fresh"
			}
			pm.Coverage = fmt.Sprintf("%d fresh · %d stale · %d unknown", m.Coverage.Known, m.Coverage.Stale, m.Coverage.Unknown)
			slot.Meters = append(slot.Meters, pm)
		}
		out.Slots = append(out.Slots, slot)
	}
	out.Empty = len(doc.Slots) == 0
	return out
}

func usageNominationForm(r interface{ PostFormValue(string) string }) (ops.UsageNominationSetArgs, error) {
	a := ops.UsageNominationSetArgs{Action: r.PostFormValue("action"), Scope: r.PostFormValue("scope"), Kind: r.PostFormValue("kind"), BucketID: r.PostFormValue("bucket")}
	if a.Action == ops.UsageNominationNominate && r.PostFormValue("confirm") != "yes" {
		return a, fmt.Errorf("confirm what will be published before publishing")
	}
	if a.Action == ops.UsageNominationClear && a.Scope == ops.UsageScopeDomain {
		a.BucketID = ""
	}
	if err := a.Validate(); err != nil {
		return a, fmt.Errorf("%s", strings.TrimSpace(err.Error()))
	}
	return a, nil
}
