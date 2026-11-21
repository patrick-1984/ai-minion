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
	WorkerID  string `json:"worker_id"`
	Inventory bool   `json:"inventory,omitempty"`
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
	if a.Section != "sessions" && a.Section != "accounts" && a.Section != "workers" && a.Section != "overview" {
		return invalid("section", "choose sessions, accounts, workers or overview")
	}
	if a.Limit < 0 || a.Limit > 200 {
		return invalid("limit", "choose 1 to 200")
	}
	for _, s := range []string{a.After, a.WorkerID, a.BucketID} {
		if !usageText(s, 128) {
			return invalid("filter", "invalid filter")
		}
	}
	if a.SubscriptionOnly && a.Section != "accounts" || a.BucketID != "" && a.Section == "workers" {
		return invalid("filter", "filter does not apply to this section")
	}
	return nil
}

type UsageSessionRow struct {
	UsageSession
	WorkerID string `json:"worker_id"`
	LastSeen int64  `json:"last_seen"` // successful full enumeration on this Worker for a live session
	BucketID string `json:"bucket_id,omitempty"`
}

type UsageAccountRow struct {
	UsageAccount
	WorkerID string `json:"worker_id"` // origin of the most recent authorized observation
	BucketID string `json:"bucket_id,omitempty"`
}

type UsageWorkerRow struct {
	ID         string         `json:"id"`
	Name       string         `json:"name"`
	ObservedAt *int64         `json:"observed_at"`
	Inventory  []UsageHarness `json:"inventory"`
}

type UsageListResult struct {
	Overview *UsageOverview    `json:"overview,omitempty"`
	Version  int               `json:"version"`
	AsOf     int64             `json:"as_of"`
	Sessions []UsageSessionRow `json:"sessions,omitempty"`
	Accounts []UsageAccountRow `json:"accounts,omitempty"`
	Workers  []UsageWorkerRow  `json:"workers,omitempty"`
	Next     string            `json:"next,omitempty"`
}

type UsageBucket struct {
	ID       string `json:"id"`
	Kind     string `json:"kind"`
	Label    string `json:"label"`
	Position int    `json:"position"`
}

type UsageBucketListArgs struct{}
type UsageBucketListResult struct {
	Buckets []UsageBucket `json:"buckets"`
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
