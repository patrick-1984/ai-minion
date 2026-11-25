package ops

import (
	"encoding/json"
	"reflect"
	"strings"
)

// OpGrantHistory is the name of the Hub operation that reads the audit rows
// of one session grant: how it was created, changed, used and ended.
const OpGrantHistory = "grant_history"

// GrantHistoryArgs name the grant and page through its events.
type GrantHistoryArgs struct {
	// ID is the grant; it may be a grant that has ended.
	ID string `json:"id"`
	// Cursor is the next_cursor of the previous page; empty starts at the
	// newest event.
	Cursor string `json:"cursor,omitempty"`
	// Limit caps the events returned; 0 means DefaultAuditListLimit.
	Limit int `json:"limit,omitempty"`
}

// Validate checks the grant id, the cursor and the limit.
func (a GrantHistoryArgs) Validate() error {
	if !ValidUUIDv7(a.ID) {
		return invalid("id", "must be a UUIDv7")
	}
	if len(a.Cursor) > MaxAuditCursorLen || strings.ContainsAny(a.Cursor, " \t\r\n") {
		return invalid("cursor", "is not a cursor returned by grant_history")
	}
	if a.Limit < 0 || a.Limit > MaxAuditListLimit {
		return invalid("limit", "must be between 0 and %d", MaxAuditListLimit)
	}
	return nil
}

// GrantHistoryResult is the result of grant_history: the audit events of the
// grant, newest first. NextCursor is empty on the last page.
type GrantHistoryResult struct {
	Events     []AuditEvent `json:"events"`
	NextCursor string       `json:"next_cursor,omitempty"`
}

// MarshalJSON writes events as an empty array instead of null.
func (r GrantHistoryResult) MarshalJSON() ([]byte, error) {
	type plain GrantHistoryResult
	if r.Events == nil {
		r.Events = []AuditEvent{}
	}
	return json.Marshal(plain(r))
}

func init() {
	Register(Op{
		Name:    OpGrantHistory,
		Summary: "List the audit events of one session grant, newest first (owner of a Worker it touches or administrator).",
		Scope:   ScopeHub, Actions: MustActionSet(ActionOperate),
		Args: reflect.TypeFor[GrantHistoryArgs](), Result: reflect.TypeFor[GrantHistoryResult](),
	})
}
