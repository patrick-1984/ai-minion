package files

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/audit"
	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/transport"
)

// Store is the part of the Hub store the service uses; *store.Store
// implements it.
type Store interface {
	InsertFileTransfer(ctx context.Context, r store.FileTransferRow) error
	FileTransfer(ctx context.Context, id string) (store.FileTransferRow, error)
	MarkFileTransferPrepared(ctx context.Context, id string, p store.FilePrepared, now int64) error
	FinishFileTransfer(ctx context.Context, id string, out store.FileOutcome, now int64) (store.FileTransferRow, bool, error)
	FileTransferEntries(ctx context.Context, id string) ([]ops.FileTransferEntry, error)
	ReconcileFileTransfers(ctx context.Context, now int64) ([]string, error)
	PurgeFileTransfers(ctx context.Context, before int64) (int64, error)
	ListFileTransfers(ctx context.Context, f store.FileTransferFilter) ([]store.FileTransferRow, string, error)
	WorkerFileMode(ctx context.Context, workerID string) (string, error)
	WorkerPreviewOnly(ctx context.Context, workerID string) (bool, error)
	FileAcceptances(ctx context.Context, source string) ([]store.AcceptRow, error)
}

// Decider decides the file action of a transfer on its target session;
// *authz.Authorizer implements it.
type Decider interface {
	DecideFile(ctx context.Context, p authz.Principal, direction string, res authz.SessionResource) (owner string, basis authz.FileBasis, err error)
}

// Auditor records audit events; *audit.Log implements it.
type Auditor interface {
	Record(ctx context.Context, ev audit.Event) error
}

// LegOpener opens the data leg to a target Worker: an fx stream on the
// connection whose handshake transcript is conn (the connection the
// transfer was admitted on). Another connection, a Worker that is offline or
// a connection without files.v1 is an error, and nothing is opened.
type LegOpener interface {
	OpenLeg(ctx context.Context, workerID string, conn []byte) (*transport.LegConn, error)
}

// Config configures [New].
type Config struct {
	// Store, Authz and Legs are required.
	Store Store
	Authz Decider
	Legs  LegOpener
	// Audit records the audit events; nil records none.
	Audit Auditor
	// Policy returns the Hub-wide policy (ops.FileModeOn, ...); nil or "" is
	// on. Read on every decision (live).
	Policy func() string
	// IsAdmin tells whether the principal is an administrator now (for the
	// admins_only policy and the history). Nil: nobody is.
	IsAdmin func(ctx context.Context, p authz.Principal) (bool, error)
	// KeyLive tells whether the API key a principal was built from still
	// authenticates (not revoked, not expired): asked at every re-decision
	// and at the commit gate, so a key revoked while bytes move publishes
	// nothing. Nil: keys are not checked again.
	KeyLive func(ctx context.Context, keyID string) error
	// SessionShared tells whether a Worker's session is shared with the Hub
	// now (the caller's own session must be, FR-10). Nil: every session is.
	SessionShared func(ctx context.Context, workerID, session string) (bool, error)
	// Conn returns the handshake transcript of a Worker's current connection,
	// nil when it is offline. Required.
	Conn func(workerID string) []byte
	// Limits are the Hub's limits of one transfer; the zero value is
	// ops.DefaultFileTransferLimits.
	Limits ops.FileTransferLimits
	// Retention returns how long records are kept; nil or 0 is
	// store.DefaultFileTransferRetention.
	Retention func() time.Duration
	// Now is the clock; nil is time.Now.
	Now func() time.Time
	// Logger gets problems, never paths, names or tokens; nil is
	// slog.Default().
	Logger *slog.Logger

	// Redecide is the period of the re-decision while bytes move;
	// 0 is proto.RedecideInterval. IdleTimeout ends a leg without body
	// bytes; 0 is proto.LegIdleTimeout. ReceiptWait is how long a received
	// pull waits for its receipt before it is unknown; 0 is two minutes.
	// Tests shorten them.
	Redecide    time.Duration
	IdleTimeout time.Duration
	ReceiptWait time.Duration
	// PreparedDeadline is the store deadline of a prepared record (after it,
	// reconciliation turns it unknown); 0 is 12 hours.
	PreparedDeadline time.Duration
}

// Service is the Hub's transfer service.
type Service struct {
	cfg    Config
	log    *slog.Logger
	limits ops.FileTransferLimits

	mu sync.Mutex
	// live are the transfers admitted by this process that are not finished.
	live map[string]*transfer
	// admissions are the admission times per Worker in the last minute.
	admissions map[string][]time.Time
	// pending are the transfers that hold concurrency slots (take).
	pending map[*transfer]struct{}
}

// New returns the service. It fails when a required dependency is missing.
func New(cfg Config) (*Service, error) {
	if cfg.Store == nil || cfg.Authz == nil || cfg.Legs == nil || cfg.Conn == nil {
		return nil, errors.New("files: the transfer service needs a store, an authorizer, a leg opener and the connections")
	}
	s := &Service{cfg: cfg, log: cfg.Logger, live: map[string]*transfer{}, admissions: map[string][]time.Time{}, pending: map[*transfer]struct{}{}}
	if s.log == nil {
		s.log = slog.Default()
	}
	s.limits = ops.DefaultFileTransferLimits().Lower(cfg.Limits)
	return s, nil
}

func (s *Service) now() time.Time {
	if s.cfg.Now != nil {
		return s.cfg.Now()
	}
	return time.Now()
}

func (s *Service) nowMS() int64 { return s.now().UnixMilli() }

func dur(v, def time.Duration) time.Duration {
	if v > 0 {
		return v
	}
	return def
}

// transfer is a live transfer of this process.
type transfer struct {
	id        string
	direction string
	kind      string
	path      string

	targetWorker, targetSession string
	// callerWorker and callerSession are the caller's end; empty for a REST
	// or browser caller.
	callerWorker, callerSession string
	// callerID is the identity the caller leg must come from (identityOf);
	// callerConn the transcript of the caller's connection (nil for a leg
	// over the Hub's HTTP endpoint); targetConn the target's.
	callerID   string
	callerConn []byte
	targetConn []byte
	// web is a browser caller: a download ends served, never delivered.
	web bool

	// size is a push's declared logical bytes; user the caller's user, for
	// the per-user limit.
	size int64
	user string

	callerToken, targetToken string
	expires                  time.Time
	limits                   ops.FileTransferLimits

	// principal is the caller's principal with its file authority; verified
	// is the calling session ("" for none) and bound whether the Hub verified
	// it (a bound master lease).
	principal authz.Principal
	verified  string
	bound     bool
	// refresh, when set, reads the caller's principal again from its
	// admission path (workers.FileCall.Refresh) for every re-decision.
	refresh func(ctx context.Context) (authz.Principal, error)

	// release gives back the concurrency slots of the transfer.
	release func()
	// wire are the wire bytes relayed, kept for the receipt of a pull.
	wire atomic.Int64

	mu       sync.Mutex
	used     bool // the caller leg was opened
	finished bool
	timer    *time.Timer // expiry before the leg, receipt wait after it
}

// identityOf is the identity a caller leg and a receipt are bound to: the
// Worker of a Worker principal, the key of an API key, the user (and actor)
// of a browser or act-as session.
func identityOf(p authz.Principal) string {
	switch p.Kind {
	case authz.KindWorker:
		return "worker:" + p.WorkerID
	case authz.KindAPIKey:
		if p.KeyID != "" {
			return "key:" + p.KeyID
		}
		return "key-of:" + p.UserID
	case authz.KindActAs:
		return "actas:" + p.ActorID + ":" + p.UserID
	case authz.KindBrowserSession:
		return "user:" + p.UserID
	}
	return ""
}

// IdentityOf is the identity a caller leg of p must present to [Service.ServeLeg]
// and a receipt of p must come from ([Service.Receipt]).
func IdentityOf(p authz.Principal) string { return identityOf(p) }

// WorkerIdentity is the identity of a Worker's own legs and receipts.
func WorkerIdentity(workerID string) string { return "worker:" + workerID }

// principalLabel is the display form of the principal in the record.
func principalLabel(p authz.Principal) string {
	switch p.Kind {
	case authz.KindWorker:
		return "worker:" + p.WorkerID
	case authz.KindAPIKey:
		if p.KeyID != "" {
			return "key:" + p.KeyID
		}
		return "user:" + p.UserID
	case authz.KindActAs:
		return "user:" + p.ActorID
	}
	return "user:" + p.UserID
}

func tokenHash(tok string) string {
	sum := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(sum[:])
}

func (s *Service) get(id string) *transfer {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.live[id]
}

func (s *Service) forget(t *transfer) {
	s.mu.Lock()
	if s.live[t.id] == t {
		delete(s.live, t.id)
	}
	s.mu.Unlock()
	t.mu.Lock()
	if t.timer != nil {
		t.timer.Stop()
	}
	release := t.release
	t.release = nil
	t.mu.Unlock()
	if release != nil {
		release()
	}
}

// finish ends a transfer's record with out and records the audit end, once.
// The store keeps a final state, so a later finish of the same transfer
// (a receipt after a failure) changes nothing.
func (s *Service) finish(ctx context.Context, t *transfer, out store.FileOutcome) {
	t.mu.Lock()
	done := t.finished
	t.finished = true
	t.mu.Unlock()
	s.forget(t)
	if done {
		return
	}
	_, _, _ = s.finishID(ctx, t.id, out)
}

func (s *Service) finishID(ctx context.Context, id string, out store.FileOutcome) (store.FileTransferRow, bool, error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	row, changed, err := s.cfg.Store.FinishFileTransfer(ctx, id, out, s.nowMS())
	if err != nil {
		s.log.Error("files: the transfer record could not be finished", "transfer_id", id, "error", err.Error())
		return row, false, err
	}
	if changed {
		s.auditEnd(ctx, row)
	}
	return row, changed, nil
}

// failed is the outcome of a transfer that published nothing.
func failed(code proto.Code, wire int64) store.FileOutcome {
	return store.FileOutcome{State: ops.FileStateFailedNotSent, Reason: string(code), WireBytes: wire}
}

// codeOf is the proto code of err; internal for an error without one.
func codeOf(err error) proto.Code {
	var pe *proto.Error
	if errors.As(err, &pe) {
		return pe.Code
	}
	if errors.Is(err, context.Canceled) {
		return proto.CodeConnectionLost
	}
	return proto.CodeInternal
}
