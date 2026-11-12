package workers

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/transport"
)

// Limits on what a Worker announces in its hello. The hello is at most 64 KiB
// already; these keep the registry small and its op names well formed.
const (
	maxAnnouncedOps = 256
	maxOpNameLen    = 64
)

var opName = regexp.MustCompile(`^[a-z][a-z0-9]*(_[a-z0-9]+)*$`)

// OpSession is the part of a transport session the registry uses.
// *transport.Session implements it; tests use a fake.
type OpSession interface {
	// OpenOp opens an op stream towards the Worker.
	OpenOp(ctx context.Context) (*transport.Stream, error)
	// Close ends the session and its connection.
	Close() error
	// Done is closed when the session has ended.
	Done() <-chan struct{}
}

// Online describes a connected Worker. It is a snapshot: later changes to the
// registry do not alter it.
type Online struct {
	WorkerID string
	Name     string
	OwnerID  string
	Kind     string
	Version  proto.Version // the protocol version negotiated for this connection
	Ops      []string      // the ops the Worker announced, sorted
	// Features are the proto.Feature* names the Worker announced in the signed
	// hello of this connection, sorted. They are connection state: never stored,
	// read again on every connect, so that a Worker replaced by an older build
	// cannot inherit the features of the build before it.
	Features    []string
	ConnectedAt time.Time
	// Pending is true while the Worker waits for approval (F-02). A pending
	// Worker is registered so that it shows as connected, but no op stream
	// is opened towards it and no Registry hook sees it; its approval ends
	// the connection, and the reconnect is registered as approved.
	Pending bool
	// TranscriptHash identifies the handshake of this connection. A signed
	// revoke that the Hub sends over the live connection is signed over it
	// (AD-9), so that it cannot be replayed on another connection. It is not
	// secret.
	TranscriptHash []byte
}

// Announces reports whether the Worker announced op.
func (o Online) Announces(op string) bool {
	i := sort.SearchStrings(o.Ops, op)
	return i < len(o.Ops) && o.Ops[i] == op
}

// HasFeature reports whether the Worker announced the feature.
func (o Online) HasFeature(f string) bool { return proto.HasFeature(o.Features, f) }

// Registry holds the Workers that are online. It is safe for concurrent use.
// A Worker is online from the moment its connect succeeded until its session
// ends; the registry removes it by itself when the session is done.
type Registry struct {
	// addMu orders whole Add calls: a hook must see the adds of one Worker in
	// the order the registry applied them, or a delayed hook of an older
	// connection would supersede the newer one (for example in the event
	// state of Story 5.3). Hooks must not block, so holding it is cheap.
	addMu   sync.Mutex
	mu      sync.Mutex
	online  map[string]*entry
	nextGen uint64
	now     func() time.Time
	hooks   []func(Online, OpSession)
}

type entry struct {
	info Online
	sess OpSession
	gen  uint64
	// signer is the operational key of this connection's snapshot: the live
	// revoke is signed with it (design 12.4), never with whatever key the Hub
	// uses now. nil for a registration without a snapshot (tests).
	signer *identity.Key
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{online: map[string]*entry{}, now: time.Now}
}

// Add marks a Worker online with its session. A Worker that reconnects while
// its previous session is still registered (a half-dead connection that the
// keepalive has not reaped yet) replaces it: the old session is closed. The
// ops are normalised to a sorted list of unique, well-formed names.
func (r *Registry) Add(info Online, sess OpSession) {
	r.add(info, sess, nil, nil)
}

// AddAdmitted is [Registry.Add] for a connection that took a Hub identity
// snapshot: admit is called under the registry lock, and when it reports false
// nothing is registered and AddAdmitted reports false (the activation fence,
// design 9.6: a handshake that stalled across an activation fails here, and
// the activation's closing of all sessions cannot miss one that passed). The
// operational key signs the live revoke of this connection.
func (r *Registry) AddAdmitted(info Online, sess OpSession, signer *identity.Key, admit func() bool) bool {
	return r.add(info, sess, signer, admit)
}

func (r *Registry) add(info Online, sess OpSession, signer *identity.Key, admit func() bool) bool {
	r.addMu.Lock()
	defer r.addMu.Unlock()
	info.Ops = normaliseOps(info.Ops)
	info.Features = proto.NormaliseFeatures(info.Features)
	r.mu.Lock()
	if admit != nil && !admit() {
		r.mu.Unlock()
		return false
	}
	if info.ConnectedAt.IsZero() {
		info.ConnectedAt = r.now()
	}
	r.nextGen++
	e := &entry{info: info, sess: sess, gen: r.nextGen, signer: signer}
	old := r.online[info.WorkerID]
	r.online[info.WorkerID] = e
	hooks := slices.Clone(r.hooks)
	r.mu.Unlock()

	if old != nil {
		_ = old.sess.Close()
	}
	if !info.Pending {
		for _, h := range hooks {
			h(clone(info), sess)
		}
	}
	go func() {
		<-sess.Done()
		r.remove(info.WorkerID, e.gen)
	}()
	return true
}

// WhileOffline runs fn only while the Worker has no live session, and keeps
// any registration of that Worker waiting until fn returned: it holds the
// lock that orders whole adds, so a connection cannot become online between
// the check and the write fn makes. It reports online = true (and does not run
// fn) when a session of the Worker is registered. fn must not touch the
// registry.
func (r *Registry) WhileOffline(workerID string, fn func() error) (online bool, err error) {
	r.addMu.Lock()
	defer r.addMu.Unlock()
	r.mu.Lock()
	_, online = r.online[workerID]
	r.mu.Unlock()
	if online {
		return true, nil
	}
	return false, fn()
}

// SignRevoke signs the revoke of the online connection of a Worker with the
// operational key of that connection's snapshot, over its transcript hash.
func (r *Registry) SignRevoke(workerID string) (RevokeFrame, bool, error) {
	r.mu.Lock()
	e := r.online[workerID]
	var (
		key  *identity.Key
		hash []byte
	)
	if e != nil {
		key, hash = e.signer, bytes.Clone(e.info.TranscriptHash)
	}
	r.mu.Unlock()
	if e == nil {
		return RevokeFrame{}, false, nil
	}
	if key == nil {
		return RevokeFrame{}, true, errors.New("workers: the connection has no operational key to sign a revoke")
	}
	f, err := SignRevoke(key, hash)
	return f, true, err
}

// OnAdd registers fn to be called, synchronously, for every Worker that is
// added from now on, after it is registered and its previous session (if any)
// was closed. Calls are made in the order of the adds, one Add at a time. It is how a module takes over the inbound streams of a
// session (Story 5.3: the ev stream) without the connect handler knowing it.
// fn must not block; it starts its own goroutines.
func (r *Registry) OnAdd(fn func(info Online, sess OpSession)) {
	if fn == nil {
		return
	}
	r.mu.Lock()
	r.hooks = append(r.hooks, fn)
	r.mu.Unlock()
}

// remove deletes the entry of a worker if it is still the same connection.
func (r *Registry) remove(id string, gen uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if e := r.online[id]; e != nil && e.gen == gen {
		delete(r.online, id)
	}
}

// Get returns the online record of a Worker.
func (r *Registry) Get(workerID string) (Online, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e := r.online[workerID]
	if e == nil {
		return Online{}, false
	}
	return clone(e.info), true
}

// IsOnline reports whether the Worker is connected.
func (r *Registry) IsOnline(workerID string) bool {
	_, ok := r.Get(workerID)
	return ok
}

// List returns every online Worker, ordered by worker id.
func (r *Registry) List() []Online {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Online, 0, len(r.online))
	for _, e := range r.online {
		out = append(out, clone(e.info))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].WorkerID < out[j].WorkerID })
	return out
}

// Len returns the number of online Workers.
func (r *Registry) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.online)
}

// OpenOp opens an op stream towards the Worker for the operation op. It is the
// only way the Hub opens an op stream, and it never dispatches an op the
// Worker did not announce in its hello (AD-4): that is unsupported_op, and an
// offline Worker is worker_offline. The caller writes the op.request on the
// returned stream and closes it.
//
// need lists the features the request relies on (proto.Feature*). They are
// checked in the same critical section that picks the session, so the stream
// that is opened belongs to a connection that announced them: a Worker that
// reconnects as an older build between a check and the open cannot receive the
// request. A Worker without one gets none: unsupported_op, with nothing sent.
func (r *Registry) OpenOp(ctx context.Context, workerID, op string, need ...string) (*transport.Stream, error) {
	r.mu.Lock()
	e := r.online[workerID]
	var (
		sess      OpSession
		announced bool
		missing   string
		pending   bool
	)
	if e != nil {
		sess, announced, pending = e.sess, e.info.Announces(op), e.info.Pending
		for _, f := range need {
			if !e.info.HasFeature(f) {
				missing = f
				break
			}
		}
	}
	r.mu.Unlock()

	switch {
	case e == nil:
		return nil, proto.NewError(proto.CodeWorkerOffline, "the Worker is not connected to the Hub")
	case pending:
		return nil, proto.NewError(proto.CodeWorkerPending, "the Worker is waiting for approval; approve it first")
	case !announced:
		return nil, proto.NewError(proto.CodeUnsupportedOp, fmt.Sprintf("the Worker does not support the operation %q", op))
	case missing != "":
		return nil, needsUpdate(op, missing)
	}
	st, err := sess.OpenOp(ctx)
	if err != nil {
		select {
		case <-sess.Done():
			return nil, proto.NewError(proto.CodeWorkerOffline, "the Worker connection ended")
		default:
		}
		return nil, fmt.Errorf("workers: open op stream: %w", err)
	}
	return st, nil
}

// Disconnect closes the session of a Worker, so that its next connect
// re-reads everything the Hub knows about it (Story 6.6: a policy change).
// It reports whether the Worker was online.
func (r *Registry) Disconnect(workerID string) bool {
	r.mu.Lock()
	e := r.online[workerID]
	r.mu.Unlock()
	if e == nil {
		return false
	}
	_ = e.sess.Close()
	return true
}

// CloseAll closes every session; it is the shutdown path.
func (r *Registry) CloseAll() {
	r.mu.Lock()
	sessions := make([]OpSession, 0, len(r.online))
	for _, e := range r.online {
		sessions = append(sessions, e.sess)
	}
	r.mu.Unlock()
	for _, s := range sessions {
		_ = s.Close()
	}
}

// OpenControl opens an op stream towards an online Worker for a control frame
// that is not an op request (the live revoke, Story 4.10). Unlike
// [Registry.OpenOp] it needs no announced op: the Worker's connector, not its
// op server, answers it. An offline Worker is worker_offline.
func (r *Registry) OpenControl(ctx context.Context, workerID string) (*transport.Stream, error) {
	r.mu.Lock()
	e := r.online[workerID]
	var sess OpSession
	if e != nil {
		sess = e.sess
	}
	r.mu.Unlock()
	if sess == nil {
		return nil, proto.NewError(proto.CodeWorkerOffline, "the Worker is not connected to the Hub")
	}
	st, err := sess.OpenOp(ctx)
	if err != nil {
		select {
		case <-sess.Done():
			return nil, proto.NewError(proto.CodeWorkerOffline, "the Worker connection ended")
		default:
		}
		return nil, fmt.Errorf("workers: open control stream: %w", err)
	}
	return st, nil
}

func clone(o Online) Online {
	o.Ops = append([]string(nil), o.Ops...)
	o.TranscriptHash = append([]byte(nil), o.TranscriptHash...)
	return o
}

// normaliseOps returns the sorted, de-duplicated list of well-formed op names,
// at most maxAnnouncedOps. Anything else a Worker announces is ignored, so a
// newer Worker can announce ops this Hub does not know (they are simply
// never dispatched).
func normaliseOps(ops []string) []string {
	seen := make(map[string]struct{}, len(ops))
	out := make([]string, 0, len(ops))
	for _, op := range ops {
		if len(op) > maxOpNameLen || !opName.MatchString(op) {
			continue
		}
		if _, dup := seen[op]; dup {
			continue
		}
		seen[op] = struct{}{}
		out = append(out, op)
		if len(out) == maxAnnouncedOps {
			break
		}
	}
	sort.Strings(out)
	return out
}

// needsUpdate is the refusal of a request that relies on a feature the Worker
// did not announce. It is an API error before anything was sent (never an
// outcome), so the caller may repeat the request once the Worker is updated.
func needsUpdate(op, feature string) error {
	return proto.NewError(proto.CodeUnsupportedOp,
		fmt.Sprintf("the Worker must be updated to run %q safely: it does not announce the feature %q; nothing was sent", op, feature))
}
