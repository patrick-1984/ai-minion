package server

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/hub/workers"
	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// Timing of the read-back delivery. It does not share a slot, a timer or a
// failure state with the usage collector: a saturated or failing collector
// does not delay a document.
const (
	// readbackReconcile is the period of the reconciliation: the document is
	// rebuilt and compared with the last acknowledged content.
	readbackReconcile = 60 * time.Second
	// readbackRefresh is how long an unchanged document may go unsent. It is
	// well inside the document's own stale_after_s, so a connected Worker's
	// copy is always fresh evidence.
	readbackRefresh = 5 * time.Minute
	// readbackRetryMin and readbackRetryMax bound the retry after a failed or
	// unacknowledged delivery.
	readbackRetryMin = 2 * time.Second
	readbackRetryMax = 60 * time.Second
	// readbackMaxConcurrent bounds the deliveries in flight.
	readbackMaxConcurrent = 16
)

// readbackSender is the Hub's way to the Worker: the registry sends the frame
// on the exact connection. Tests replace it.
type readbackSender interface {
	SendReadback(ctx context.Context, w workers.Online, f proto.ReadbackApply) (proto.ReadbackAck, error)
}

// readbackDirectory answers the one question the dispatch recheck needs.
type readbackDirectory interface {
	WorkerOwner(ctx context.Context, workerID string) (string, error)
}

// readbackRecipients answers whether a Worker may hold usage data at all:
// its owner is active with an effective role and the Worker is approved. The
// store implements it (Store.ReadbackRecipientEligible); a directory without it
// is treated as always eligible.
type readbackRecipients interface {
	ReadbackRecipientEligible(ctx context.Context, workerID string) (ownerID string, ok bool, err error)
}

// readbackDelivery pushes the nominated usage document to every approved
// Worker that announced usage_readback_v1, over the private control frame.
//
// A document is bound to the connection (handshake transcript), the Worker
// id, the pinned Hub key, the stable Hub owner id and a revision, and the
// owner is read again from the store right before the frame is written: a
// reassignment, a revocation or a reconnect between building and sending
// turns the delivery into nothing.
type readbackDelivery struct {
	dir readbackDirectory
	// elig is the recipient eligibility recheck (nil: always eligible).
	elig readbackRecipients
	doc  interface {
		documentBytes(ctx context.Context, owner string) ([]byte, string, error)
	}
	send readbackSender
	log  *slog.Logger
	now  func() time.Time

	reconcile, refresh, retryMin, retryMax time.Duration
	slots                                  chan struct{}

	mu   sync.Mutex
	kick chan struct{} // closed (and replaced) by invalidate
}

func newReadbackDelivery(dir readbackDirectory, rb *usageReadback, send readbackSender, log *slog.Logger) *readbackDelivery {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &readbackDelivery{
		dir: dir, doc: rb, send: send, log: log, now: time.Now,
		reconcile: readbackReconcile, refresh: readbackRefresh, retryMin: readbackRetryMin, retryMax: readbackRetryMax,
		slots: make(chan struct{}, readbackMaxConcurrent), kick: make(chan struct{}),
	}
}

// documentBytes returns the encoded document for owner and its content hash.
func (u *usageReadback) documentBytes(ctx context.Context, owner string) ([]byte, string, error) {
	doc, hash, err := u.Document(ctx, owner)
	if err != nil {
		return nil, "", err
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		return nil, "", err
	}
	if len(raw) > proto.MaxReadbackDocBytes {
		return nil, "", errors.New("the usage document is larger than a read-back frame allows")
	}
	return raw, hash, nil
}

// invalidate wakes every delivery loop at once; wakes that arrive together
// coalesce into one rebuild per Worker.
func (d *readbackDelivery) invalidate() {
	d.mu.Lock()
	close(d.kick)
	d.kick = make(chan struct{})
	d.mu.Unlock()
}

// generation is the publication-authority revision of the document source (0
// for a source without one).
func (d *readbackDelivery) generation() uint64 {
	if g, ok := d.doc.(interface{ generation() uint64 }); ok {
		return g.generation()
	}
	return 0
}

func (d *readbackDelivery) wake() <-chan struct{} {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.kick
}

// readbackEligible reports whether the Hub may send to this connection at all:
// approved, and announcing the feature in its signed hello.
//
// The usage acceptance is not decided here. The hello's usage_consent fact is
// display-only and stale on a first connection (the Worker computes it before
// the welcome stored its Worker id), and the A72 admission is the Worker's own:
// a Worker whose enrollment requires the usage acceptance and has not got it
// closes the link before it starts any handler (so it runs no receiver and
// gets presence only), while an approved legacy link without acceptance
// (legacy_pending) stays connected and receives the read-back as before.
func readbackEligible(w workers.Online) bool {
	return !w.Pending && w.HasFeature(proto.FeatureUsageReadbackV1)
}

// serve delivers to one connection until it ends or stops being the Worker's
// current one.
func (d *readbackDelivery) serve(ctx context.Context, w workers.Online, sess workers.OpSession) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		select {
		case <-sess.Done():
			cancel()
		case <-ctx.Done():
		}
	}()
	hubFP, err := identity.FingerprintOf(w.HubRoot)
	if err != nil {
		d.log.Warn("usage read-back: the connection has no Hub key to bind to", "worker_id", w.WorkerID)
		return
	}
	var st deliveryState
	var retry time.Duration
	for ctx.Err() == nil {
		wait := d.reconcile
		wakeCh := d.wake()
		switch err := d.step(ctx, w, hubFP.String(), &st); {
		case err == nil:
			retry = 0
		case errors.Is(err, errReadbackRetryWait):
			// An unrelated edit woke the loop while a failed revision waits
			// for its retry deadline: the deadline stands.
			wait = max(st.retryAt.Sub(d.now()), time.Millisecond)
		case errors.Is(err, errReadbackStale):
			// The store names another owner than the one this connection was
			// welcomed for (a reassignment that committed while it registered):
			// end this exact connection, the reconnect is welcomed with the
			// new owner and gets its own loop.
			d.log.Info("usage read-back: the connection's owner changed; the connection is ended", "worker_id", w.WorkerID)
			_ = sess.Close()
			return
		case errors.Is(err, workers.ErrReadbackFenced), errors.Is(err, store.ErrWorkerNotFound):
			// The connection is no longer the Worker's current one (a
			// reconnect, a removal): its replacement has its own loop.
			return
		case proto.CodeOf(err) == proto.CodeUnsupportedOp:
			// The Worker announced the feature but cannot publish (its
			// presence record is unavailable): nothing to retry on this link.
			d.log.Info("usage read-back: the Worker cannot publish a usage document", "worker_id", w.WorkerID)
			return
		case proto.ReadbackReason(err) == proto.ReadbackFenced:
			// The Worker refused the binding: do not hammer it; the next
			// reconciliation or invalidation looks again.
			d.log.Warn("usage read-back: the Worker refused the document's binding", "worker_id", w.WorkerID)
			retry = 0
		default:
			if proto.ReadbackReason(err) == proto.ReadbackStale {
				st.acked = false
				st.rev++ // continue above whatever the Worker holds
			}
			retry = min(d.retryMax, max(d.retryMin, retry*2))
			wait = retry + time.Duration(rand.Int64N(int64(retry/4)+1))
			st.retryAt = d.now().Add(wait)
			d.log.Debug("usage read-back: delivery failed; it will be retried", "worker_id", w.WorkerID, "error", err.Error())
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-wakeCh:
		case <-timer.C:
		}
		timer.Stop()
	}
}

// deliveryState is what one connection's loop remembers.
type deliveryState struct {
	rev      uint64
	sentHash string // content hash of the revision rev
	sentRaw  []byte
	acked    bool
	ackedAt  time.Time
	// retryAt is the earliest time the unacknowledged revision is sent again.
	retryAt time.Time
	// cleared: the last frame acknowledged was a withdrawal.
	cleared bool
}

// errReadbackStale: the owner of the connection is not the owner the store
// names now.
var errReadbackStale = errors.New("the connection's owner is not the Worker's owner any more")

// errReadbackRetryWait: the failed revision's retry deadline has not passed.
var errReadbackRetryWait = errors.New("the retry of the unacknowledged revision is not due")

// recheck reads the store's owner and the recipient's eligibility.
func (d *readbackDelivery) recheck(ctx context.Context, w workers.Online) (owner string, eligible bool, err error) {
	owner, err = d.dir.WorkerOwner(ctx, w.WorkerID)
	if err != nil {
		return "", false, err
	}
	if owner != w.OwnerID {
		return "", false, errReadbackStale
	}
	if d.elig == nil {
		return owner, true, nil
	}
	cur, eligible, err := d.elig.ReadbackRecipientEligible(ctx, w.WorkerID)
	if err == nil && cur != owner {
		return "", false, errReadbackStale
	}
	return owner, eligible, err
}

// withdraw tells the Worker to remove what it publishes (once): its recipient
// lost the entitlement to the data.
func (d *readbackDelivery) withdraw(ctx context.Context, w workers.Online, hubKey, owner string, st *deliveryState) error {
	if st.cleared {
		return nil
	}
	if d.now().Before(st.retryAt) {
		return errReadbackRetryWait
	}
	st.rev++
	st.sentHash, st.sentRaw, st.acked = "", nil, false
	ack, err := d.send.SendReadback(ctx, w, proto.ReadbackApply{
		Type: proto.TypeReadbackApply, Revision: st.rev, OwnerID: owner, WorkerID: w.WorkerID, HubKey: hubKey, Clear: true,
	})
	if err != nil {
		if proto.ReadbackReason(err) == proto.ReadbackStale {
			st.rev++
		}
		return err
	}
	st.cleared, st.retryAt = ack.Revision == st.rev, time.Time{}
	return nil
}

// step runs one evaluation: recheck, build, and send when the content changed,
// the last send was not acknowledged and due, or the refresh interval passed.
func (d *readbackDelivery) step(ctx context.Context, w workers.Online, hubKey string, st *deliveryState) error {
	select {
	case d.slots <- struct{}{}:
		defer func() { <-d.slots }()
	case <-ctx.Done():
		return ctx.Err()
	}
	// The dispatch recheck: the store's owner, read now, must be the owner of
	// this connection, and the recipient (owner active, Worker approved) must
	// still be entitled to usage data. The registry checks the connection
	// itself when it opens the stream.
	owner, eligible, err := d.recheck(ctx, w)
	if err != nil {
		return err
	}
	if !eligible {
		return d.withdraw(ctx, w, hubKey, owner, st)
	}
	gen := d.generation()
	raw, hash, err := d.doc.documentBytes(ctx, owner)
	if err != nil {
		return err
	}
	// The document took time to build: read the owner and the eligibility once
	// more right before the frame is written, so that a reassignment or a
	// deactivation that committed meanwhile sends nothing built for the
	// previous state.
	now, eligible, err := d.recheck(ctx, w)
	if err != nil {
		return err
	}
	if now != owner {
		return errReadbackStale
	}
	if !eligible {
		return d.withdraw(ctx, w, hubKey, owner, st)
	}
	if d.generation() != gen {
		// The publication authority changed while the document was built: it
		// is not sent. The change's invalidation wakes the loop to build again.
		return nil
	}
	st.cleared = false
	at := d.now()
	switch {
	case st.acked && hash == st.sentHash && at.Sub(st.ackedAt) < d.refresh:
		return nil
	case st.sentRaw != nil && !st.acked && hash == st.sentHash:
		// The same content was sent and not acknowledged: send the same
		// revision with the same bytes again, but not before its retry
		// deadline: an unrelated edit must not turn the backoff into a hot
		// loop. The Worker takes a repeat.
		if at.Before(st.retryAt) {
			return errReadbackRetryWait
		}
	default:
		st.rev++
		st.sentHash, st.sentRaw = hash, raw
		st.acked = false
	}
	ack, err := d.send.SendReadback(ctx, w, proto.ReadbackApply{
		Type: proto.TypeReadbackApply, Revision: st.rev, OwnerID: owner, WorkerID: w.WorkerID, HubKey: hubKey, Doc: st.sentRaw,
	})
	if errors.Is(err, workers.ErrReadbackIneligible) {
		// The recipient lost its entitlement while the stream was opening: the
		// document was not written; the Worker is told to clear instead.
		st.sentHash, st.sentRaw, st.acked = "", nil, false
		return d.withdraw(ctx, w, hubKey, owner, st)
	}
	if err != nil {
		return err
	}
	st.acked, st.ackedAt, st.retryAt = ack.Revision == st.rev, d.now(), time.Time{}
	return nil
}

// registerReadbackDelivery starts the delivery for every approved Worker that
// connects and returns the invalidation the usage operations call.
func registerReadbackDelivery(ctx context.Context, env *Env, st *store.Store, rb *usageReadback) (func(), error) {
	svc, err := Require[*workers.Service](env)
	if err != nil {
		return nil, err
	}
	d := newReadbackDelivery(st, rb, svc.Registry(), nil)
	// The last check before a document is written, after its stream is open.
	svc.Registry().SetReadbackGuard(func(ctx context.Context, w workers.Online) error {
		_, eligible, err := d.recheck(ctx, w)
		switch {
		case errors.Is(err, errReadbackStale):
			return workers.ErrReadbackFenced
		case err != nil:
			return err
		case !eligible:
			return workers.ErrReadbackIneligible
		}
		return nil
	})
	// The recipient eligibility recheck is the store's (slice A); a store
	// without it leaves the delivery as before.
	if e, ok := any(st).(readbackRecipients); ok {
		d.elig = e
	}
	run, cancel := context.WithCancel(ctx)
	var mu sync.Mutex
	var group sync.WaitGroup
	svc.Registry().OnAdd(func(w workers.Online, sess workers.OpSession) {
		mu.Lock()
		defer mu.Unlock()
		if run.Err() != nil || !readbackEligible(w) {
			return
		}
		group.Add(1)
		go func() {
			defer group.Done()
			d.serve(run, w, sess)
		}()
	})
	env.OnStop(func(context.Context) error { mu.Lock(); cancel(); mu.Unlock(); group.Wait(); return nil })
	return func() { rb.bump(); d.invalidate() }, nil
}
