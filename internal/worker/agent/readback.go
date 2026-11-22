package agent

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/worker/presence"
)

// ReadbackSink is what the op server calls for the Hub's private read-back
// control frame (proto.TypeReadbackApply). It is not an op: no executor, local
// socket, MCP tool, REST route, Worker call or master call reaches it. Only
// the op server hands it a frame, and only one that arrived on a Hub-opened op
// stream of an authenticated link.
type ReadbackSink interface {
	// LinkAdmitted is called when a Hub link starts being served, before any
	// of its frames. A document published under another owner, Worker or Hub
	// key than the link's is removed then.
	LinkAdmitted(l *Link)
	// WelcomeStored is called when an authenticated welcome has been stored,
	// before the usage registration (A72) decides whether the link is served.
	// A document bound to another owner, Worker or Hub key is removed then, so
	// that a reassignment is effective even while the registration is pending
	// and the link is closed before any handler starts.
	WelcomeStored(workerID, ownerID string, hubPub ed25519.PublicKey, pending bool)
	// Apply publishes the document of f if every binding of f matches l, and
	// acknowledges the revision.
	Apply(l *Link, f proto.ReadbackApply) (proto.ReadbackAck, *proto.Error)
}

type linkKey struct{}

// withLink makes the link available to the op server's stream handlers.
func withLink(ctx context.Context, l *Link) context.Context {
	return context.WithValue(ctx, linkKey{}, l)
}

func linkFrom(ctx context.Context) *Link {
	l, _ := ctx.Value(linkKey{}).(*Link)
	return l
}

// readbackBinding is what a published document is bound to.
type readbackBinding struct {
	worker, owner, hubKey string
}

func bindingOfLink(l *Link) (readbackBinding, bool) {
	return bindingOf(l.Welcome.WorkerID, l.Welcome.OwnerID, l.pin.Pub)
}

func bindingOf(workerID, ownerID string, hubPub ed25519.PublicKey) (readbackBinding, bool) {
	fp, err := identity.FingerprintOf(hubPub)
	if err != nil {
		return readbackBinding{}, false
	}
	return readbackBinding{worker: workerID, owner: ownerID, hubKey: fp.String()}, true
}

// usageClearTombstone is the file in the state directory that records a usage
// document that could not be removed (on Windows a reader holding the file
// open can make the unlink fail). It is retried when the publisher attaches,
// that is on the next start, and dropped once the file is gone or replaced by
// a document that was published under a valid fence.
const usageClearTombstone = "usage-clear.pending"

// Bounded backoff of a failed unlink.
var usageUnlinkBackoff = []time.Duration{10 * time.Millisecond, 20 * time.Millisecond, 40 * time.Millisecond, 80 * time.Millisecond, 160 * time.Millisecond}

// guardedUsageSetter is the optional publisher API that runs ok after the
// document is staged and under the publisher's own lock, immediately before
// the rename, and does not rename when it reports false.
type guardedUsageSetter interface {
	SetUsageIf(raw []byte, ok func() bool) error
}

// enrollmentGuard runs fn while the enrollment of generation gen is current,
// with the enrollment lock held, after live reported true.
type enrollmentGuard func(ctx context.Context, gen uint64, live func() bool, fn func() error) error

// WithEnrollmentIfLive runs fn with the enrollment locks held, only while the
// enrollment is still generation gen and live reports true. A wipe cannot run
// meanwhile, and one that ran before is seen as [ErrEnrollmentChanged].
func (e *Enrollments) WithEnrollmentIfLive(ctx context.Context, gen uint64, live func() bool, fn func() error) error {
	return e.lockedIf(ctx, gen, func() error {
		if live != nil && !live() {
			return errGuardExpired
		}
		return fn()
	})
}

// ReadbackReceiver publishes the usage document the Hub hands the Worker. Every
// decision is made under one lock, immediately before the local replacement:
// the link must be the newest admitted one and still alive, approved and inside
// its certificate; the enrollment must not have moved since the link started;
// the frame's Worker, owner and Hub key must be the link's; the revision must
// be newer than the last one applied on this link.
type ReadbackReceiver struct {
	log   *slog.Logger
	moved func(gen uint64) bool
	guard enrollmentGuard
	// tombDir is where the unlink tombstone lives ("" none); tmu serializes
	// its file operations.
	tombDir string
	tmu     sync.Mutex
	sleep   func(time.Duration)

	// epoch counts the clears. Clear runs from the enrollment wipe, which
	// holds the enrollment lock that Apply takes (through moved) while it
	// holds mu, so Clear takes no lock of the receiver: it advances epoch and
	// clears the publisher, and the state under mu is read as empty once its
	// own epoch is behind.
	epoch atomic.Uint64
	pubP  atomic.Pointer[presence.Publisher]
	// noPub is set when the presence publisher failed to start for good: the
	// Worker then neither announces the feature nor accepts a frame.
	noPub atomic.Bool

	mu     sync.Mutex
	stEp   uint64 // the epoch bound and rev belong to
	latest *Link
	bound  *readbackBinding // binding of the document currently published
	rev    uint64           // last revision applied on latest
	sum    [32]byte         // digest of the document of rev
	have   bool
}

// NewReadbackReceiver returns a receiver; moved reports whether the enrollment
// generation gen is no longer current (nil: never).
func NewReadbackReceiver(log *slog.Logger, moved func(gen uint64) bool) *ReadbackReceiver {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &ReadbackReceiver{log: log, moved: moved, sleep: time.Sleep}
}

// SetGuard gives the receiver the enrollment guard its commit runs under.
func (r *ReadbackReceiver) SetGuard(g func(ctx context.Context, gen uint64, live func() bool, fn func() error) error) {
	r.guard = g
}

// SetTombstoneDir sets the state directory in which a failed removal is
// recorded for retry on the next start.
func (r *ReadbackReceiver) SetTombstoneDir(dir string) { r.tombDir = dir }

var _ ReadbackSink = (*ReadbackReceiver)(nil)

// SetPublisher gives the receiver the local publisher (nil: none).
func (r *ReadbackReceiver) SetPublisher(p *presence.Publisher) {
	// Under the receiver's lock, and before the publisher is exposed: a frame
	// cannot write a document that the old removal then deletes.
	r.mu.Lock()
	defer r.mu.Unlock()
	if p != nil && r.tombstoned() {
		// A removal that failed (or had no publisher) earlier is done now.
		_ = r.clearVia(p)
	}
	r.pubP.Store(p)
}

func (r *ReadbackReceiver) tombPath() string {
	if r.tombDir == "" {
		return ""
	}
	return filepath.Join(r.tombDir, usageClearTombstone)
}

func (r *ReadbackReceiver) tombstoned() bool {
	p := r.tombPath()
	if p == "" {
		return false
	}
	r.tmu.Lock()
	defer r.tmu.Unlock()
	_, err := os.Lstat(p)
	return err == nil
}

func (r *ReadbackReceiver) setTombstone(on bool) {
	p := r.tombPath()
	if p == "" {
		return
	}
	r.tmu.Lock()
	defer r.tmu.Unlock()
	if on {
		if err := identity.WritePrivate(p, []byte("1\n")); err != nil {
			r.log.Warn("the pending removal of the usage document could not be recorded", "error", err)
		}
		return
	}
	_ = os.Remove(p)
}

// SetUnavailable records that no publisher will be attached (the presence
// record could not be published); the feature is withdrawn.
func (r *ReadbackReceiver) SetUnavailable() { r.noPub.Store(true) }

// Ready reports whether the Worker can publish a document, which is what the
// feature announcement stands for. The connector can dial before the presence
// module (which starts last) has attached its publisher, so a Worker is ready
// until it is known that it cannot publish; a frame that arrives in that gap is
// answered "busy" and the Hub sends it again.
func (r *ReadbackReceiver) Ready() bool {
	return r.pubP.Load() != nil || !r.noPub.Load()
}

// Clear removes the published document from memory and disk and forgets its
// binding: a revoke, a reset, a new identity or a change of owner.
func (r *ReadbackReceiver) Clear() {
	r.epoch.Add(1)
	_ = r.clearPublished()
}

// clearPublished removes the document, retrying a failed unlink with a bounded
// backoff. A removal that still fails (or has no publisher to run on) is
// recorded as a tombstone and retried when the publisher next attaches.
func (r *ReadbackReceiver) clearPublished() error { return r.clearVia(r.pubP.Load()) }

func (r *ReadbackReceiver) clearVia(p *presence.Publisher) error {
	if p == nil {
		r.setTombstone(true)
		return nil
	}
	var err error
	for i := 0; ; i++ {
		if err = p.ClearUsage(); err == nil {
			r.setTombstone(false)
			return nil
		}
		if i >= len(usageUnlinkBackoff) {
			break
		}
		r.sleep(usageUnlinkBackoff[i])
	}
	r.log.Warn("the stored usage document could not be removed; the removal is retried on the next start", "error", err)
	r.setTombstone(true)
	return err
}

// syncLocked forgets the state a Clear made obsolete.
func (r *ReadbackReceiver) syncLocked() {
	if e := r.epoch.Load(); e != r.stEp {
		r.stEp, r.bound, r.rev, r.have, r.sum = e, nil, 0, false, [32]byte{}
	}
}

// clearLocked clears on behalf of the receiver itself (owner change).
func (r *ReadbackReceiver) clearLocked() error {
	r.epoch.Add(1)
	r.syncLocked()
	return r.clearPublished()
}

// LinkAdmitted implements [ReadbackSink].
func (r *ReadbackReceiver) LinkAdmitted(l *Link) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.syncLocked()
	r.latest = l
	r.rev, r.have, r.sum = 0, false, [32]byte{}
	b, ok := bindingOfLink(l)
	if r.bound != nil && (!ok || *r.bound != b) {
		_ = r.clearLocked()
	}
}

// WelcomeStored implements [ReadbackSink].
func (r *ReadbackReceiver) WelcomeStored(workerID, ownerID string, hubPub ed25519.PublicKey, pending bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.syncLocked()
	b, ok := bindingOf(workerID, ownerID, hubPub)
	if r.bound != nil && (pending || !ok || *r.bound != b) {
		_ = r.clearLocked()
		r.rev, r.have, r.sum = 0, false, [32]byte{}
	}
}

func refuse(code proto.Code, reason, msg string) *proto.Error {
	return proto.ReadbackRefusal(code, reason, msg)
}

// Apply implements [ReadbackSink].
func (r *ReadbackReceiver) Apply(l *Link, f proto.ReadbackApply) (proto.ReadbackAck, *proto.Error) {
	if err := f.Check(); err != nil {
		return proto.ReadbackAck{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	pub := r.pubP.Load()
	if pub == nil {
		if r.noPub.Load() {
			return proto.ReadbackAck{}, proto.NewError(proto.CodeUnsupportedOp, "the Worker cannot publish a usage document")
		}
		return proto.ReadbackAck{}, proto.NewError(proto.CodeBusy, "the Worker is still starting; try again")
	}
	r.syncLocked()
	fenced := func(why string) (proto.ReadbackAck, *proto.Error) {
		return proto.ReadbackAck{}, refuse(proto.CodeConflict, proto.ReadbackFenced, why)
	}
	switch {
	case l == nil || l != r.latest:
		return fenced("the frame is not for the current Hub connection")
	case !linkLive(l) || l.Expired():
		return fenced("the Hub connection has ended")
	case l.Welcome.Pending:
		return fenced("the Worker is not approved")
	case r.moved != nil && r.moved(l.gen):
		return fenced("the enrollment changed")
	}
	b, ok := bindingOfLink(l)
	if !ok || l.Welcome.OwnerID == "" {
		return fenced("the connection has no owner binding")
	}
	if f.WorkerID != b.worker || f.OwnerID != b.owner || f.HubKey != b.hubKey {
		return fenced("the document was built for another owner, Worker or Hub key")
	}
	if r.bound != nil && *r.bound != b {
		_ = r.clearLocked() // defensive: LinkAdmitted does this already
	}
	sum := sha256.Sum256(f.Doc)
	if r.have && f.Revision == r.rev && sum == r.sum {
		// A repeat of what is published (the acknowledgement was lost).
		return proto.ReadbackAck{Revision: f.Revision}, nil
	}
	if r.have && f.Revision <= r.rev {
		return proto.ReadbackAck{}, refuse(proto.CodeConflict, proto.ReadbackStale, "the revision is not newer than the one applied")
	}
	if f.Clear {
		// The Hub withdrew the document (the recipient is not entitled to it
		// any more). Removal needs no fence: it only ever takes access away.
		if err := r.clearLocked(); err != nil {
			return proto.ReadbackAck{}, proto.NewError(proto.CodeInternal, "the usage document could not be removed")
		}
		r.rev, r.sum, r.have = f.Revision, sum, true
		return proto.ReadbackAck{Revision: f.Revision}, nil
	}
	// The fence, as one predicate: asked right before the document becomes
	// visible (inside the enrollment lock, and in the publisher right before
	// the rename), and again after the write.
	fence := func() bool {
		return l == r.latest && linkLive(l) && !l.Expired() && !l.Welcome.Pending &&
			(r.moved == nil || !r.moved(l.gen))
	}
	if r.guard != nil {
		// The enrollment lock is held by the guard: the moved check above would
		// take it again, and it is implied by the guard's own generation check.
		fence = func() bool {
			return l == r.latest && linkLive(l) && !l.Expired() && !l.Welcome.Pending
		}
	}
	// Verbatim: the receiver keeps the bytes the Hub signed the link for.
	ep := r.epoch.Load()
	doc := bytes.Clone(f.Doc)
	var ended bool
	write := func() error {
		if !fence() {
			ended = true
			return errReadbackFenceEnded
		}
		if g, ok := any(pub).(guardedUsageSetter); ok {
			return g.SetUsageIf(doc, func() bool {
				if !fence() {
					ended = true
					return false
				}
				return true
			})
		}
		return pub.SetUsage(doc)
	}
	var err error
	if r.guard != nil {
		err = r.guard(l.Context(), l.gen, fence, write)
	} else {
		err = write()
	}
	switch {
	case ended, errors.Is(err, ErrEnrollmentChanged), errors.Is(err, errGuardExpired), errors.Is(err, errReadbackFenceEnded),
		err != nil && !linkLive(l):
		return fenced("the enrollment or the connection changed before the document was published")
	case err != nil:
		r.log.Warn("the usage document was not published", "error", err)
		return proto.ReadbackAck{}, proto.NewError(proto.CodeInternal, "the usage document could not be published")
	}
	// A wipe that ran while the document was being written advanced the
	// enrollment generation before it cleared; look again after the write, so
	// that a document written after that clear is removed here. A certificate
	// that ended during the write ends the document with it.
	if r.epoch.Load() != ep || l.Expired() || !linkLive(l) || (r.guard == nil && r.moved != nil && r.moved(l.gen)) {
		_ = r.clearLocked()
		return fenced("the enrollment or the connection changed while the document was published")
	}
	r.setTombstone(false) // a new document under a valid fence replaces a pending removal
	r.bound = &b
	r.rev, r.sum, r.have = f.Revision, sum, true
	return proto.ReadbackAck{Revision: f.Revision}, nil
}

// linkLive reports whether the connection is still up: its context is alive
// and its transport session is not closed (the connector cancels the context
// only after it has seen the session end).
func linkLive(l *Link) bool {
	if l.Context().Err() != nil {
		return false
	}
	if l.Session != nil {
		select {
		case <-l.Session.Done():
			return false
		default:
		}
	}
	return true
}

var errReadbackFenceEnded = errors.New("agent: the fence ended before the usage document was published")
