package audit

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/ops"
)

// MaxFieldLen is the longest value of any audit field, in characters. The
// limit guards against content (message bodies, terminal output) leaking into
// the log (AD-20).
const MaxFieldLen = ops.MaxAuditFieldLen

// ErrInvalid is matched by errors.Is for every rejected event, filter or
// cursor. A [FieldError] also matches ops.ErrInvalidArgument, which the
// adapter edge maps to the invalid_argument code.
var ErrInvalid = errors.New("audit: invalid argument")

// FieldError describes one rejected field. It never contains the value.
type FieldError struct {
	Field  string
	Reason string
}

// Error implements error.
func (e *FieldError) Error() string { return "audit: " + e.Field + ": " + e.Reason }

// Is makes errors.Is true for ErrInvalid and ops.ErrInvalidArgument.
func (e *FieldError) Is(target error) bool {
	return target == ErrInvalid || target == ops.ErrInvalidArgument
}

// Event is what a caller records. Actor, Action and Outcome are required.
type Event struct {
	// Actor is who did it: a user id, an API key id, a Worker id or "system".
	Actor string
	// ActingAs is the user an admin acted as (R45); empty otherwise.
	ActingAs string
	// Action is a dotted verb such as "login", "role.set" or "worker.remove".
	Action string
	// Target is what the action touched (a resource id); may be empty.
	Target string
	// Outcome is how it ended, for example "success", "denied" or "failed".
	Outcome string
}

// Validate checks every field: required fields are non-empty, and no field is
// longer than [MaxFieldLen] characters, invalid UTF-8 or contains a control
// character.
func (e Event) Validate() error {
	for _, f := range []struct {
		name     string
		v        string
		required bool
	}{
		{"actor", e.Actor, true},
		{"acting_as", e.ActingAs, false},
		{"action", e.Action, true},
		{"target", e.Target, false},
		{"outcome", e.Outcome, true},
	} {
		if err := checkField(f.name, f.v, f.required); err != nil {
			return err
		}
	}
	return nil
}

func checkField(name, v string, required bool) error {
	if v == "" {
		if required {
			return &FieldError{name, "is required"}
		}
		return nil
	}
	if !utf8.ValidString(v) {
		return &FieldError{name, "is not valid UTF-8"}
	}
	if utf8.RuneCountInString(v) > MaxFieldLen {
		return &FieldError{name, fmt.Sprintf("is longer than %d characters; audit events carry identifiers, never content", MaxFieldLen)}
	}
	for _, r := range v {
		if unicode.IsControl(r) {
			return &FieldError{name, "contains a control character"}
		}
	}
	return nil
}

// Entry is a stored event.
type Entry struct {
	ID   string    // UUIDv7
	Time time.Time // UTC, millisecond precision
	Event
}

// Filter selects events for [Log.List]. The zero value selects everything.
type Filter struct {
	Actor  string    // exact match; empty matches any
	Action string    // exact match; empty matches any
	From   time.Time // events at or after From; zero means no lower bound
	To     time.Time // events before To; zero means no upper bound
	Cursor string    // NextCursor of the previous page; empty starts at the newest
	Limit  int       // 0 means [ops.DefaultAuditListLimit]
}

// Page is one page of events, newest first. NextCursor is empty on the last
// page.
type Page struct {
	Events     []Entry
	NextCursor string
}

// Repository is the storage port of the log; *store.Store implements it.
type Repository interface {
	InsertAudit(ctx context.Context, ex store.Execer, row store.AuditRow) error
	ListAudit(ctx context.Context, q store.AuditQuery) ([]store.AuditRow, error)
}

// Option changes how [New] builds a [Log]. They exist for tests.
type Option func(*Log)

// WithClock sets the clock that stamps events.
func WithClock(c store.Clock) Option { return func(l *Log) { l.clock = c } }

// WithIDFunc sets the identifier generator (default: [store.NewID]).
func WithIDFunc(f func() string) Option { return func(l *Log) { l.newID = f } }

// Log writes and reads audit events. It is safe for concurrent use.
type Log struct {
	repo  Repository
	clock store.Clock
	newID func() string
}

// New returns a Log on repo.
func New(repo Repository, opts ...Option) *Log {
	l := &Log{repo: repo, clock: time.Now, newID: store.NewID}
	for _, o := range opts {
		o(l)
	}
	if l.clock == nil {
		l.clock = time.Now
	}
	return l
}

// Record validates ev and writes it as one row with a UUIDv7 id and the
// current time in unix milliseconds, in a transaction of its own. A rejected
// event is not written; the error matches [ErrInvalid].
func (l *Log) Record(ctx context.Context, ev Event) error {
	return l.record(ctx, nil, ev)
}

// RecordTx is [Log.Record] inside the caller's open transaction tx, so the
// audit row and the change it describes commit or roll back together.
func (l *Log) RecordTx(ctx context.Context, tx store.Execer, ev Event) error {
	if tx == nil {
		return errors.New("audit: RecordTx needs a transaction")
	}
	return l.record(ctx, tx, ev)
}

func (l *Log) record(ctx context.Context, ex store.Execer, ev Event) error {
	if err := ev.Validate(); err != nil {
		return err
	}
	return l.repo.InsertAudit(ctx, ex, store.AuditRow{
		ID:       l.newID(),
		TS:       store.UnixMS(l.clock()),
		Actor:    ev.Actor,
		ActingAs: ev.ActingAs,
		Action:   ev.Action,
		Target:   ev.Target,
		Outcome:  ev.Outcome,
	})
}

// List returns one page of events, newest first.
func (l *Log) List(ctx context.Context, f Filter) (Page, error) {
	limit := f.Limit
	if limit == 0 {
		limit = ops.DefaultAuditListLimit
	}
	if limit < 0 || limit > ops.MaxAuditListLimit {
		return Page{}, &FieldError{"limit", fmt.Sprintf("must be between 0 and %d", ops.MaxAuditListLimit)}
	}
	for _, v := range []struct{ name, v string }{{"actor", f.Actor}, {"action", f.Action}} {
		if utf8.RuneCountInString(v.v) > MaxFieldLen || !utf8.ValidString(v.v) {
			return Page{}, &FieldError{v.name, fmt.Sprintf("must be valid UTF-8 of at most %d characters", MaxFieldLen)}
		}
	}
	q := store.AuditQuery{Actor: f.Actor, Action: f.Action, Limit: limit + 1}
	if !f.From.IsZero() {
		ms := store.UnixMS(f.From)
		q.From = &ms
	}
	if !f.To.IsZero() {
		ms := store.UnixMS(f.To)
		q.To = &ms
	}
	if q.From != nil && q.To != nil && *q.From >= *q.To {
		return Page{}, &FieldError{"to", "must be after from"}
	}
	if f.Cursor != "" {
		pos, err := decodeCursor(f.Cursor)
		if err != nil {
			return Page{}, err
		}
		q.After = &pos
	}

	rows, err := l.repo.ListAudit(ctx, q)
	if err != nil {
		return Page{}, err
	}
	var next string
	if len(rows) > limit { // one extra row proves there is a next page
		rows = rows[:limit]
		last := rows[limit-1]
		next = encodeCursor(store.AuditPos{TS: last.TS, ID: last.ID})
	}
	page := Page{Events: make([]Entry, len(rows)), NextCursor: next}
	for i, r := range rows {
		page.Events[i] = Entry{
			ID:   r.ID,
			Time: store.FromUnixMS(r.TS),
			Event: Event{
				Actor: r.Actor, ActingAs: r.ActingAs, Action: r.Action, Target: r.Target, Outcome: r.Outcome,
			},
		}
	}
	return page, nil
}

// ListOp serves the audit_list operation: it maps the catalog arguments to a
// [Filter] and the page to the catalog result. The caller (the REST
// dispatcher) has already checked that the caller holds the manage action.
func (l *Log) ListOp(ctx context.Context, args ops.AuditListArgs) (ops.AuditListResult, error) {
	if err := args.Validate(); err != nil {
		return ops.AuditListResult{}, err
	}
	f := Filter{Actor: args.Actor, Action: args.Action, Cursor: args.Cursor, Limit: args.Limit}
	if args.From != nil {
		f.From = args.From.Time()
	}
	if args.To != nil {
		f.To = args.To.Time()
	}
	page, err := l.List(ctx, f)
	if err != nil {
		return ops.AuditListResult{}, err
	}
	res := ops.AuditListResult{Events: make([]ops.AuditEvent, len(page.Events)), NextCursor: page.NextCursor}
	for i, e := range page.Events {
		res.Events[i] = ops.AuditEvent{
			ID: e.ID, TS: ops.Timestamp(e.Time),
			Actor: e.Actor, ActingAs: e.ActingAs, Action: e.Action, Target: e.Target, Outcome: e.Outcome,
		}
	}
	return res, nil
}

// The cursor is the position (ts, id) of the last event of a page, as
// base64url("<ts>:<id>"). It is opaque to callers.
func encodeCursor(p store.AuditPos) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(p.TS, 10) + ":" + p.ID))
}

func decodeCursor(s string) (store.AuditPos, error) {
	bad := &FieldError{"cursor", "is not a cursor returned by the audit log"}
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return store.AuditPos{}, bad
	}
	tsText, id, ok := strings.Cut(string(raw), ":")
	if !ok || id == "" || len(id) > 64 {
		return store.AuditPos{}, bad
	}
	ts, err := strconv.ParseInt(tsText, 10, 64)
	if err != nil {
		return store.AuditPos{}, bad
	}
	return store.AuditPos{TS: ts, ID: id}, nil
}
