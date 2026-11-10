package workers

// The authoritative share rule of every Worker (Story 4.15, privacy hardening).
//
// What a Worker shares is decided on the Worker, by a rule (all, none, or the
// names selected) and a durable, monotonic revision of that rule. The Hub does
// not infer what is shared from the sessions that happen to be listed: a session
// that is not in a list may be closed, offline or unshared, and the list cannot
// tell which. It keeps the rule itself, as the Worker declares it:
//
//   - on the event stream, in the first frame of every stream and in a
//     share_changed event whenever the rule changes (also when the owner changed
//     it on the machine, with no Hub involved);
//   - in the answer of share_get and share_set, from the connection that is
//     current.
//
// With the rule, [EventHub.Fenced] answers for any session name, whether the
// Worker lists it or not, so the history the Hub stored of a session is shown
// exactly as long as the Worker's current rule shares it. The rule is written to
// the Hub database ([ShareStore]) as soon as it is applied: a restart does not
// lift a restriction, and a restriction that could not be written hides the
// whole Worker's history until it can be.
//
// Rules of one Worker are ordered by the Worker's own revision, never by the
// order in which answers arrive: an older answer cannot undo a newer rule, also
// not across the replacement of the connection. A declaration with a lower
// revision on a new connection (the Worker's database was restored or replaced)
// is accepted only if it is no wider than the rule the Hub holds.
//
// While the current rule is unknown the history is hidden (fail closed): for a
// Worker whose connection has not yet declared its rule, for a Worker that never
// declared one, and for a restriction that is requested but whose outcome is not
// known ([ShareCall]).

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// ShareStore keeps the share rule of every Worker durably. *store.Store of the
// Hub implements it. An EventHub with a ShareStore is strict: it hides what it
// stored of a session whenever the rule that governs it is not known.
type ShareStore interface {
	LoadWorkerShares(ctx context.Context) (map[string]ops.ShareDecl, error)
	SaveWorkerShare(ctx context.Context, workerID string, d ops.ShareDecl) error
	DeleteWorkerShare(ctx context.Context, workerID string) error
	// The restrictive share_set calls that were dispatched and are not settled
	// (0018_worker_share_pending.sql): written before the request is sent, read
	// back after a restart.
	LoadPendingShares(ctx context.Context) ([]store.PendingShare, error)
	SavePendingShare(ctx context.Context, p store.PendingShare) error
	DeletePendingShares(ctx context.Context, workerID string, upTo uint64) error
	DeletePendingShare(ctx context.Context, workerID string, seq uint64) error
}

// shareSaveTimeout bounds one write of a rule to the database.
const shareSaveTimeout = 5 * time.Second

// shareState is what the Hub holds of one Worker's share rule.
type shareState struct {
	rule  ops.ShareRule
	rev   uint64
	known bool
	// gen is the connection that last declared the rule; 0 when the rule was
	// only loaded from the database.
	gen uint64
	// saved is true when the held rule and revision are in the database.
	saved bool
	// denied is true when a declaration of the current connection with a lower
	// revision and a wider rule was refused (a rollback of the Worker's own
	// state): the held rule stays until the owner sets one through the Hub.
	denied bool
	// prov are the restrictions requested through the Hub and not yet settled.
	prov map[uint64]*provisionalShare
}

// provisionalShare is a restrictive share_set that was dispatched: until its
// outcome is settled the sessions its rule would hide are hidden as well. It is
// written to the database before the request is sent (when the Hub has a
// ShareStore), so that a restart does not forget it.
type provisionalShare struct {
	rule ops.ShareRule
	// seq is the Hub's sequence number of the change: its key, its row in the
	// database and the op_seq the Worker sees.
	seq uint64
	// baseRev and baseGen are the revision held and the connection when it was
	// dispatched.
	baseRev uint64
	baseGen uint64
	// unknown is true when the call ended without a usable answer after it may
	// have been delivered, and for every restriction read back from the database
	// after a restart. One that is still in flight is settled by its own call.
	// An unknown restriction is settled only by evidence ordered after it: the
	// answer of a later share_set that went through the Hub (the Worker's
	// barrier proves that no earlier change can still apply), or the Worker's
	// answer to a probe that raised the barrier to its sequence number. A
	// revision above the one the Hub held, an answer of another call, an event
	// or a reconnect settle nothing.
	unknown bool
}

func (h *EventHub) shareFor(workerID string) *shareState {
	st := h.shares[workerID]
	if st == nil {
		st = &shareState{prov: map[uint64]*provisionalShare{}}
		h.shares[workerID] = st
	}
	return st
}

// ensureSharesLocked loads the stored rules once. A failure keeps every
// history hidden and is retried at the next call.
func (h *EventHub) ensureSharesLocked() {
	if h.shareStore == nil || h.sharesLoaded {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), shareSaveTimeout)
	defer cancel()
	m, err := h.shareStore.LoadWorkerShares(ctx)
	if err != nil {
		if h.sharesLoadErr == nil {
			h.log.Error("workers: the stored share rules cannot be read; the history of every Worker stays hidden", "error", err.Error())
		}
		h.sharesLoadErr = err
		return
	}
	pend, err := h.shareStore.LoadPendingShares(ctx)
	if err != nil {
		if h.sharesLoadErr == nil {
			h.log.Error("workers: the pending share restrictions cannot be read; the history of every Worker stays hidden", "error", err.Error())
		}
		h.sharesLoadErr = err
		return
	}
	h.sharesLoaded, h.sharesLoadErr = true, nil
	for id, d := range m {
		st := h.shareFor(id)
		if st.known {
			continue
		}
		st.rule, st.rev, st.known, st.saved = canonRule(d.Rule), d.Revision, true, true
	}
	// A restriction that was dispatched and not settled before the restart is
	// of unknown outcome now: the Worker may have applied it.
	for _, p := range pend {
		st := h.shareFor(p.WorkerID)
		if _, ok := st.prov[p.Seq]; !ok {
			st.prov[p.Seq] = &provisionalShare{rule: canonRule(p.Rule), seq: p.Seq, baseRev: p.BaseRevision, unknown: true}
		}
		if p.Seq > h.lastSeq {
			h.lastSeq = p.Seq
		}
	}
}

// nextSeqLocked returns the sequence number of a new share_set: it is higher
// than every earlier one of this Hub (also across a restart, through the clock
// and the pending rows read back) so that the Worker's barrier orders them.
func (h *EventHub) nextSeqLocked() uint64 {
	n := uint64(h.now().UnixMicro())
	if n <= h.lastSeq {
		n = h.lastSeq + 1
	}
	h.lastSeq = n
	return n
}

// canonRule returns r with its names sorted and without duplicates, and without
// names unless the mode is select. An unknown mode shares nothing.
func canonRule(r ops.ShareRule) ops.ShareRule {
	switch r.Mode {
	case ops.ShareModeAll, ops.ShareModeNone, ops.ShareModeLater:
		return ops.ShareRule{Mode: r.Mode}
	case ops.ShareModeSelect:
		names := slices.Clone(r.Names)
		slices.Sort(names)
		return ops.ShareRule{Mode: ops.ShareModeSelect, Names: slices.Compact(names)}
	}
	return ops.ShareRule{Mode: ops.ShareModeLater}
}

func sameRule(a, b ops.ShareRule) bool {
	return a.Mode == b.Mode && slices.Equal(a.Names, b.Names)
}

// hiddenLocked reports whether what the Hub holds of a session must not be
// shown. history asks about the stored history and queued events (it also
// hides while the rule is not known), not about the live session list, which
// only ever holds what the Worker reported as shared.
func (h *EventHub) hiddenLocked(workerID, session string, history bool) bool {
	if _, ok := h.fenced[workerID][session]; ok {
		return true // a revisionless answer named it unshared
	}
	st := h.shares[workerID]
	if st != nil {
		for _, p := range st.prov {
			// A restriction that is requested and not settled hides the stored
			// history at once. The live session list keeps what the Worker
			// reported until the outcome is unknown: a restriction that is only
			// in flight, and then refused, must not have emptied the list.
			if (history || p.unknown) && !p.rule.Includes(session) {
				return true
			}
		}
	}
	if st == nil || !st.known {
		return history && h.shareStore != nil // strict: no rule, no history
	}
	if history && h.shareStore != nil {
		if h.sharesLoadErr != nil {
			return true
		}
		if !st.saved {
			h.retrySaveLocked(workerID, st)
			if !st.saved {
				return true // a restriction that is not durable hides the Worker
			}
		}
		if w := h.workers[workerID]; w != nil && w.online && st.gen != w.gen {
			return true // the connection has not declared its rule yet
		}
	}
	return !st.rule.Includes(session)
}

// retrySaveLocked writes the held rule to the database.
func (h *EventHub) retrySaveLocked(workerID string, st *shareState) {
	if h.shareStore == nil {
		st.saved = true
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), shareSaveTimeout)
	defer cancel()
	if err := h.shareStore.SaveWorkerShare(ctx, workerID, ops.ShareDecl{Rule: st.rule, Revision: st.rev}); err != nil {
		if st.saved {
			h.log.Error("workers: a share rule cannot be written; the history of the Worker stays hidden until it can", "worker_id", workerID, "error", err.Error())
		}
		st.saved = false
		return
	}
	st.saved = true
}

// adoptLocked applies a share rule that the Worker declared (workerID, on the
// connection gen), if it is current. rebase is true for the answer of a
// share_set that went through the Hub: the owner's own explicit change, which
// may replace a rule the Hub held after refusing a rollback.
func (h *EventHub) adoptLocked(workerID string, w *workerState, gen uint64, d ops.ShareDecl, rebase bool) (current bool) {
	if d.Revision == 0 {
		return false // not versioned: it cannot be ordered
	}
	d.Rule = canonRule(d.Rule)
	st := h.shareFor(workerID)
	accept := false
	switch {
	case !st.known, d.Revision > st.rev:
		accept = true
	case d.Revision == st.rev:
		if sameRule(d.Rule, st.rule) || d.Rule.Narrower(st.rule) {
			accept = true
		} else {
			// The same revision with a wider rule: the Worker's state does not
			// match what it told before. Keep the narrower rule.
			st.denied = true
			h.log.Warn("workers: a Worker declared a wider share rule under the same revision; the narrower rule is kept", "worker_id", workerID)
		}
	default: // a lower revision
		switch {
		case st.gen == gen && !st.denied:
			return false // an older answer of this connection: stale
		case st.gen == gen && st.denied && rebase:
			accept = true
		case st.gen != gen:
			// The first declaration of a connection is behind what the Hub
			// holds: the Worker's database was restored or replaced.
			if d.Rule.Narrower(st.rule) {
				accept = true
			} else {
				st.denied = true
				h.log.Warn("workers: a Worker declared a share rule older and wider than the one the Hub holds; the held rule stays until the owner sets one", "worker_id", workerID)
			}
		}
	}
	if accept {
		st.rule, st.rev, st.known, st.denied = d.Rule, d.Revision, true, false
		st.saved = false
		h.retrySaveLocked(workerID, st) // the rule is written before it is relied on
	}
	st.gen = gen
	// A restriction that is requested and not settled is never settled here: a
	// revision above the one it was dispatched at proves nothing about it (the
	// answer may be an older one, of another call). See [settleLocked].
	h.dropHiddenLocked(workerID, w)
	return st.known && st.rev == d.Revision && sameRule(st.rule, d.Rule)
}

// settleLocked ends the provisional restrictions of the Worker with a sequence
// number up to and including upTo, in memory and in the database. The caller
// holds the proof that none of them can still be applied (see
// [provisionalShare]) and that the rule it adopted is the rule in force. A
// failure to delete the row leaves it: after a restart the restriction is
// unknown again and is settled by a probe, which only costs availability.
func (h *EventHub) settleLocked(workerID string, upTo uint64) {
	st := h.shares[workerID]
	if st == nil {
		return
	}
	for seq := range st.prov {
		if seq <= upTo {
			delete(st.prov, seq)
		}
	}
	if h.shareStore != nil {
		ctx, cancel := context.WithTimeout(context.Background(), shareSaveTimeout)
		defer cancel()
		if err := h.shareStore.DeletePendingShares(ctx, workerID, upTo); err != nil {
			h.log.Warn("workers: a settled share restriction could not be removed from the database; it is settled again after a restart", "worker_id", workerID, "error", err.Error())
		}
	}
}

// settleOneLocked ends one provisional restriction.
func (h *EventHub) settleOneLocked(workerID string, seq uint64) {
	st := h.shares[workerID]
	if st == nil {
		return
	}
	delete(st.prov, seq)
	if h.shareStore != nil {
		ctx, cancel := context.WithTimeout(context.Background(), shareSaveTimeout)
		defer cancel()
		if err := h.shareStore.DeletePendingShare(ctx, workerID, seq); err != nil {
			h.log.Warn("workers: a settled share restriction could not be removed from the database; it is settled again after a restart", "worker_id", workerID, "error", err.Error())
		}
	}
}

// dropHiddenLocked removes from the Worker's live sessions the ones the rule
// hides, with a session_removed event each.
func (h *EventHub) dropHiddenLocked(workerID string, w *workerState) {
	if w == nil {
		return
	}
	var names []string
	for n := range w.sessions {
		if h.hiddenLocked(workerID, n, false) {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	for _, n := range names {
		delete(w.sessions, n)
		h.publishLocked(h.eventLocked(w, EventSessionRemoved, n))
	}
	if len(names) > 0 {
		w.updated = h.now()
	}
}

// ShareDeclOf returns the share rule and revision the Hub holds of a Worker. It
// is for the Hub's own checks and tests; no consumer of events or session lists
// gets it.
func (h *EventHub) ShareDeclOf(workerID string) (ops.ShareDecl, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.ensureSharesLocked()
	st := h.shares[workerID]
	if st == nil || !st.known {
		return ops.ShareDecl{}, false
	}
	return ops.ShareDecl{Rule: ops.ShareRule{Mode: st.rule.Mode, Names: slices.Clone(st.rule.Names)}, Revision: st.rev}, true
}

// ShareCall is the handle of one share_get or share_set that goes through the
// Hub, from before the request is dispatched to the end of the call. Exactly one
// of Refused, Unknown and Applied settles it. [EventHub.BeginShare] makes one.
type ShareCall interface {
	// Err is not nil when the restriction of a share_set could not be recorded
	// durably: the request must not be sent, and nothing else is to be called.
	Err() error
	// Seq is the sequence number the Hub sends the Worker with the share_set
	// (ops.ShareSetArgs.OpSeq); 0 for a share_get.
	Seq() uint64
	// Refused settles the call as refused before anything changed.
	Refused()
	// Unknown settles it as ended without a usable answer after the request may
	// have been delivered.
	Unknown()
	// Applied settles it with the answer of the Worker.
	Applied(res ops.ShareResult)
}

type shareCall struct {
	h      *EventHub
	worker string
	id     uint64 // 0: no restriction was registered (share_get)
	gen    uint64
	err    error
}

func (c *shareCall) Err() error  { return c.err }
func (c *shareCall) Seq() uint64 { return c.id }

// errShareNotRecorded is returned to the caller of a share_set whose restriction
// could not be recorded: nothing was sent to the Worker.
func errShareNotRecorded(cause error) error {
	_ = cause // the cause is logged where it happened; it is not shown to callers
	return proto.NewError(proto.CodeInternal, "the Hub could not record the change durably, so it did not send it; nothing changed")
}

// BeginShare is called before a share op is dispatched to a Worker. For a
// share_set, next is the rule it asks for: from this moment, until the call is
// settled, the sessions that rule would not share are hidden as well as the
// ones the held rule hides, so that a change which is made on the Worker but
// whose answer is lost never leaves the Hub showing more than the Worker
// does. The restriction is written to the database first (with a ShareStore):
// when that fails, the call carries the error ([ShareCall.Err]) and nothing is
// to be sent. The Hub remembers the connection the call is made on: an answer
// from another connection is not applied.
func (h *EventHub) BeginShare(workerID string, next *ops.ShareRule) ShareCall {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.ensureSharesLocked()
	c := &shareCall{h: h, worker: workerID}
	if w := h.workers[workerID]; w != nil {
		c.gen = w.gen
	}
	if next != nil {
		st := h.shareFor(workerID)
		p := &provisionalShare{rule: canonRule(*next), seq: h.nextSeqLocked(), baseRev: st.rev, baseGen: c.gen}
		if h.shareStore != nil {
			ctx, cancel := context.WithTimeout(context.Background(), shareSaveTimeout)
			defer cancel()
			if err := h.shareStore.SavePendingShare(ctx, store.PendingShare{Seq: p.seq, WorkerID: workerID, Rule: p.rule, BaseRevision: p.baseRev}); err != nil {
				h.log.Error("workers: a share restriction cannot be written; the change is not sent", "worker_id", workerID, "error", err.Error())
				c.err = errShareNotRecorded(err)
				return c
			}
		}
		c.id = p.seq
		st.prov[c.id] = p
	}
	return c
}

// Refused settles the call as refused before anything changed on the Worker (a
// refusal of the Worker's policy, an invalid argument, a Worker that was not
// reached, a change the Worker's barrier refused): the requested restriction is
// lifted.
func (c *shareCall) Refused() {
	h := c.h
	h.mu.Lock()
	defer h.mu.Unlock()
	if c.err == nil && c.id != 0 {
		h.settleOneLocked(c.worker, c.id)
	}
}

// Unknown settles the call as ended without a usable answer after the request
// may have been delivered (the connection was lost, the answer did not come in
// time or could not be read, the Worker failed in a way that does not say
// whether it changed anything). The requested restriction stays, also across a
// restart of the Hub, until evidence ordered after it settles it: see
// [EventHub.ConfirmShare] and [provisionalShare].
func (c *shareCall) Unknown() {
	h := c.h
	h.mu.Lock()
	defer h.mu.Unlock()
	if st := h.shares[c.worker]; st != nil && c.err == nil && c.id != 0 {
		if p := st.prov[c.id]; p != nil {
			p.unknown = true
			h.dropHiddenLocked(c.worker, h.workers[c.worker])
		}
	}
}

// Applied settles the call with the Worker's answer. A versioned answer is
// applied if it comes from the connection the call was made on and is not older
// than the rule the Hub holds; an answer without a revision (a Worker that
// predates revisions) only names unshared sessions and is applied as such.
//
// The answer of a share_set settles its own restriction. It also settles the
// older restrictions of unknown outcome when it carries the Worker's barrier
// and the barrier is not below its sequence number: the Worker then refuses
// every earlier change from now on, so the rule in this answer is the rule in
// force after all of them. That is the explicit, newer and successful change
// that may supersede an earlier uncertainty. Any other answer (a share_get, a
// delayed answer of an earlier share_set, one without the barrier) settles at
// most its own restriction, whatever its revision.
func (c *shareCall) Applied(res ops.ShareResult) {
	h := c.h
	h.mu.Lock()
	defer h.mu.Unlock()
	if c.err != nil {
		return
	}
	h.ensureSharesLocked()
	w := h.workers[c.worker]
	var cur uint64
	if w != nil {
		cur = w.gen
	}
	if cur != c.gen {
		// The connection was replaced while the call ran: this answer is from a
		// connection that is gone. What the Worker declares on the new one is
		// the current rule; until then the restriction stays.
		if st := h.shares[c.worker]; st != nil && c.id != 0 {
			if p := st.prov[c.id]; p != nil {
				p.unknown = true
				h.dropHiddenLocked(c.worker, w)
			}
		}
		return
	}
	current := false
	if res.RevisionNumber() == 0 {
		h.applyShareNamesLocked(c.worker, res.Unshared)
	} else {
		current = h.adoptLocked(c.worker, w, c.gen, ops.ShareDecl{Rule: res.Rule, Revision: res.RevisionNumber()}, c.id != 0)
	}
	if c.id != 0 {
		if current && res.BarrierNumber() >= c.id {
			h.settleLocked(c.worker, c.id)
		} else {
			h.settleOneLocked(c.worker, c.id)
		}
		h.dropHiddenLocked(c.worker, w)
	}
}

// ---- confirmation on the current connection ----------------------------------

// shareProber asks the Worker on one connection for its rule (the Hub's
// internal share probe); the Worker raises its barrier to barrier first.
type shareProber func(ctx context.Context, barrier uint64) (ops.ShareProbe, error)

// SetShareProbe gives the Link the way to ask its own connection's Worker for
// the share rule it has now. It is the metadata-only internal port that
// [EventHub.ConfirmShare] and [EventHub.HistoryVisible] use: it answers a rule,
// never a session or its content. A superseded Link changes nothing.
func (l *Link) SetShareProbe(p func(ctx context.Context, barrier uint64) (ops.ShareProbe, error)) {
	h := l.h
	h.mu.Lock()
	defer h.mu.Unlock()
	if w := h.workers[l.id]; w != nil && w.gen == l.gen {
		w.probe = p
	}
}

var errNoProbe = errors.New("workers: the Worker's connection cannot answer a share probe")

// ConfirmShare asks the Worker on its current connection for the rule it has
// now, raising its barrier past every restriction of unknown outcome, and
// settles what that proves. It returns an error when the Worker cannot be asked
// (offline, no probe, the connection was replaced meanwhile, no usable answer):
// then nothing is settled and the restrictions stay.
func (h *EventHub) ConfirmShare(ctx context.Context, workerID string) error {
	h.mu.Lock()
	h.ensureSharesLocked()
	w := h.workers[workerID]
	if w == nil || !w.online || w.probe == nil {
		h.mu.Unlock()
		return errNoProbe
	}
	gen, probe := w.gen, w.probe
	var upTo uint64
	if st := h.shares[workerID]; st != nil {
		for seq, p := range st.prov {
			if p.unknown && seq > upTo {
				upTo = seq
			}
		}
	}
	h.mu.Unlock()

	res, err := probe(ctx, upTo)
	if err != nil {
		return err
	}
	rev, rerr := strconv.ParseUint(res.Revision, 10, 64)
	barrier, _ := strconv.ParseUint(res.OpBarrier, 10, 64)
	if rerr != nil || rev == 0 {
		return errors.New("workers: the Worker's share probe has no revision")
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	w = h.workers[workerID]
	if w == nil || !w.online || w.gen != gen {
		return ErrSuperseded // the connection that answered is not the current one
	}
	current := h.adoptLocked(workerID, w, gen, ops.ShareDecl{Rule: res.Rule, Revision: rev}, false)
	if upTo != 0 && current && barrier >= upTo {
		h.settleLocked(workerID, upTo)
	}
	h.dropHiddenLocked(workerID, w)
	return nil
}

// HistoryVisible reports whether what the Hub stored of a session may be shown
// now. For a Worker that is not connected it is the Hub's durable knowledge: the
// rule it last declared, with every restriction not yet settled (a known rule
// that shares the session and no pending revocation keeps the history readable
// offline). For a connected Worker the Hub does not rely on what it holds: it
// asks the Worker on its current connection for the rule it has now, so a change
// made on the machine, whose event has not arrived yet, is honoured before the
// history is served. When that cannot be done (the probe fails, the connection
// was replaced meanwhile) the answer is false: there is no fallback to what the
// Hub stored. It answers about history only, never about the live session list,
// and no caller gets the rule.
func (h *EventHub) HistoryVisible(ctx context.Context, workerID, session string) bool {
	h.mu.Lock()
	h.ensureSharesLocked()
	w := h.workers[workerID]
	connected := w != nil && w.online
	hasProbe := connected && w.probe != nil
	var gen uint64
	if w != nil {
		gen = w.gen
	}
	h.mu.Unlock()
	if hasProbe {
		if err := h.ConfirmShare(ctx, workerID); err != nil {
			return false
		}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if hasProbe {
		// The connection that answered must still be the one that is current.
		if w2 := h.workers[workerID]; w2 == nil || !w2.online || w2.gen != gen || w2.probe == nil {
			return false
		}
	}
	return !h.hiddenLocked(workerID, session, true)
}

// String is for logs.
func (p *provisionalShare) String() string {
	return fmt.Sprintf("restriction %d (%s)", p.seq, p.rule.Mode)
}
