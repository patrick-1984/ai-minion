package files

import (
	"context"
	"errors"
	"fmt"

	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// decision is what a decision of both ends found.
type decision struct {
	owner string
	basis authz.FileBasis
}

// decideEnds decides a transfer of direction by p on the target session and
// applies the Hub policies of both ends: the action on the target
// (authz.Authorizer.DecideFile), the Hub-wide policy and each end's
// per-Worker override and preview_only (authz.CheckFileEnds), and the share
// of the caller's own session. It is the decision of the admission, of the
// re-decisions and of the commit gate. A lookup that fails refuses (fail
// closed, internal).
func (s *Service) decideEnds(ctx context.Context, p authz.Principal, direction, targetWorker, targetSession, callerWorker, callerSession string) (decision, error) {
	owner, basis, err := s.cfg.Authz.DecideFile(ctx, p, direction, authz.SessionResource{WorkerID: targetWorker, Name: targetSession})
	if err != nil {
		return decision{}, err
	}
	admin := false
	if s.cfg.IsAdmin != nil && p.Kind != authz.KindWorker {
		if admin, err = s.cfg.IsAdmin(ctx, p); err != nil {
			return decision{}, fmt.Errorf("files: read the caller's role: %w", err)
		}
	}
	target, err := s.end(ctx, targetWorker)
	if err != nil {
		return decision{}, err
	}
	var caller authz.FileEnd
	if callerWorker != "" {
		if caller, err = s.end(ctx, callerWorker); err != nil {
			return decision{}, err
		}
	}
	policy := ""
	if s.cfg.Policy != nil {
		policy = s.cfg.Policy()
	}
	if err := authz.CheckFileEnds(policy, direction, admin, target, caller); err != nil {
		return decision{}, err
	}
	if callerWorker != "" {
		if callerSession == "" {
			return decision{}, proto.NewError(proto.CodeForbidden, "a session caller needs a verified calling session")
		}
		if s.cfg.SessionShared != nil {
			ok, err := s.cfg.SessionShared(ctx, callerWorker, callerSession)
			if err != nil {
				return decision{}, fmt.Errorf("files: read the share of the calling session: %w", err)
			}
			if !ok {
				return decision{}, proto.NewError(proto.CodeNotShared, "the calling session is not shared with the Hub")
			}
		}
	}
	return decision{owner: owner, basis: basis}, nil
}

// end is the Hub's view of one Worker end.
func (s *Service) end(ctx context.Context, workerID string) (authz.FileEnd, error) {
	mode, err := s.cfg.Store.WorkerFileMode(ctx, workerID)
	if errors.Is(err, store.ErrWorkerNotFound) {
		return authz.FileEnd{}, proto.NewError(proto.CodeNotFound, "not found")
	}
	if err != nil {
		return authz.FileEnd{}, fmt.Errorf("files: read the Worker's file transfer mode: %w", err)
	}
	preview, err := s.cfg.Store.WorkerPreviewOnly(ctx, workerID)
	if err != nil && !errors.Is(err, store.ErrWorkerNotFound) {
		return authz.FileEnd{}, fmt.Errorf("files: read the Worker's preview_only: %w", err)
	}
	return authz.FileEnd{WorkerID: workerID, Mode: mode, PreviewOnly: preview}, nil
}

// fileAuthority returns p with the verified file authority of its calling
// session: the master acceptance rows with file flags that the target
// Workers have for p's Worker, read now. A principal without a verified
// session is returned as it is (it holds no file action through flags).
func (s *Service) fileAuthority(ctx context.Context, p authz.Principal, verified string) (authz.Principal, error) {
	if p.Kind != authz.KindWorker || verified == "" {
		return p, nil
	}
	rows, err := s.cfg.Store.FileAcceptances(ctx, p.WorkerID)
	if err != nil {
		return p, fmt.Errorf("files: read the master acceptances: %w", err)
	}
	accepts := make([]authz.FileAcceptance, 0, len(rows))
	for _, r := range rows {
		accepts = append(accepts, authz.FileAcceptance{
			ID:             store.AcceptanceID(r.TargetWorkerID, r.SourceWorkerID),
			TargetWorkerID: r.TargetWorkerID, Sessions: r.Sessions,
			FilesPull: r.FilesPull != nil && *r.FilesPull, FilesPush: r.FilesPush != nil && *r.FilesPush,
		})
	}
	return p.WithVerifiedFiles(verified, accepts), nil
}

// redecide decides a live transfer again on the principal it was admitted
// with, with its file authority read now (grants on a Worker principal are
// the ones of its admission path; a path that can revoke them ends the call
// itself, see workers.MasterControl).
func (s *Service) redecide(ctx context.Context, t *transfer) error {
	p, err := s.fileAuthority(ctx, t.principal, t.verified)
	if err != nil {
		return err
	}
	_, err = s.decideEnds(ctx, p, t.direction, t.targetWorker, t.targetSession, t.callerWorker, t.callerSession)
	return err
}
