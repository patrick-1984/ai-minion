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
	doc interface {
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
	var (
		rev      uint64
		sentHash string // content hash of the revision rev
		sentRaw  []byte
		acked    bool
		ackedAt  time.Time
		retry    time.Duration
	)
	for ctx.Err() == nil {
		wait := d.reconcile
		wakeCh := d.wake()
		switch err := d.step(ctx, w, hubFP.String(), &rev, &sentHash, &sentRaw, &acked, &ackedAt); {
		case err == nil:
			retry = 0
		case errors.Is(err, workers.ErrReadbackFenced), errors.Is(err, store.ErrWorkerNotFound), errors.Is(err, errReadbackStale):
			// The connection is no longer the Worker's current one (a new
			// owner, a reconnect, a removal): its replacement has its own loop.
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
				acked = false
				rev++ // continue above whatever the Worker holds
			}
			retry = min(d.retryMax, max(d.retryMin, retry*2))
			wait = retry + time.Duration(rand.Int64N(int64(retry/4)+1))
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

// errReadbackStale: the owner of the connection is not the owner the store
// names now.
var errReadbackStale = errors.New("the connection's owner is not the Worker's owner any more")

// step runs one evaluation: recheck, build, and send when the content changed,
// the last send was not acknowledged or the refresh interval passed.
func (d *readbackDelivery) step(ctx context.Context, w workers.Online, hubKey string,
	rev *uint64, sentHash *string, sentRaw *[]byte, acked *bool, ackedAt *time.Time) error {
	select {
	case d.slots <- struct{}{}:
		defer func() { <-d.slots }()
	case <-ctx.Done():
		return ctx.Err()
	}
	// The dispatch recheck: the store's owner, read now, must be the owner of
	// this connection. The registry checks the connection itself when it opens
	// the stream.
	owner, err := d.dir.WorkerOwner(ctx, w.WorkerID)
	if err != nil {
		return err
	}
	if owner != w.OwnerID {
		return errReadbackStale
	}
	raw, hash, err := d.doc.documentBytes(ctx, owner)
	if err != nil {
		return err
	}
	// The document took time to build: read the owner once more right before
	// the frame is written, so that a reassignment that committed meanwhile
	// sends nothing built for the previous owner.
	if now, err := d.dir.WorkerOwner(ctx, w.WorkerID); err != nil {
		return err
	} else if now != owner {
		return errReadbackStale
	}
	now := d.now()
	switch {
	case *acked && hash == *sentHash && now.Sub(*ackedAt) < d.refresh:
		return nil
	case *sentRaw != nil && !*acked && hash == *sentHash:
		// The same content was sent and not acknowledged: send the same
		// revision with the same bytes again; the Worker takes a repeat.
	default:
		*rev++
		*sentHash, *sentRaw = hash, raw
		*acked = false
	}
	ack, err := d.send.SendReadback(ctx, w, proto.ReadbackApply{
		Type: proto.TypeReadbackApply, Revision: *rev, OwnerID: owner, WorkerID: w.WorkerID, HubKey: hubKey, Doc: *sentRaw,
	})
	if err != nil {
		return err
	}
	*acked, *ackedAt = ack.Revision == *rev, d.now()
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
	return d.invalidate, nil
}
