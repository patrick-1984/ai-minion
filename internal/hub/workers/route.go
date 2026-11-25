package workers

// Story 5.1: Hub op routing and target addressing (AD-2, AD-3, AD-11, AD-12,
// AD-15, AD-18).
//
// A [Router] is the Hub's faithful, permission-checked router for Worker ops:
//
//   - [Router.Resolve] turns the human target "worker-name:session-name" (or
//     "owner/worker-name:session-name") into the canonical ops.Target inside
//     the caller's space, or the act-as space;
//   - [Router.Call] and [Router.Stream] decide the call with authz.Decide,
//     compute the caller's effective actions for the resolved target, open one
//     new op stream, send one op.request that carries them, relay progress
//     and return the result. A write is never retried, and a stream that is
//     lost after the request left yields the outcome "unknown" (AD-15);
//   - [Router.ListWorkers] is the data behind workers_list.
//
// # Wire shape of an op stream (AD-3)
//
// Every frame is a uint32 big-endian length plus one UTF-8 JSON object with a
// "type" field (transport.Stream.WriteFrame). The Hub writes exactly one
// request and the Worker answers with progress frames and then exactly one
// result or error frame, after which the stream closes:
//
//	{"type":"op.request","op":"send_text","request_id":"<uuidv7>",
//	 "args":{...the op's Go argument struct...},"actions":["operate","read"],
//	 "origin":{"kind":"worker","source_worker_id":"...",...}}
//	{"type":"op.progress","data":{...}}      // zero or more; the elements of a streaming op
//	{"type":"op.result","result":{...}}      // exactly one, or:
//	{"type":"error","code":"forbidden","message":"..."}
//
// request_id is the request_id member of the arguments of a write (AD-15) and
// a fresh UUIDv7 for every other op. actions is the caller's effective action
// set on the resolved target (read, operate, message, manage); the Worker
// makes the final message versus operate check and applies its local policy
// whatever the Hub sends (AD-12). For a streaming op every element is an
// op.progress frame and the closing op.result carries no element.

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"slices"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/obs/compliance"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/transport"
)

// DefaultResultTimeout bounds how long the Hub waits for the answer of a
// non-streaming op once the request is sent. A write that runs out of time
// ends as "unknown": the Worker may still apply it.
const DefaultResultTimeout = 2 * time.Minute

// OpRequest is the op.request frame the Hub sends on an op stream.
type OpRequest struct {
	Type      string          `json:"type"`
	Op        string          `json:"op"`
	RequestID string          `json:"request_id"`
	Args      json.RawMessage `json:"args"`
	Actions   ops.ActionSet   `json:"actions"`
	// Origin is the server-derived provenance of the request (protocol 2.0,
	// mandatory). It is built from the authenticated principal and never from
	// caller JSON (see [Router.originOf]).
	Origin proto.Origin `json:"origin"`
}

// OpProgress is an op.progress frame: one element of a streaming op, or
// progress of a long op.
type OpProgress struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

// OpResult is the op.result frame that ends an op stream.
type OpResult struct {
	Type   string          `json:"type"`
	Result json.RawMessage `json:"result"`
}

// OpStream is the part of a transport stream the Router uses.
// *transport.Stream implements it; tests use a fake.
type OpStream interface {
	WriteFrame(v any) error
	ReadFrame() (json.RawMessage, error)
	Close() error
	// Reset abandons the stream at once.
	Reset()
}

var _ OpStream = (*transport.Stream)(nil)

// Backend is the part of the [Registry] the Router uses.
type Backend interface {
	// Get returns the online record of a Worker.
	Get(workerID string) (Online, bool)
	// OpenOp opens an op stream for op. It returns worker_offline for a
	// Worker that is not connected and unsupported_op, without opening a
	// stream, for an op the Worker did not announce. check, when not nil,
	// decides on the record of the very connection that gets the stream (see
	// [Registry.OpenOpChecked]); its error is returned with nothing opened.
	OpenOp(ctx context.Context, workerID, op string, check func(Online) error, need ...string) (OpStream, error)
}

// RegistryBackend adapts a [Registry] to a [Backend].
func RegistryBackend(r *Registry) Backend { return registryBackend{r} }

type registryBackend struct{ *Registry }

func (b registryBackend) OpenOp(ctx context.Context, workerID, op string, check func(Online) error, need ...string) (OpStream, error) {
	st, err := b.OpenOpChecked(ctx, workerID, op, check, need...)
	if err != nil {
		return nil, err
	}
	return st, nil
}

// WorkerRecord is an enrolled Worker as the Hub stores it.
type WorkerRecord struct {
	ID      string
	Name    string
	OwnerID string
	Kind    string
	// Fingerprint is the fingerprint of the Worker's public key (AD-5). The
	// origin of a request from this Worker carries it.
	Fingerprint string
	CreatedAt   time.Time
	// Pending is true while the Worker waits for approval (F-02); EnrollAddr is
	// the client address it enrolled from. A [Directory] lists only approved
	// Workers from Workers; the pending ones come from [AllWorkersDirectory].
	Pending    bool
	EnrollAddr string
	// NeverConnected is true while the Worker has never completed a connect
	// handshake (no first_connected_at): the owner may re-issue its pairing
	// (design 12.3).
	NeverConnected bool
	// HubCheck is the hub_check of the Worker's last hello ("" when none was
	// reported), kept so that an offline pending Worker still shows it.
	HubCheck string
}

// Directory lists the enrolled Workers that the Hub may route to: the
// approved ones. A pending Worker (F-02) is not in this list, so it cannot be
// resolved as a target, listed among sessions, or be the source of a request.
// It reads current state on every call.
type Directory interface {
	Workers(ctx context.Context) ([]WorkerRecord, error)
}

// AllWorkersDirectory is a [Directory] that can also list the Workers that
// wait for approval. Only the Workers list and the approval use it.
type AllWorkersDirectory interface {
	Directory
	AllWorkers(ctx context.Context) ([]WorkerRecord, error)
}

// WorkerStore is the part of the store the Hub's Directory reads;
// *store.Store implements it.
type WorkerStore interface {
	ListWorkers(ctx context.Context) ([]store.WorkerRow, error)
}

// StoreDirectory is a [Directory] on the enrolled Workers of the store. It
// holds no SQL (AD-16): it maps store rows to records.
type StoreDirectory struct{ Store WorkerStore }

// Workers returns every approved Worker, ordered by name, then id.
func (d StoreDirectory) Workers(ctx context.Context) ([]WorkerRecord, error) {
	all, err := d.AllWorkers(ctx)
	if err != nil {
		return nil, err
	}
	return slices.DeleteFunc(all, func(w WorkerRecord) bool { return w.Pending }), nil
}

// AllWorkers returns every enrolled Worker, the pending ones included, ordered
// by name, then id.
func (d StoreDirectory) AllWorkers(ctx context.Context) ([]WorkerRecord, error) {
	rows, err := d.Store.ListWorkers(ctx)
	if err != nil {
		return nil, fmt.Errorf("workers: list workers: %w", err)
	}
	out := make([]WorkerRecord, len(rows))
	for i, w := range rows {
		fp, _ := identity.FingerprintOf(ed25519.PublicKey(w.Pub)) // "" for an invalid key
		out[i] = WorkerRecord{
			ID: w.ID, Name: w.Name, OwnerID: w.OwnerID, Kind: w.Kind,
			Fingerprint: fp.String(),
			CreatedAt:   time.UnixMilli(w.CreatedAt).UTC(),
			Pending:     w.Pending, EnrollAddr: w.EnrollAddr,
			NeverConnected: w.FirstConnectedAt == 0,
			HubCheck:       w.HubCheck,
		}
	}
	return out, nil
}

// RouterConfig configures [NewRouter].
type RouterConfig struct {
	// Telemetry returns the traffic telemetry (op counts); nil: none.
	Telemetry func() Telemetry
	// Backend opens op streams and tells which Workers are online. Required;
	// use [RegistryBackend].
	Backend Backend
	// Directory lists the enrolled Workers. Required.
	Directory Directory
	// Authz is the one decision point. Required.
	Authz *authz.Authorizer
	// Catalog declares the ops; the default is ops.Default.
	Catalog *ops.Registry
	// OwnerName renders an owner id as the user name, for "owner/worker"
	// targets and workers_list. Without it the owner id is used.
	OwnerName func(ctx context.Context, ownerID string) string
	// ResultTimeout bounds the wait for the answer of a non-streaming op;
	// the default is [DefaultResultTimeout].
	ResultTimeout time.Duration
	// Logger receives routing problems. It never gets arguments, results or
	// terminal content. The default is slog.Default().
	Logger *slog.Logger
	// UserManaged reports whether people and API keys may control a Worker's
	// sessions (Minion control, v0.9.0). Nil means always.
	UserManaged func(ctx context.Context, workerID string) (bool, error)
	// HiddenSessions returns the hidden-sessions policy of a caller (Hub
	// setting hidden_sessions and the caller's own preference, v0.10.0).
	// Nil means hidden sessions are never refused.
	HiddenSessions HiddenSessionsPolicyFunc
	// Monitor records the calls routed to Workers for compliance monitoring;
	// nil: none.
	Monitor *compliance.Monitor
	// ContentManaged tells whether the content of a Worker's sessions may be
	// recorded; it must say no for a Worker it does not know. Nil: UserManaged.
	ContentManaged func(ctx context.Context, workerID string) (bool, error)
}

// Router routes catalog Worker ops to the Workers that run them.
type Router struct {
	backend    Backend
	dir        Directory
	az         *authz.Authorizer
	catalog    *ops.Registry
	ownerName  func(ctx context.Context, ownerID string) string
	timeout    time.Duration
	log        *slog.Logger
	userMgd    func(ctx context.Context, workerID string) (bool, error)
	hidden     HiddenSessionsPolicyFunc
	mon        *compliance.Monitor
	contentMgd func(ctx context.Context, workerID string) (bool, error)
	telemetry  func() Telemetry
	// synced reports whether a Worker's current connection sent its session
	// list (the event hub, set after the Router is built).
	synced atomic.Pointer[func(workerID string) bool]
}

// SetSessionsSynced sets the source of [ops.WorkerInfo.SessionsSynced]: the
// event hub, which a module builds after the Router.
func (rt *Router) SetSessionsSynced(fn func(workerID string) bool) {
	if fn == nil {
		rt.synced.Store(nil)
		return
	}
	rt.synced.Store(&fn)
}

// NewRouter returns a Router. It fails when a required dependency is missing.
func NewRouter(c RouterConfig) (*Router, error) {
	if c.Backend == nil || c.Directory == nil || c.Authz == nil {
		return nil, errors.New("workers: a Router needs a Backend, a Directory and an Authorizer")
	}
	rt := &Router{
		telemetry: c.Telemetry,
		userMgd:   c.UserManaged, hidden: c.HiddenSessions, mon: c.Monitor, contentMgd: c.ContentManaged,
		backend: c.Backend, dir: c.Directory, az: c.Authz, catalog: c.Catalog,
		ownerName: c.OwnerName, timeout: c.ResultTimeout, log: c.Logger,
	}
	if rt.catalog == nil {
		rt.catalog = ops.Default
	}
	if rt.timeout <= 0 {
		rt.timeout = DefaultResultTimeout
	}
	if rt.log == nil {
		rt.log = slog.Default()
	}
	return rt, nil
}

func (rt *Router) ownerLabel(ctx context.Context, ownerID string) string {
	if rt.ownerName == nil {
		return ownerID
	}
	if n := rt.ownerName(ctx, ownerID); n != "" {
		return n
	}
	return ownerID
}

// visibleFor returns the Workers the principal may read. Without all, only the
// Workers the principal's user owns (the act-as space); with all, every Worker,
// which the authorizer then narrows to those the principal may read. Each
// candidate is tested on a resource: the Worker itself, or, to resolve a session target, the session, which
// is what a key scoped to one session is allowed to read.
func (rt *Router) visibleFor(ctx context.Context, p authz.Principal, all bool, res func(WorkerRecord) authz.Resource) ([]WorkerRecord, error) {
	return rt.visibleFrom(ctx, rt.dir.Workers, p, all, res)
}

func (rt *Router) visibleFrom(ctx context.Context, list func(context.Context) ([]WorkerRecord, error), p authz.Principal, all bool, res func(WorkerRecord) authz.Resource) ([]WorkerRecord, error) {
	recs, err := list(ctx)
	if err != nil {
		return nil, fmt.Errorf("workers: read the Workers: %w", err)
	}
	if !all {
		recs = slices.DeleteFunc(slices.Clone(recs), func(w WorkerRecord) bool {
			return p.UserID == "" || w.OwnerID != p.UserID
		})
	}
	return authz.Filter(ctx, rt.az, p, recs, res)
}

// ListWorkers returns the Workers of workers_list: those visible to the
// principal (see [Router.visibleFor] for "mine" and all) with their online state,
// ordered by name, then owner, then id.
func (rt *Router) ListWorkers(ctx context.Context, p authz.Principal, all bool) ([]ops.WorkerInfo, error) {
	// The list is the one place that shows the Workers waiting for approval:
	// the owner and admins need them to approve or remove them.
	list := rt.dir.Workers
	if d, ok := rt.dir.(AllWorkersDirectory); ok {
		list = d.AllWorkers
	}
	recs, err := rt.visibleFrom(ctx, list, p, all, func(w WorkerRecord) authz.Resource { return authz.WorkerResource{ID: w.ID} })
	if err != nil {
		return nil, err
	}
	owners := map[string]string{}
	// The hidden-sessions policy of the caller, read once and only when a
	// Worker reports hidden sessions.
	var policy *HiddenSessionsPolicy
	policyFor := func() *HiddenSessionsPolicy {
		if policy == nil && rt.hidden != nil {
			pol, err := rt.hidden(ctx, p)
			if err != nil {
				pol = HiddenSessionsPolicy{Hub: config.HiddenSessionsDisabled}
			}
			policy = &pol
		}
		return policy
	}
	out := make([]ops.WorkerInfo, 0, len(recs))
	for _, w := range recs {
		owner, ok := owners[w.OwnerID]
		if !ok {
			owner = rt.ownerLabel(ctx, w.OwnerID)
			owners[w.OwnerID] = owner
		}
		info := ops.WorkerInfo{ID: w.ID, Name: w.Name, OwnerID: w.OwnerID, Owner: owner, Kind: w.Kind,
			Pending: w.Pending, Fingerprint: w.Fingerprint, FingerprintWords: identity.Fingerprint(w.Fingerprint).Words(),
			EnrollAddr: w.EnrollAddr, NeverConnected: w.NeverConnected, HubCheck: w.HubCheck}
		if !w.CreatedAt.IsZero() {
			at := ops.Timestamp(w.CreatedAt)
			info.EnrolledAt = &at
		}
		if on, ok := rt.backend.Get(w.ID); ok {
			at := ops.Timestamp(on.ConnectedAt)
			info.Online, info.ProtoVersion, info.ConnectedAt, info.Ops = true, on.Version.String(), &at, on.Ops
			info.Features = on.Features
			info.Build, info.OS, info.Arch, info.UpdateBlock = on.Facts.Build, on.Facts.OS, on.Facts.Arch, on.Facts.UpdateBlock
			info.SessionHost, info.SessionsHidden, info.SessionHostReason = on.Facts.SessionHost, on.Facts.SessionsHidden, on.Facts.SessionHostReason
			info.SessionsListError = on.Facts.SessionsListError
			if fn := rt.synced.Load(); fn != nil {
				info.SessionsSynced = (*fn)(w.ID)
			}
			if newSessionsHost(on.Facts) != hostVisible {
				if pol := policyFor(); pol != nil {
					info.NewSessionRefused = HiddenSessionRefusal(w.Name, on.Facts, *pol)
				}
			}
			info.DebugRemote = on.Facts.DebugRemote
			info.LoginStartupOutdated = on.Facts.LoginStartupOutdated
			info.HubCheck = on.Facts.HubCheck
			if w.Pending {
				// Nothing is routed to a pending Worker; do not offer what it announced.
				info.Ops, info.Features = nil, nil
			}
		}
		out = append(out, info)
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		if a.Owner != b.Owner {
			return a.Owner < b.Owner
		}
		return a.ID < b.ID
	})
	return out, nil
}

// Resolve resolves a human target "worker-name:session-name[:window.pane]"
// to the canonical target {worker_id, session_name} (AD-18).
//
// The Worker name is looked up among the Workers visible to the principal:
// the principal's own Workers, which for an act-as principal are the Workers
// of the target user. With all (an admin with the "mine" filter off) it is
// looked up among every Worker the principal may read, and a name that more
// than one owner uses is a conflict until it is written "owner/worker-name"
// (the owner's user name or id). A Worker id in place of the name is accepted
// when that Worker is visible. A name that matches no visible Worker is
// target_not_found, which is also what a foreign Worker looks like: the
// answer never says whether someone else's Worker exists. Whether the session
// exists is for the Worker to say.
func (rt *Router) Resolve(ctx context.Context, p authz.Principal, target string, all bool) (ops.Target, error) {
	ht, qualified, err := parseTarget(target)
	if err != nil {
		return ops.Target{}, err
	}
	// The candidates are tested on the session the target names: a key scoped
	// to one session may use it although it cannot read the whole Worker.
	cands, err := rt.visibleFor(ctx, p, all, func(w WorkerRecord) authz.Resource {
		return authz.SessionResource{WorkerID: w.ID, Name: ht.SessionName}
	})
	if err != nil {
		return ops.Target{}, err
	}
	var matches []WorkerRecord
	if ops.ValidUUIDv7(qualified.literal) {
		matches = filterRecords(cands, func(w WorkerRecord) bool { return w.ID == qualified.literal })
	}
	if len(matches) == 0 {
		// A name that contains "/" is both a literal Worker name and, read as
		// owner/worker-name, an owner-qualified one. Both readings are collected
		// and deduplicated: more than one distinct Worker is a conflict, never
		// a silent pick of the literal one.
		seen := map[string]bool{}
		for _, w := range cands {
			literal := qualified.literalOK && w.Name == qualified.literal
			owned := qualified.owner != "" && w.Name == qualified.name &&
				(w.OwnerID == qualified.owner || strings.EqualFold(rt.ownerLabel(ctx, w.OwnerID), qualified.owner))
			if (literal || owned) && !seen[w.ID] {
				seen[w.ID] = true
				matches = append(matches, w)
			}
		}
	}
	switch len(matches) {
	case 0:
		return ops.Target{}, proto.NewError(proto.CodeTargetNotFound, "no Worker of that name is visible to you")
	case 1:
		return ht.Resolve(matches[0].ID), nil
	}
	forms := make([]string, len(matches))
	for i, w := range matches {
		forms[i] = rt.ownerLabel(ctx, w.OwnerID) + "/" + w.Name
	}
	sort.Strings(forms)
	details, _ := json.Marshal(map[string]any{"candidates": forms})
	return ops.Target{}, &proto.Error{
		Code:    proto.CodeConflict,
		Message: fmt.Sprintf("several Workers match %q; write owner/worker-name:session-name", qualified.literal),
		Details: details,
	}
}

// targetForms is how the Worker part of a human target can be read.
type targetForms struct {
	// literal is the whole Worker part; literalOK tells it is a valid Worker
	// name (at most 64 bytes in all).
	literal   string
	literalOK bool
	// owner and name are the parts of an owner/worker-name reading, each
	// validated on its own; owner is empty when the part has no "/".
	owner, name string
}

// parseTarget parses a human target. The owner qualifier and the Worker name
// of an owner/worker-name target are validated separately, so a 64-byte
// Worker name stays addressable behind a qualifier. The returned HumanTarget
// carries the session and the pane.
func parseTarget(target string) (ops.HumanTarget, targetForms, error) {
	ht, litErr := ops.ParseHumanTarget(target)
	head, rest, _ := strings.Cut(target, ":")
	f := targetForms{literal: head, literalOK: litErr == nil}
	owner, name, has := strings.Cut(head, "/")
	if has && owner != "" && name != "" {
		if _, ownerErr := ops.ParseHumanTarget(owner + ":x"); ownerErr == nil {
			if qht, err := ops.ParseHumanTarget(name + ":" + rest); err == nil {
				f.owner, f.name = owner, name
				if litErr != nil {
					ht = qht
				}
				return ht, f, nil
			}
		}
	}
	if litErr != nil {
		return ops.HumanTarget{}, f, litErr
	}
	return ht, f, nil
}

func filterRecords(in []WorkerRecord, keep func(WorkerRecord) bool) []WorkerRecord {
	var out []WorkerRecord
	for _, w := range in {
		if keep(w) {
			out = append(out, w)
		}
	}
	return out
}

// CallRequest is one op call to route.
type CallRequest struct {
	// Principal is the caller; for act-as it is the act-as principal.
	Principal authz.Principal
	// Op is the catalog name of a Worker op.
	Op string
	// WorkerID is the Worker that runs the op. Required.
	WorkerID string
	// Session is the session the op acts on; empty for an op on the Worker
	// itself (sessions_list, session_create).
	Session string
	// Args is the op's argument struct (a value or a pointer), encoded as is.
	Args any
	// PromptOnly routes a send_text of a caller that holds message but not
	// operate on the session (a Message grant): the decision accepts message
	// too, and the Worker gets operate for this one agent-only, submitted,
	// unforced send. The caller (the name-only call handler) builds those
	// arguments; any other op or shape is an internal error.
	PromptOnly bool
	// admit is the last, private check on a scoped master dispatch, after
	// opening the exact destination connection and before sending any bytes.
	admit func(context.Context, Online) error
}

// Call routes a non-streaming op and returns its result as a value of the
// op's Result type.
//
// The caller needs the op's accepted actions on the Worker or session: a
// failure is the *proto.Error of authz.Decide (not_found for a Worker the
// caller has no access to, forbidden otherwise). The Worker must be online
// (worker_offline) and must have announced the op (unsupported_op); both are
// answered without opening a stream. An error frame of the Worker is returned
// as the same code.
//
// A write is never retried. When the connection is lost, or no answer comes
// in time, after the request was sent, the write returns, with no error, the
// result {outcome: unknown, reason: worker_offline} and a no_answer warning:
// the Worker may or may not have applied it (AD-15). A read in the same
// situation returns worker_offline.
func (rt *Router) Call(ctx context.Context, req CallRequest) (any, error) {
	start := time.Now()
	var c *call
	res, err := rt.call(ctx, req, &c)
	rt.debugRouted(req, start, res, err, c)
	rt.observe(ctx, req, c, res, err)
	return res, err
}

// debugRouted writes, at debug level, one line about an op routed to a
// Worker: the op, the Worker, the outcome (ok, the write outcome or the error
// code) and the duration. Arguments and results are never logged.
func (rt *Router) debugRouted(req CallRequest, start time.Time, res any, err error, c *call) {
	outcome := "ok"
	if wr, ok := res.(ops.WriteResult); ok {
		outcome = string(wr.Outcome)
	}
	if err != nil {
		outcome = string(proto.CodeInternal)
		var pe *proto.Error
		if errors.As(err, &pe) {
			outcome = string(pe.Code)
		}
	}
	// The metric counts the ops that were sent to a Worker, for the Worker
	// that took them (captured when the stream opened, so a connection that
	// is gone by now keeps its attribution); a refusal before dispatch is
	// not an op sent and takes no Worker slot.
	if t := rt.tel(); t != nil && c != nil && c.dispatched.Load() {
		t.OpRouted(c.workerID, req.Op, outcome, time.Since(start))
	}
	rt.log.Debug("workers: op routed", "op", req.Op, "worker_id", req.WorkerID,
		"principal_kind", string(req.Principal.Kind), "outcome", outcome, "duration", time.Since(start).String())
}

func (rt *Router) call(ctx context.Context, req CallRequest, out **call) (any, error) {
	c, err := rt.prepare(ctx, req, false)
	if err != nil {
		return nil, err
	}
	*out = c
	ctx, cancel := context.WithTimeout(ctx, rt.timeout)
	defer cancel()
	body, err := rt.exchange(ctx, c, nil)
	if err != nil {
		var le *lostError
		if c.op.Write && errors.As(err, &le) {
			rt.log.Warn("workers: write ended unknown", "op", c.op.Name, "worker_id", req.WorkerID,
				"request_id", c.frame.RequestID, "cause", le.Error())
			return unknownWrite(), nil
		}
		if c.op.Mutates && errors.As(err, &le) {
			rt.log.Warn("workers: a state change ended unknown", "op", c.op.Name, "worker_id", req.WorkerID, "cause", le.Error())
			return nil, &OutcomeUnknownError{Err: proto.NewError(proto.CodeConnectionLost, unknownChangeMessage)}
		}
		return nil, rt.readError(err)
	}
	res, err := decodeResult(c.op, body)
	if err != nil {
		if c.op.Mutates {
			rt.log.Warn("workers: a state change was answered with an unusable result", "op", c.op.Name, "worker_id", req.WorkerID)
			return nil, &OutcomeUnknownError{Err: proto.NewError(proto.CodeConnectionLost, unknownChangeMessage)}
		}
		if c.op.Write {
			rt.log.Warn("workers: write answered with an unusable result", "op", c.op.Name,
				"worker_id", req.WorkerID, "request_id", c.frame.RequestID)
			return unknownWrite(), nil
		}
		return nil, proto.NewError(proto.CodeInternal, "the Worker sent a result the Hub cannot read")
	}
	if wr, ok := res.(ops.WriteResult); ok && wr.Validate() != nil {
		rt.log.Warn("workers: write answered with an invalid result", "op", c.op.Name,
			"worker_id", req.WorkerID, "request_id", c.frame.RequestID)
		return unknownWrite(), nil
	}
	return res, nil
}

// unknownChangeMessage is what a caller is told of a state change (an op that
// Mutates and is not a Write) whose request went out and whose answer did not
// come.
const unknownChangeMessage = "the connection to the Worker ended before it answered; the change may or may not have been made. The Hub hides what the change would hide until the Worker confirms its current state; ask for the current state before changing it again"

// OutcomeUnknownError is the error of a state change that is not a Write op
// (share_set) whose request was or may have been delivered and whose answer was
// lost or unusable (AD-15). The Worker may or may not have applied it. It is
// told apart from every error that was returned before anything could change
// (a refusal, a Worker that was not reached) so that its caller can protect
// what the change would have protected until the outcome is known. On the wire
// it is the *proto.Error connection_lost.
type OutcomeUnknownError struct{ Err *proto.Error }

func (e *OutcomeUnknownError) Error() string { return e.Err.Error() }

// Unwrap returns the wire error.
func (e *OutcomeUnknownError) Unwrap() error { return e.Err }

// Stream routes a streaming op. It calls emit with each element, a value of
// the op's Result type, in order, and returns when the Worker ends the
// stream. Errors are as for [Router.Call]; a stream that is lost midway ends
// with worker_offline. A failing emit abandons the stream and its error is
// returned. The wait for the next element has no time limit of its own: it
// ends with ctx, with the Worker's own timeout or with the connection.
func (rt *Router) Stream(ctx context.Context, req CallRequest, emit func(any) error) error {
	start := time.Now()
	var c *call
	err := rt.stream(ctx, req, emit, &c)
	rt.debugRouted(req, start, nil, err, c)
	return err
}

func (rt *Router) stream(ctx context.Context, req CallRequest, emit func(any) error, out **call) error {
	c, err := rt.prepare(ctx, req, true)
	if err != nil {
		return err
	}
	*out = c
	_, err = rt.exchange(ctx, c, func(data json.RawMessage) error {
		el, err := decodeResult(c.op, data)
		if err != nil {
			return &lostError{cause: errors.New("unreadable stream element")}
		}
		return emit(el)
	})
	return rt.readError(err)
}

// errStaleOwner refuses remote debug on a connection admitted under another
// owner than the current one.
var errStaleOwner = proto.NewError(proto.CodeConflict, "the Worker is reconnecting after an owner change; try again")

// debugOwnerCheck returns the check of worker_debug and worker_logs, nil for
// any other op. The caller was authorised on the Worker's current owner, but
// the connection that gets the op carries the owner recorded at its welcome:
// a reconnect that read the previous owner before a reassignment committed can
// register after it, and that Worker still keeps the previous owner's log
// lines and debug switch. The op goes only to a connection whose welcome owner
// is the current owner (read here, right before dispatch); otherwise it is
// refused and the caller retries once the Worker has reconnected.
func (rt *Router) debugOwnerCheck(ctx context.Context, op, workerID string) (func(Online) error, error) {
	if op != ops.OpWorkerDebug && op != ops.OpWorkerLogs && op != ops.OpWorkerUsage {
		return nil, nil
	}
	recs, err := rt.dir.Workers(ctx)
	if err != nil {
		return nil, err
	}
	owner := ""
	for _, w := range recs {
		if w.ID == workerID {
			owner = w.OwnerID
			break
		}
	}
	return func(on Online) error {
		if owner == "" || on.OwnerID != owner {
			return errStaleOwner
		}
		return nil
	}, nil
}

// call is a prepared op call: the op, the request frame and the worker.
type call struct {
	op       ops.Op
	frame    OpRequest
	workerID string
	// need are the features of the Worker that the request relies on
	// (requiredFeatures); the Backend checks them with the session it opens.
	need []string
	// check, when set, decides on the connection that gets the op (the
	// hidden-session refusal of session_create).
	check func(Online) error
	admit func(context.Context, Online) error
	// dispatched is set when the Worker's connection took the op (the
	// stream opened): from then on it counts as an op sent to that Worker.
	dispatched atomic.Bool
}

// CanReadSession reports whether p may read the session of the Worker now.
// A refusal is false, only a failing lookup is an error.
func (rt *Router) CanReadSession(ctx context.Context, p authz.Principal, workerID, session string) (bool, error) {
	err := rt.az.Decide(ctx, p, ops.MustActionSet(ops.ActionRead), authz.SessionResource{WorkerID: workerID, Name: session})
	if err == nil {
		return true, nil
	}
	if c := proto.CodeOf(err); c == proto.CodeForbidden || c == proto.CodeNotFound {
		return false, nil
	}
	return false, err
}

// HoldsMessageOnly reports whether p holds message but not operate on the
// session now: the holder of a Message grant, whose send_text is a prompt.
func (rt *Router) HoldsMessageOnly(ctx context.Context, p authz.Principal, workerID, session string) (bool, error) {
	held, err := rt.az.EffectiveActions(ctx, p, authz.SessionResource{WorkerID: workerID, Name: session})
	if err != nil {
		return false, err
	}
	return slices.Contains(held, ops.ActionMessage) && !slices.Contains(held, ops.ActionOperate), nil
}

// promptOnlyRequest reports whether req is the one shape PromptOnly allows:
// send_text on a session, agent-only, submitted and not forced.
func promptOnlyRequest(op string, req CallRequest) bool {
	if op != ops.OpSendText || req.Session == "" {
		return false
	}
	var a ops.SendTextArgs
	switch v := req.Args.(type) {
	case ops.SendTextArgs:
		a = v
	case *ops.SendTextArgs:
		if v == nil {
			return false
		}
		a = *v
	default:
		return false
	}
	return a.AgentOnly && a.Submit && !a.Force
}

// prepare validates the request, decides it and computes the request frame.
// Nothing has been sent yet when it returns an error.
func (rt *Router) prepare(ctx context.Context, req CallRequest, wantStream bool) (*call, error) {
	op, ok := rt.catalog.Lookup(req.Op)
	if !ok || op.Scope != ops.ScopeWorker {
		return nil, proto.NewError(proto.CodeUnsupportedOp, "not a Worker operation")
	}
	if op.Stream != wantStream {
		return nil, proto.NewError(proto.CodeInternal, "the operation was routed through the wrong call kind")
	}
	if req.WorkerID == "" {
		return nil, proto.NewError(proto.CodeInvalidArgument, "the Worker is not named")
	}
	var res authz.Resource = authz.WorkerResource{ID: req.WorkerID}
	if req.Session != "" {
		res = authz.SessionResource{WorkerID: req.WorkerID, Name: req.Session}
	}
	accepted := op.Actions
	if req.PromptOnly {
		if !promptOnlyRequest(op.Name, req) {
			return nil, proto.NewError(proto.CodeInternal, "a prompt-only request must be an agent-only, submitted send_text")
		}
		accepted = ops.MustActionSet(ops.ActionOperate, ops.ActionMessage)
	}
	if err := rt.az.Decide(ctx, req.Principal, accepted, res); err != nil {
		return nil, err
	}
	if err := rt.checkUserManaged(ctx, req.Principal, op.Name, req.WorkerID); err != nil {
		return nil, err
	}
	check, err := rt.hiddenSessionCheck(ctx, req.Principal, op.Name, req.WorkerID)
	if err != nil {
		return nil, err
	}
	if check == nil {
		if check, err = rt.debugOwnerCheck(ctx, op.Name, req.WorkerID); err != nil {
			return nil, err
		}
	}
	actions, err := rt.az.EffectiveActions(ctx, req.Principal, res)
	if err != nil {
		return nil, err
	}
	if req.PromptOnly {
		// The Worker executes send_text under operate; the agent-only guard
		// of the arguments keeps the send a prompt to an agent pane.
		if !slices.Contains(actions, ops.ActionOperate) {
			if actions, err = ops.NewActionSet(append(slices.Clone(actions), ops.ActionOperate)...); err != nil {
				return nil, proto.NewError(proto.CodeInternal, "")
			}
		}
		inner := check
		check = func(on Online) error {
			if !on.HasFeature(proto.FeatureSendTextAgentOnly) {
				return proto.NewError(proto.CodeUnsupportedOp, "the Worker does not support prompts to agent panes only; update it")
			}
			if inner != nil {
				return inner(on)
			}
			return nil
		}
	}
	args, err := json.Marshal(req.Args)
	if err != nil {
		return nil, proto.NewError(proto.CodeInvalidArgument, "the arguments cannot be encoded")
	}
	if req.Args == nil || (reflect.ValueOf(req.Args).Kind() == reflect.Pointer && reflect.ValueOf(req.Args).IsNil()) {
		args = []byte("{}")
	}
	origin, err := rt.originOf(ctx, req.Principal, accepted, res)
	if err != nil {
		return nil, err
	}
	id := ops.NewUUIDv7()
	if op.Write {
		var w struct {
			RequestID string `json:"request_id"`
		}
		if err := json.Unmarshal(args, &w); err != nil || !ops.ValidUUIDv7(w.RequestID) {
			return nil, proto.NewError(proto.CodeInvalidArgument, "a write needs a request_id that is a UUIDv7")
		}
		id = w.RequestID
	}
	return &call{
		op:       op,
		workerID: req.WorkerID,
		need:     requiredFeatures(op.Name, args),
		check:    check,
		admit:    req.admit,
		frame: OpRequest{
			Type: proto.TypeOpRequest, Op: op.WireName(), RequestID: id,
			Args: args, Actions: actions, Origin: origin,
		},
	}, nil
}

// userControlOps are the session operations that a Worker which is not
// user-managed refuses from people and API keys (Minion control, v0.9.0).
// Managing the Worker itself (sharing, update) stays with its owner and admins.
var userControlOps = map[string]bool{
	ops.OpSessionsList: true, ops.OpSessionSnapshot: true, ops.OpSessionCapture: true,
	ops.OpSendText: true, ops.OpSendKeys: true, ops.OpSetTask: true, ops.OpAnswer: true,
	ops.OpSessionCreate: true, ops.OpPipeSend: true,
}

// checkUserManaged refuses a person or an API key on the sessions of a Worker
// that is not user-managed. A Worker principal (a master session's grant) is
// not affected. A failed lookup refuses (fail closed).
func (rt *Router) checkUserManaged(ctx context.Context, p authz.Principal, op, workerID string) error {
	if rt.userMgd == nil || p.Kind == authz.KindWorker || !userControlOps[op] {
		return nil
	}
	ok, err := rt.userMgd(ctx, workerID)
	if err != nil {
		return fmt.Errorf("workers: read the Minion control of the Worker: %w", err)
	}
	if !ok {
		return proto.NewError(proto.CodeForbidden, "this Worker is not user-managed: only master sessions of the Workers it accepts control it (Access > Minion control)")
	}
	return nil
}

// originOf derives the structured origin of a request from the authenticated
// principal (AD-12, F-01). Nothing of it comes from the caller's JSON. A Worker
// identity, and an API key bound to a Worker, name that Worker as the source
// Worker, with its fingerprint and the id of its owner (for a bound key the
// owner of the bound Worker, whoever owns the key). A Worker identity whose
// session-control grant by itself authorizes the op (accepted actions on res)
// is marked Grant (authz.Authorizer.GrantedByScope); the own-Worker rule, a
// key and a user never are. The origin is validated before it is sent: a
// request whose origin cannot be derived is not sent.
func (rt *Router) originOf(ctx context.Context, p authz.Principal, accepted ops.ActionSet, res authz.Resource) (proto.Origin, error) {
	var o proto.Origin
	sourceID := ""
	switch p.Kind {
	case authz.KindBrowserSession:
		o = proto.Origin{Kind: proto.OriginUser, UserID: p.UserID}
	case authz.KindActAs:
		o = proto.Origin{Kind: proto.OriginUser, UserID: p.UserID, ActorID: p.ActorID}
	case authz.KindAPIKey:
		o = proto.Origin{Kind: proto.OriginAPIKey, UserID: p.UserID, KeyID: p.KeyID, KeyBoundWorkerID: p.WorkerID}
		sourceID = p.WorkerID
	case authz.KindWorker:
		granted, err := rt.az.GrantedByScope(ctx, p, accepted, res)
		if err != nil {
			return proto.Origin{}, err
		}
		o = proto.Origin{Kind: proto.OriginWorker, Grant: granted}
		sourceID = p.WorkerID
	}
	if sourceID != "" {
		recs, err := rt.dir.Workers(ctx)
		if err != nil {
			return proto.Origin{}, fmt.Errorf("workers: read the Workers: %w", err)
		}
		i := slices.IndexFunc(recs, func(w WorkerRecord) bool { return w.ID == sourceID })
		if i < 0 {
			return proto.Origin{}, proto.NewError(proto.CodeForbidden, "the calling Worker is not enrolled")
		}
		o.SourceWorkerID, o.SourceWorkerFP, o.SourceWorkerOwnerID = sourceID, recs[i].Fingerprint, recs[i].OwnerID
	}
	if err := o.Validate(); err != nil {
		rt.log.Warn("workers: the origin of a request cannot be derived", "kind", string(p.Kind), "cause", err.Error())
		return proto.Origin{}, proto.NewError(proto.CodeInternal, "the origin of the request could not be derived")
	}
	return o, nil
}

// lostError means the request was (or may have been) sent and no answer came:
// the connection ended, the Worker sent something unreadable, or time ran out.
type lostError struct {
	cause   error
	timeout bool
}

func (e *lostError) Error() string { return "lost the op stream: " + e.cause.Error() }
func (e *lostError) Unwrap() error { return e.cause }

// exchange opens the op stream, sends the request and reads frames until the
// result. It returns the body of the op.result. onElement, when set, gets the
// data of every op.progress frame (the elements of a streaming op); an error
// it returns abandons the stream and is returned as is. Failures before
// anything was sent are returned as they are; every failure after that is a
// *lostError, except an error frame of the Worker (a *proto.Error) and an
// error of onElement.
func (rt *Router) exchange(ctx context.Context, c *call, onElement func(json.RawMessage) error) (json.RawMessage, error) {
	check := c.check
	var destination Online
	if c.admit != nil {
		check = func(on Online) error {
			destination = on
			if c.check != nil {
				return c.check(on)
			}
			return nil
		}
	}
	st, err := rt.backend.OpenOp(ctx, c.workerID, c.frame.Op, check, c.need...)
	if err != nil {
		if pe, ok := err.(*proto.Error); ok && pe == errStaleOwner {
			// A connection admitted under the previous owner: close it, so
			// that the Worker reconnects, learns its owner from the new
			// welcome and clears what the previous owner may read.
			if d, ok := rt.backend.(interface{ Disconnect(string) bool }); ok {
				d.Disconnect(c.workerID)
			}
		}
		var pe *proto.Error
		if errors.As(err, &pe) {
			return nil, err
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, proto.NewError(proto.CodeWorkerOffline, "the Worker connection could not take a new operation")
	}
	// The context ending abandons the stream, which ends the blocked read.
	stopWatch := context.AfterFunc(ctx, st.Reset)
	defer stopWatch()

	lost := func(err error) error {
		st.Reset()
		return &lostError{cause: err, timeout: errors.Is(ctx.Err(), context.DeadlineExceeded)}
	}
	if c.admit != nil {
		if err := c.admit(ctx, destination); err != nil {
			st.Reset()
			return nil, err
		}
	}
	if err := st.WriteFrame(c.frame); err != nil {
		var pe *proto.Error
		if errors.As(err, &pe) {
			// The frame was refused before a byte was written: nothing is sent.
			st.Reset()
			return nil, err
		}
		c.dispatched.Store(true) // bytes may have reached the Worker
		return nil, lost(err)
	}
	c.dispatched.Store(true)
	for {
		body, err := readFrame(ctx, st)
		if err != nil {
			return nil, lost(err)
		}
		typ, err := proto.FrameType(body)
		if err != nil {
			return nil, lost(err)
		}
		switch typ {
		case proto.TypeOpProgress:
			if onElement == nil {
				continue
			}
			var f OpProgress
			if err := proto.DecodeFrame(body, &f); err != nil {
				return nil, lost(err)
			}
			if err := onElement(f.Data); err != nil {
				st.Reset()
				var le *lostError
				if errors.As(err, &le) {
					return nil, lost(le.cause)
				}
				return nil, err
			}
		case proto.TypeOpResult:
			var f OpResult
			if err := proto.DecodeFrame(body, &f); err != nil {
				return nil, lost(err)
			}
			_ = st.Close()
			return f.Result, nil
		case proto.TypeError:
			var f proto.ErrorFrame
			if err := proto.DecodeFrame(body, &f); err != nil {
				return nil, lost(err)
			}
			_ = st.Close()
			return nil, f.Err()
		}
		// Any other frame type is ignored: minor protocol changes are additive.
	}
}

// readFrame reads one frame and gives up when ctx ends. Resetting a real
// transport stream half-closes it and drains the rest, so the read in flight
// can stay blocked on a silent peer for the stream close timeout; the caller
// must not wait for it. The read still ends once the stream is gone.
func readFrame(ctx context.Context, st OpStream) (json.RawMessage, error) {
	type result struct {
		body json.RawMessage
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		body, err := st.ReadFrame()
		ch <- result{body, err}
	}()
	select {
	case r := <-ch:
		return r.body, r.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// readError turns the failure of an exchange into the error a caller sees
// where there is no outcome to report.
func (rt *Router) readError(err error) error {
	var le *lostError
	switch {
	case err == nil:
		return nil
	case errors.As(err, &le):
		if le.timeout {
			return proto.NewError(proto.CodeWorkerOffline, "the Worker did not answer in time")
		}
		return proto.NewError(proto.CodeWorkerOffline, "the connection to the Worker ended before it answered")
	}
	return err
}

// unknownWrite is the result of a write whose fate cannot be told (AD-15).
func unknownWrite() ops.WriteResult {
	return ops.WriteResult{
		Outcome: ops.OutcomeUnknown,
		Reason:  string(proto.CodeWorkerOffline),
		Warnings: []ops.Warning{{
			Code:    ops.WarnNoAnswer,
			Message: "The connection to the Worker ended before it answered; the write may or may not have been applied. Do not repeat it blindly.",
		}},
	}
}

// decodeResult decodes an op.result or a stream element into a value of the
// op's Result type. Unknown fields are ignored (AD-4).
func decodeResult(op ops.Op, body json.RawMessage) (any, error) {
	if len(body) == 0 || string(body) == "null" {
		return nil, errors.New("no result")
	}
	ptr := op.NewResult()
	if err := json.Unmarshal(body, ptr); err != nil {
		return nil, err
	}
	return reflect.ValueOf(ptr).Elem().Interface(), nil
}

// tel returns the Router's telemetry, or nil.
func (rt *Router) tel() Telemetry {
	if rt.telemetry == nil {
		return nil
	}
	return rt.telemetry()
}
