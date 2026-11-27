package files

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/hub/workers"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

var (
	_ workers.FileAdmitter   = (*Service)(nil)
	_ workers.FileAuthorizer = (*Service)(nil)
)

// request is a caller's file_pull or file_push, decoded.
type request struct {
	direction string
	pull      ops.FilePullArgs
	push      ops.FilePushArgs
	leg       ops.FileLegArgs
	path      string // the path on the target (pull: Path, push: As)
	kind      string // declared by a push
	size      int64
}

func decodeRequest(op string, args any) (request, error) {
	var r request
	switch op {
	case ops.OpFilePull:
		r.direction = ops.FileDirectionPull
		switch a := args.(type) {
		case ops.FilePullArgs:
			r.pull = a
		case *ops.FilePullArgs:
			if a == nil {
				return r, proto.NewError(proto.CodeInvalidArgument, "file_pull needs arguments")
			}
			r.pull = *a
		default:
			return r, proto.NewError(proto.CodeInternal, "file_pull was routed with arguments of another type")
		}
		r.leg, r.path = r.pull.FileLegArgs, r.pull.Path
		if err := ops.CheckFilePath(r.path, false); err != nil {
			return r, err
		}
	case ops.OpFilePush:
		r.direction = ops.FileDirectionPush
		switch a := args.(type) {
		case ops.FilePushArgs:
			r.push = a
		case *ops.FilePushArgs:
			if a == nil {
				return r, proto.NewError(proto.CodeInvalidArgument, "file_push needs arguments")
			}
			r.push = *a
		default:
			return r, proto.NewError(proto.CodeInternal, "file_push was routed with arguments of another type")
		}
		r.leg, r.path, r.kind, r.size = r.push.FileLegArgs, r.push.As, r.push.Kind, r.push.Size
		if r.path == "" {
			return r, proto.NewError(proto.CodeInvalidArgument, "file_push needs the destination (as) on the target")
		}
		if err := ops.CheckFilePath(r.path, false); err != nil {
			return r, err
		}
		if r.kind != ops.FileKindFile && r.kind != ops.FileKindDir {
			return r, proto.NewError(proto.CodeInvalidArgument, "file_push needs the declared kind (file or dir)")
		}
	default:
		return r, proto.NewError(proto.CodeUnsupportedOp, "not a file transfer admission")
	}
	// The Hub's members are the Hub's: a caller that sets them is refused,
	// so that a forged transfer id, token or limit never reaches a Worker.
	if r.leg != (ops.FileLegArgs{}) {
		return r, proto.NewError(proto.CodeInvalidArgument, "transfer_id, leg_token, max_bytes and max_entries are set by the Hub")
	}
	return r, nil
}

// withLeg returns the caller's arguments with the Hub's members set.
func (r request) withLeg(leg ops.FileLegArgs) any {
	if r.direction == ops.FileDirectionPull {
		a := r.pull
		a.FileLegArgs = leg
		return a
	}
	a := r.push
	a.FileLegArgs = leg
	return a
}

// AdmitFile admits a file_pull or file_push (workers.FileAdmitter): it
// decides both ends, applies the limits, persists the record before anything
// is sent to the target and returns the request the Router sends, with the
// Hub's members set. A refusal is recorded (state denied) and audited.
func (s *Service) AdmitFile(ctx context.Context, c workers.FileCall) (workers.FileDispatch, error) {
	req, err := decodeRequest(c.Op, c.Args)
	if err != nil {
		return workers.FileDispatch{}, err
	}
	if c.WorkerID == "" || c.Session == "" {
		return workers.FileDispatch{}, proto.NewError(proto.CodeInvalidArgument, "a transfer needs a target session")
	}
	t := &transfer{
		id: ops.NewUUIDv7(), direction: req.direction, kind: req.kind, path: req.path,
		targetWorker: c.WorkerID, targetSession: c.Session,
		callerID:  identityOf(c.Principal),
		principal: c.Principal, verified: c.CallerSession, bound: c.Bound, refresh: c.Refresh,
		web:  c.Principal.Kind == authz.KindBrowserSession || c.Principal.Kind == authz.KindActAs,
		size: req.size,
	}
	if c.Principal.Kind == authz.KindWorker {
		t.callerWorker, t.callerSession = c.Principal.WorkerID, c.CallerSession
		t.callerConn = s.cfg.Conn(t.callerWorker)
	}
	row := s.baseRow(t, c.Principal)

	p, err := s.fileAuthority(ctx, c.Principal, c.CallerSession, c.Bound)
	if err != nil {
		return workers.FileDispatch{}, s.deny(ctx, row, err)
	}
	if t.callerID == "" {
		return workers.FileDispatch{}, s.deny(ctx, row, proto.NewError(proto.CodeForbidden, "not allowed"))
	}
	d, err := s.decideEnds(ctx, p, t.direction, t.targetWorker, t.targetSession, t.callerWorker, t.callerSession)
	if err != nil {
		return workers.FileDispatch{}, s.deny(ctx, row, err)
	}
	row.TargetOwnerID, row.GrantIDs, row.AcceptanceID = d.owner, d.basis.GrantIDs, d.basis.AcceptanceID
	if req.direction == ops.FileDirectionPush && req.size > s.limits.MaxBytes {
		return workers.FileDispatch{}, s.deny(ctx, row, proto.NewError(proto.CodeTransferTooLarge, "the transfer is larger than the Hub allows"))
	}
	if t.callerWorker != "" && t.callerConn == nil {
		return workers.FileDispatch{}, s.deny(ctx, row, proto.NewError(proto.CodeWorkerOffline, "the calling Worker is not connected"))
	}

	if t.callerToken, err = proto.NewLegToken(); err != nil {
		return workers.FileDispatch{}, s.deny(ctx, row, fmt.Errorf("files: leg token: %w", err))
	}
	if t.targetToken, err = proto.NewLegToken(); err != nil {
		return workers.FileDispatch{}, s.deny(ctx, row, fmt.Errorf("files: leg token: %w", err))
	}
	release, err := s.take(t, c.Principal)
	if err != nil {
		return workers.FileDispatch{}, s.deny(ctx, row, err)
	}

	// The durable record comes before any byte moves (BRD 5.9): an insert
	// that fails refuses the transfer.
	now := s.now()
	deadline := now.Add(proto.LegOpenWindow).UnixMilli()
	row.State, row.CreatedAt, row.DeadlineAt = ops.FileStateAdmitted, now.UnixMilli(), &deadline
	if req.direction == ops.FileDirectionPull {
		row.Receiver, row.ReceiptHash = t.callerID, tokenHash(t.callerToken)
	} else {
		row.Receiver, row.ReceiptHash = WorkerIdentity(t.targetWorker), tokenHash(t.targetToken)
	}
	if err := s.cfg.Store.InsertFileTransfer(ctx, row); err != nil {
		release()
		s.log.Error("files: the transfer record could not be stored; the transfer is refused", "error", err.Error())
		return workers.FileDispatch{}, proto.NewError(proto.CodeInternal, "the transfer could not be recorded; nothing was sent")
	}
	s.record(ctx, row, AuditAdmitted)

	leg := ops.FileLegArgs{TransferID: t.id, LegToken: t.targetToken, MaxBytes: s.limits.MaxBytes, MaxEntries: s.limits.MaxEntries}
	return workers.FileDispatch{
		Principal: p,
		Args:      req.withLeg(leg),
		Reprincipal: func(ctx context.Context, fresh authz.Principal) (authz.Principal, error) {
			return s.fileAuthority(ctx, fresh, c.CallerSession, c.Bound)
		},
		Finish: func(ctx context.Context, res any, err error) (any, error) {
			return s.prepared(ctx, t, release, res, err)
		},
	}, nil
}

// baseRow is the record of a transfer as far as the call names it.
func (s *Service) baseRow(t *transfer, p authz.Principal) store.FileTransferRow {
	r := store.FileTransferRow{
		ID: t.id, Direction: t.direction, Principal: principalLabel(p), PrincipalUserID: p.UserID,
		SourceWorkerID: t.callerWorker, SourceSession: t.callerSession,
		TargetWorkerID: t.targetWorker, TargetSession: t.targetSession,
		Path: t.path, Kind: t.kind,
	}
	if p.Kind == authz.KindActAs {
		r.PrincipalUserID, r.ActingAs = p.ActorID, p.UserID
	}
	if pol := s.policy(); pol != "" {
		r.Policy = "hub=" + pol
	}
	return r
}

func (s *Service) policy() string {
	if s.cfg.Policy == nil {
		return ops.FileModeOn
	}
	if p := s.cfg.Policy(); p != "" {
		return p
	}
	return ops.FileModeOn
}

// deny records and audits a refused admission and returns err. A refusal
// whose record cannot be stored is still a refusal.
func (s *Service) deny(ctx context.Context, row store.FileTransferRow, err error) error {
	row.State, row.Reason = ops.FileStateDenied, string(codeOf(err))
	if codeOf(err) == proto.CodeInternal {
		s.log.Error("files: an admission failed", "error", err.Error())
		err = proto.NewError(proto.CodeInternal, "the transfer could not be admitted")
	}
	if ierr := s.cfg.Store.InsertFileTransfer(context.WithoutCancel(ctx), row); ierr != nil && !errors.Is(ierr, store.ErrFileTransferExists) {
		s.log.Warn("files: the record of a refused transfer could not be stored", "error", ierr.Error())
	}
	s.record(ctx, row, AuditDenied)
	return err
}

// prepared handles the target's answer to the admission: a refusal or a lost
// answer ends the record failed_not_sent (no data leg was opened, so nothing
// moved); an answer moves it to prepared and gives the caller its leg.
func (s *Service) prepared(ctx context.Context, t *transfer, release func(), res any, err error) (any, error) {
	t.release = release
	fail := func(code proto.Code) {
		s.finish(ctx, t, failed(code, 0))
	}
	if err != nil {
		var unknown *workers.OutcomeUnknownError
		if errors.As(err, &unknown) {
			fail(proto.CodeConnectionLost)
			return nil, unknown.Err
		}
		fail(codeOf(err))
		return nil, err
	}
	ans, ok := res.(ops.FileTransferResult)
	if !ok || ans.Stage != ops.FileStageAdmitted || (ans.TransferID != "" && ans.TransferID != t.id) ||
		ans.Kind != ops.FileKindFile && ans.Kind != ops.FileKindDir || ans.Direction != "" && ans.Direction != t.direction {
		fail(proto.CodeInternal)
		return nil, proto.NewError(proto.CodeInternal, "the Worker answered the admission with an unusable result")
	}
	limits := s.limits
	if ans.Limits != nil {
		limits = limits.Lower(*ans.Limits)
	}
	if t.direction == ops.FileDirectionPull && ans.Kind == ops.FileKindFile && ans.Size > limits.MaxBytes {
		fail(proto.CodeTransferTooLarge)
		return nil, proto.NewError(proto.CodeTransferTooLarge, "the file is larger than the transfer limit")
	}
	t.kind, t.limits = ans.Kind, limits
	t.targetConn = s.cfg.Conn(t.targetWorker)
	if t.targetConn == nil {
		fail(proto.CodeWorkerOffline)
		return nil, proto.NewError(proto.CodeWorkerOffline, "the target Worker is not connected")
	}
	now := s.now()
	t.expires = now.Add(proto.LegOpenWindow)
	deadline := now.Add(dur(s.cfg.PreparedDeadline, 12*time.Hour)).UnixMilli()
	size := ans.Size
	if t.direction == ops.FileDirectionPush {
		size = t.size
	}
	if err := s.cfg.Store.MarkFileTransferPrepared(ctx, t.id, store.FilePrepared{
		TargetRoot: ans.TargetRoot, Kind: ans.Kind, Bytes: size, DeadlineAt: &deadline,
	}, now.UnixMilli()); err != nil {
		s.log.Error("files: the transfer record could not be updated", "transfer_id", t.id, "error", err.Error())
		fail(proto.CodeInternal)
		return nil, proto.NewError(proto.CodeInternal, "the transfer could not be recorded")
	}
	s.mu.Lock()
	s.live[t.id] = t
	s.mu.Unlock()
	t.mu.Lock()
	t.timer = time.AfterFunc(time.Until(t.expires)+time.Second, func() { s.expire(t) })
	t.mu.Unlock()

	exp := ops.Timestamp(t.expires)
	lim := limits
	return ops.FileTransferResult{
		Stage: ops.FileStageAdmitted, TransferID: t.id, Direction: t.direction,
		TargetRoot: ans.TargetRoot, Kind: ans.Kind, Size: ans.Size, ETag: ans.ETag,
		Encodings: ans.Encodings, Limits: &lim, ExpiresAt: &exp,
		LegToken: t.callerToken, LegPath: proto.FilesLegPath(t.id),
	}, nil
}

// expire ends a transfer whose caller leg did not open in time.
func (s *Service) expire(t *transfer) {
	t.mu.Lock()
	used := t.used
	t.mu.Unlock()
	if used {
		return
	}
	s.finish(context.Background(), t, failed(proto.CodeTransferExpired, 0))
}
