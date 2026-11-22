package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"log/slog"
	"sync"
	"sync/atomic"

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
	fp, err := identity.FingerprintOf(l.pin.Pub)
	if err != nil {
		return readbackBinding{}, false
	}
	return readbackBinding{worker: l.Welcome.WorkerID, owner: l.Welcome.OwnerID, hubKey: fp.String()}, true
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
	return &ReadbackReceiver{log: log, moved: moved}
}

var _ ReadbackSink = (*ReadbackReceiver)(nil)

// SetPublisher gives the receiver the local publisher (nil: none).
func (r *ReadbackReceiver) SetPublisher(p *presence.Publisher) {
	r.pubP.Store(p)
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
	r.clearPublished()
}

func (r *ReadbackReceiver) clearPublished() {
	if p := r.pubP.Load(); p != nil {
		if err := p.ClearUsage(); err != nil {
			r.log.Warn("the stored usage document could not be removed", "error", err)
		}
	}
}

// syncLocked forgets the state a Clear made obsolete.
func (r *ReadbackReceiver) syncLocked() {
	if e := r.epoch.Load(); e != r.stEp {
		r.stEp, r.bound, r.rev, r.have, r.sum = e, nil, 0, false, [32]byte{}
	}
}

// clearLocked clears on behalf of the receiver itself (owner change).
func (r *ReadbackReceiver) clearLocked() {
	r.epoch.Add(1)
	r.syncLocked()
	r.clearPublished()
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
		r.clearLocked()
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
	case l.Context().Err() != nil || l.Expired():
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
		r.clearLocked() // defensive: LinkAdmitted does this already
	}
	sum := sha256.Sum256(f.Doc)
	if r.have && f.Revision == r.rev && sum == r.sum {
		// A repeat of what is published (the acknowledgement was lost).
		return proto.ReadbackAck{Revision: f.Revision}, nil
	}
	if r.have && f.Revision <= r.rev {
		return proto.ReadbackAck{}, refuse(proto.CodeConflict, proto.ReadbackStale, "the revision is not newer than the one applied")
	}
	// Verbatim: the receiver keeps the bytes the Hub signed the link for.
	ep := r.epoch.Load()
	if err := pub.SetUsage(bytes.Clone(f.Doc)); err != nil {
		r.log.Warn("the usage document was not published", "error", err)
		return proto.ReadbackAck{}, proto.NewError(proto.CodeInternal, "the usage document could not be published")
	}
	// A wipe that ran while the document was being written advanced the
	// enrollment generation before it cleared; look again after the write, so
	// that a document written after that clear is removed here.
	if r.epoch.Load() != ep || (r.moved != nil && r.moved(l.gen)) {
		r.clearLocked()
		return fenced("the enrollment or the connection changed while the document was published")
	}
	r.bound = &b
	r.rev, r.sum, r.have = f.Revision, sum, true
	return proto.ReadbackAck{Revision: f.Revision}, nil
}
