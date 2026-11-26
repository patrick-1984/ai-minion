package store

import (
	"context"
	"database/sql"
	"sync"
)

// pending holds the functions to run after the commit of each open
// transaction started by [Tx]. A rolled-back transaction drops them.
var pending sync.Map // *sql.Tx -> *commitHooks

type commitHooks struct {
	mu  sync.Mutex
	fns []func()
}

// AfterCommit runs fn once the change written through ex is durable: after
// the commit of the open transaction ex (started by [Tx]), or at once when ex
// is nil, a *sql.DB or anything else that commits each statement by itself.
// When the transaction rolls back, fn never runs.
func AfterCommit(ex Execer, fn func()) {
	if tx, ok := ex.(*sql.Tx); ok {
		if v, ok := pending.Load(tx); ok {
			h := v.(*commitHooks)
			h.mu.Lock()
			h.fns = append(h.fns, fn)
			h.mu.Unlock()
			return
		}
	}
	fn()
}

func beginHooks(tx *sql.Tx) { pending.Store(tx, &commitHooks{}) }

// endHooks forgets the hooks of tx and, when committed, runs them in order.
func endHooks(tx *sql.Tx, committed bool) {
	v, ok := pending.LoadAndDelete(tx)
	if !ok || !committed {
		return
	}
	h := v.(*commitHooks)
	h.mu.Lock()
	fns := h.fns
	h.mu.Unlock()
	for _, fn := range fns {
		fn()
	}
}

// AuditObserver is told about every audit row once it is committed.
type AuditObserver func(ctx context.Context, row AuditRow)

// SetAuditObserver makes obs see every audit row this store writes, through
// [Store.InsertAudit] or any domain method, after its transaction commits.
// The audit log uses it to export each row to the log sinks exactly once and
// to tell the REST dispatcher that the handler wrote its own row.
//
// The first observer also sees the rows committed before it was set, the
// ones the start-up reconciliation of [Open] writes (role.suspend), so that
// they reach the log sinks before the Hub serves.
func (s *Store) SetAuditObserver(obs AuditObserver) {
	if obs == nil {
		s.auditObs.Store(nil)
		return
	}
	s.earlyMu.Lock()
	s.auditObs.Store(&obs)
	early := s.early
	s.early = nil
	s.earlyMu.Unlock()
	for _, row := range early {
		obs(context.Background(), row)
	}
	// The rows SQL wrote meanwhile (grant triggers fired by the start-up
	// reconciliation or by an older run that ended before it forwarded them).
	s.forwardSQLAudit()
}

// maxEarlyAudit bounds the rows held for the first observer.
const maxEarlyAudit = 256

// holdEarlyAudit keeps a committed row that no observer saw, for the first
// one; when an observer was set meanwhile, it sees the row now.
func (s *Store) holdEarlyAudit(ctx context.Context, row AuditRow) {
	s.earlyMu.Lock()
	if obs := s.auditObs.Load(); obs != nil {
		s.earlyMu.Unlock()
		(*obs)(ctx, row)
		return
	}
	if len(s.early) < maxEarlyAudit {
		s.early = append(s.early, row)
	}
	s.earlyMu.Unlock()
}
