package ops

import (
	"reflect"
	"strings"
)

const (
	OpUsageList       = "usage_list"
	OpUsageBucketList = "usage_bucket_list"
	OpUsageBucketSet  = "usage_bucket_set"
	OpWorkerUsage     = "worker_usage"
)

type WorkerUsageArgs struct {
	AfterEvent int64  `json:"after_event,omitempty"`
	AccountID  string `json:"account_id,omitempty"` // internal account lease only
	WorkerID   string `json:"worker_id"`
	Inventory  bool   `json:"inventory,omitempty"`
}

type UsageListArgs struct {
	Section          string `json:"section"` // sessions, accounts or workers
	WorkerID         string `json:"worker_id,omitempty"`
	BucketID         string `json:"bucket_id,omitempty"` // "unassigned" or a private bucket id
	SubscriptionOnly bool   `json:"subscription_only,omitempty"`
	After            string `json:"after,omitempty"`
	Limit            int    `json:"limit,omitempty"`
}

func (a UsageListArgs) Validate() error {
	if a.Section != "sessions" && a.Section != "accounts" && a.Section != "workers" && a.Section != "overview" && a.Section != "events" && a.Section != "history" {
		return invalid("section", "choose sessions, accounts, workers, overview, events or history")
	}
	if a.Limit < 0 || a.Limit > 200 {
		return invalid("limit", "choose 1 to 200")
	}
	for _, s := range []string{a.WorkerID, a.BucketID} {
		if !usageText(s, 128) {
			return invalid("filter", "invalid filter")
		}
	}
	for _, s := range []string{a.After} {
		// A session cursor holds a full sort key: Worker name, session name, ID.
		if !usageText(s, 1280) {
			return invalid("filter", "invalid filter")
		}
	}
	if a.SubscriptionOnly && a.Section != "accounts" || a.BucketID != "" && a.Section == "workers" {
		return invalid("filter", "filter does not apply to this section")
	}
	return nil
}

type UsageSessionRow struct {
	DurationSource string `json:"duration_source,omitempty"`
	StopSource     string `json:"stop_source,omitempty"`
	UsageSession
	WorkerID string `json:"worker_id"`
	LastSeen int64  `json:"last_seen"` // successful full enumeration on this Worker for a live session
	BucketID string `json:"bucket_id,omitempty"`
}

type UsageSource struct {
	WorkerID  string `json:"worker_id"`
	SessionID string `json:"session_id,omitempty"`
	Label     string `json:"label"`
}

type UsageAccountRow struct {
	Alias     string          `json:"alias,omitempty"`
	Position  int             `json:"position"`
	Sources   []UsageSource   `json:"sources,omitempty"`
	Collector *UsageCollector `json:"collector,omitempty"`
	UsageAccount
	WorkerID string `json:"worker_id"` // origin of the most recent authorized observation
	BucketID string `json:"bucket_id,omitempty"`
	// Totals sums the sessions of an API-key or custom-endpoint account (set
	// for no other kind): the sessions the Sessions view lists, running or
	// ended within the retention window.
	Totals *UsageAccountTotals `json:"totals,omitempty"`
}

// UsageAccountTotals is the sum of one account's sessions. A sum is nil when no
// session reported that counter, so unknown stays distinct from zero.
type UsageAccountTotals struct {
	Sessions         int      `json:"sessions"`
	InputTokens      *int64   `json:"input_tokens"`
	OutputTokens     *int64   `json:"output_tokens"`
	CostUSD          *float64 `json:"cost_usd"`
	EstimatedCostUSD *float64 `json:"estimated_cost_usd"`
}

type UsageWorkerRow struct {
	LastIssue   string `json:"last_issue,omitempty"`
	InventoryAt *int64 `json:"inventory_at"`
	Online      bool   `json:"online"`
	Status      string `json:"status"` // ready, waiting, offline, unsupported, registration_required, unavailable

	ID         string         `json:"id"`
	Name       string         `json:"name"`
	ObservedAt *int64         `json:"observed_at"`
	Inventory  []UsageHarness `json:"inventory"`
}

type UsageEventRow struct {
	UsageEvent
	WorkerID string `json:"worker_id"`
}
type UsageHistoryRow struct {
	SessionID  string        `json:"session_id"`
	WorkerID   string        `json:"worker_id"`
	Hour       int64         `json:"hour"` // UTC hour start, milliseconds
	ObservedAt int64         `json:"observed_at"`
	Counters   UsageCounters `json:"counters"` // latest cumulative snapshot of this interval
}

type UsageListResult struct {
	Events   []UsageEventRow   `json:"events,omitempty"`
	History  []UsageHistoryRow `json:"history,omitempty"`
	Overview *UsageOverview    `json:"overview,omitempty"`
	Version  int               `json:"version"`
	AsOf     int64             `json:"as_of"`
	Sessions []UsageSessionRow `json:"sessions,omitempty"`
	Accounts []UsageAccountRow `json:"accounts,omitempty"`
	Workers  []UsageWorkerRow  `json:"workers,omitempty"`
	Next     string            `json:"next,omitempty"`
}

type UsageBucket struct {
	ID        string   `json:"id"`
	Kind      string   `json:"kind"`
	Label     string   `json:"label"`
	Position  int      `json:"position"`
	Nominated []string `json:"nominated,omitempty"` // "user" and/or "domain": where this bucket is published
}

type UsageBucketListArgs struct{}
type UsageBucketListResult struct {
	Buckets []UsageBucket `json:"buckets"`
	// AllowRepublish is your own setting: whether buckets published by
	// someone else may include observations of your sessions and accounts.
	AllowRepublish bool `json:"allow_republish"`
}

type UsageBucketSetArgs struct {
	Action    string   `json:"action"` // save, delete, assign
	Kind      string   `json:"kind"`
	BucketID  string   `json:"bucket_id,omitempty"` // empty on assign removes membership
	Label     string   `json:"label,omitempty"`
	Position  int      `json:"position,omitempty"`
	Members   []string `json:"members,omitempty"`    // canonical member IDs
	WorkerIDs []string `json:"worker_ids,omitempty"` // all visible members on these Workers
	All       bool     `json:"all,omitempty"`
}

func (a UsageBucketSetArgs) Validate() error {
	if a.Kind != "session" && a.Kind != "provider" {
		return invalid("kind", "choose session or provider")
	}
	if !usageText(a.BucketID, 128) {
		return invalid("bucket_id", "invalid id")
	}
	if a.Position < 0 || a.Position > 1000000 {
		return invalid("position", "out of range")
	}
	switch a.Action {
	case "save":
		if strings.TrimSpace(a.Label) == "" || !usageText(a.Label, 80) {
			return invalid("label", "a name of at most 80 bytes is required")
		}
	case "delete":
		if a.BucketID == "" {
			return invalid("bucket_id", "is required")
		}
	case "assign":
		if len(a.Members) > 512 || len(a.WorkerIDs) > 256 || !a.All && len(a.Members)+len(a.WorkerIDs) == 0 {
			return invalid("members", "select all, workers, or up to 512 identities")
		}
		for _, id := range a.Members {
			if !usageIDValid(id) {
				return invalid("members", "invalid identity")
			}
		}
		for _, id := range a.WorkerIDs {
			if !usageText(id, 128) || id == "" {
				return invalid("worker_ids", "invalid Worker id")
			}
		}
	default:
		return invalid("action", "choose save, delete or assign")
	}
	return nil
}

type UsageBucketSetResult struct {
	ID      string `json:"id,omitempty"`
	Updated int64  `json:"updated"`
}

func init() {
	read := MustActionSet(ActionRead)
	Register(Op{Name: OpUsageList, Summary: "Read authorized Worker, session or account usage statistics, including metadata for unshared sessions on your Workers.", Scope: ScopeHub, Actions: read, Args: reflect.TypeFor[UsageListArgs](), Result: reflect.TypeFor[UsageListResult]()})
	Register(Op{Name: OpUsageBucketList, Summary: "List your private usage buckets.", Scope: ScopeHub, Actions: read, Args: reflect.TypeFor[UsageBucketListArgs](), Result: reflect.TypeFor[UsageBucketListResult]()})
	Register(Op{Name: OpUsageBucketSet, Summary: "Save, reorder, delete or assign members to your private usage buckets.", Scope: ScopeHub, Actions: MustActionSet(ActionOperate), Mutates: true, Args: reflect.TypeFor[UsageBucketSetArgs](), Result: reflect.TypeFor[UsageBucketSetResult]()})
	Register(Op{Name: OpWorkerUsage, Summary: "Read registered Worker usage metadata and optionally refresh its installed harness inventory.", Scope: ScopeWorker, Actions: read, Args: reflect.TypeFor[WorkerUsageArgs](), Result: reflect.TypeFor[UsageReport]()})
}

const OpUsageCollectorSet = "usage_collector_set"

type UsageCollectorSetArgs struct {
	Start     bool   `json:"start,omitempty"` // queue one native query at first eligible instant
	AccountID string `json:"account_id"`
	WorkerID  string `json:"worker_id,omitempty"`  // empty restores automatic election
	SessionID string `json:"session_id,omitempty"` // passive metric source, never a console target
}

func (a UsageCollectorSetArgs) Validate() error {
	if a.Start && a.SessionID != "" {
		return invalid("start", "select a Worker or automatic election for a native background query")
	}
	if !usageIDValid(a.AccountID) || a.SessionID != "" && (!usageIDValid(a.SessionID) || a.WorkerID == "") || !usageText(a.WorkerID, 128) {
		return invalid("collector", "invalid account, Worker or session")
	}
	return nil
}

type UsageCollector struct {
	CanStart         bool   `json:"can_start"`
	Mode             string `json:"mode"` // native_background or passive_events
	PreferredWorker  string `json:"preferred_worker,omitempty"`
	PreferredSession string `json:"preferred_session,omitempty"`
	ActiveWorker     string `json:"active_worker,omitempty"`
	Status           string `json:"status"`
	NextDue          *int64 `json:"next_due"`
	LastSuccess      *int64 `json:"last_success"`
}

func init() {
	Register(Op{Name: OpUsageCollectorSet, Summary: "Choose an available Worker or passive session source for this subscription account.", Scope: ScopeHub, Actions: MustActionSet(ActionOperate), Mutates: true, Args: reflect.TypeFor[UsageCollectorSetArgs](), Result: reflect.TypeFor[UsageCollector]()})
}

const OpUsageAccountSet = "usage_account_set"

type UsageAccountSetArgs struct {
	AccountID string `json:"account_id"`
	Label     string `json:"label"`
	Position  int    `json:"position"`
}

func (a UsageAccountSetArgs) Validate() error {
	if !usageIDValid(a.AccountID) || !usageText(a.Label, 80) || a.Position < 0 || a.Position > 1000000 {
		return invalid("account", "invalid personal account name, identity or position")
	}
	return nil
}
func init() {
	Register(Op{Name: OpUsageAccountSet, Summary: "Set your private display name and order for a provider account.", Scope: ScopeHub, Actions: MustActionSet(ActionOperate), Mutates: true, Args: reflect.TypeFor[UsageAccountSetArgs](), Result: reflect.TypeFor[UsageAccountSetArgs]()})
}

const (
	OpUsageNominationSet    = "usage_nomination_set"
	OpUsageNominationList   = "usage_nomination_list"
	OpUsagePublicationSet   = "usage_publication_set"
	UsageScopeUser          = "user"
	UsageScopeDomain        = "domain"
	UsageNominationNominate = "nominate"
	UsageNominationClear    = "clear"
)

// UsageNominationSetArgs publishes one of your own buckets to Workers or
// withdraws a slot. Scope user shows the bucket on your own Workers; scope
// domain shows it on every Worker of the Hub and needs the manage permission
// of an administrator. A domain clear needs no bucket: any such administrator
// can clear the slot another one filled. A bucket_id on a clear is a
// precondition: the slot is cleared only while that bucket still holds it.
type UsageNominationSetArgs struct {
	Action   string `json:"action"` // nominate or clear
	Scope    string `json:"scope"`  // user or domain
	Kind     string `json:"kind"`   // session or provider
	BucketID string `json:"bucket_id,omitempty"`
}

func (a UsageNominationSetArgs) Validate() error {
	if a.Scope != UsageScopeUser && a.Scope != UsageScopeDomain {
		return invalid("scope", "choose user or domain")
	}
	if a.Kind != "session" && a.Kind != "provider" {
		return invalid("kind", "choose session or provider")
	}
	if !usageText(a.BucketID, 128) {
		return invalid("bucket_id", "invalid id")
	}
	switch a.Action {
	case UsageNominationNominate:
		if a.BucketID == "" {
			return invalid("bucket_id", "is required")
		}
	case UsageNominationClear:
	default:
		return invalid("action", "choose nominate or clear")
	}
	return nil
}

type UsageNominationSetResult struct {
	Changed bool `json:"changed"`
	// Replaced is true when a different bucket held the slot before.
	Replaced bool `json:"replaced,omitempty"`
}

// UsageDomainSlot is one Hub-wide published slot with who put it there.
type UsageDomainSlot struct {
	Kind          string `json:"kind"`
	BucketID      string `json:"bucket_id"`
	Label         string `json:"label"`
	PublisherID   string `json:"publisher_id"`
	PublisherName string `json:"publisher_name"`
	UpdatedBy     string `json:"updated_by"`
	UpdatedAt     int64  `json:"updated_at"`
}

type UsageNominationListArgs struct{}
type UsageNominationListResult struct {
	Domain []UsageDomainSlot `json:"domain"`
}

// UsagePublicationSetArgs is the source owner's permission, default off.
type UsagePublicationSetArgs struct {
	AllowRepublish bool `json:"allow_republish"`
}

func init() {
	Register(Op{Name: OpUsageNominationSet, Summary: "Publish one of your usage buckets to your Workers, or to every Worker of the Hub (administrators with manage), or withdraw a slot.", Scope: ScopeHub, Actions: MustActionSet(ActionOperate), Mutates: true, Args: reflect.TypeFor[UsageNominationSetArgs](), Result: reflect.TypeFor[UsageNominationSetResult]()})
	Register(Op{Name: OpUsageNominationList, Summary: "Administrators: show the Hub-wide published usage slots and who published them.", Scope: ScopeHub, Actions: MustActionSet(ActionManage), Args: reflect.TypeFor[UsageNominationListArgs](), Result: reflect.TypeFor[UsageNominationListResult]()})
	Register(Op{Name: OpUsagePublicationSet, Summary: "Allow or refuse that buckets published by others include observations of your own sessions and accounts (default refuse).", Scope: ScopeHub, Actions: MustActionSet(ActionOperate), Mutates: true, Args: reflect.TypeFor[UsagePublicationSetArgs](), Result: reflect.TypeFor[UsagePublicationSetArgs]()})
}

// ErrUsageCursor is the error of a session cursor that this Hub did not issue.
var ErrUsageCursor = invalid("after", "invalid cursor")
