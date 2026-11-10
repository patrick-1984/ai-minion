package ops

import (
	"encoding/json"
	"reflect"
	"strings"
	"unicode/utf8"
)

// OpAuditList is the name of the Hub operation that reads the audit log.
const OpAuditList = "audit_list"

// Limits of audit_list.
const (
	// DefaultAuditListLimit is used when audit_list.limit is 0.
	DefaultAuditListLimit = 50
	// MaxAuditListLimit caps the events of one audit_list call.
	MaxAuditListLimit = 200
	// MaxAuditFieldLen caps every audit field and filter value in characters.
	MaxAuditFieldLen = 256
	// MaxAuditCursorLen caps the opaque cursor in bytes.
	MaxAuditCursorLen = 512
)

// AuditListArgs are the arguments of audit_list. Every field filters or pages;
// all are optional.
type AuditListArgs struct {
	// Actor keeps only events of this actor (exact match).
	Actor string `json:"actor,omitempty"`
	// Action keeps only events with this action (exact match).
	Action string `json:"action,omitempty"`
	// From keeps events at or after this time.
	From *Timestamp `json:"from,omitempty"`
	// To keeps events before this time.
	To *Timestamp `json:"to,omitempty"`
	// Cursor is the next_cursor of the previous page; empty starts at the
	// newest event.
	Cursor string `json:"cursor,omitempty"`
	// Limit caps the events returned; 0 means DefaultAuditListLimit.
	Limit int `json:"limit,omitempty"`
}

// Validate checks the filters, the time range, the cursor and the limit.
func (a AuditListArgs) Validate() error {
	for _, f := range []struct{ name, v string }{{"actor", a.Actor}, {"action", a.Action}} {
		if utf8.RuneCountInString(f.v) > MaxAuditFieldLen || !utf8.ValidString(f.v) {
			return invalid(f.name, "must be valid UTF-8 of at most %d characters", MaxAuditFieldLen)
		}
	}
	if a.From != nil && a.To != nil && !a.From.Time().Before(a.To.Time()) {
		return invalid("to", "must be after from")
	}
	if len(a.Cursor) > MaxAuditCursorLen || strings.ContainsAny(a.Cursor, " \t\r\n") {
		return invalid("cursor", "is not a cursor returned by audit_list")
	}
	if a.Limit < 0 || a.Limit > MaxAuditListLimit {
		return invalid("limit", "must be between 0 and %d", MaxAuditListLimit)
	}
	return nil
}

// EffectiveLimit returns Limit or DefaultAuditListLimit when it is 0.
func (a AuditListArgs) EffectiveLimit() int {
	if a.Limit == 0 {
		return DefaultAuditListLimit
	}
	return a.Limit
}

// AuditEvent is one audit log entry. It never carries content.
type AuditEvent struct {
	ID       string    `json:"id"` // UUIDv7
	TS       Timestamp `json:"ts"`
	Actor    string    `json:"actor"`
	ActingAs string    `json:"acting_as,omitempty"` // the user an admin acted as
	Action   string    `json:"action"`
	Target   string    `json:"target,omitempty"`
	Outcome  string    `json:"outcome"`
}

// AuditListResult is the result of audit_list: events newest first. NextCursor
// is empty on the last page.
type AuditListResult struct {
	Events     []AuditEvent `json:"events"`
	NextCursor string       `json:"next_cursor,omitempty"`
}

// MarshalJSON writes events as an empty array instead of null.
func (r AuditListResult) MarshalJSON() ([]byte, error) {
	type plain AuditListResult
	if r.Events == nil {
		r.Events = []AuditEvent{}
	}
	return json.Marshal(plain(r))
}

func init() {
	Register(Op{
		Name:    OpAuditList,
		Summary: "List audit log events, newest first.",
		Scope:   ScopeHub, Actions: MustActionSet(ActionManage),
		Args: reflect.TypeFor[AuditListArgs](), Result: reflect.TypeFor[AuditListResult](),
	})
}
