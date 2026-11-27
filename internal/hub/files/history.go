package files

import (
	"context"
	"errors"
	"fmt"

	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// History answers file_transfers_list for p: the records where p's user
// owned the source or target Worker or is the principal's user, every record
// for an administrator acting as themselves. A Worker identity has no
// history. Entries are included only when one transfer is selected.
func (s *Service) History(ctx context.Context, p authz.Principal, args ops.FileTransfersListArgs) (ops.FileTransfersListResult, error) {
	if err := args.Validate(); err != nil {
		return ops.FileTransfersListResult{}, err
	}
	if p.UserID == "" || p.Kind == authz.KindWorker {
		return ops.FileTransfersListResult{}, proto.NewError(proto.CodeForbidden, "not allowed")
	}
	admin := false
	if s.cfg.IsAdmin != nil && p.Kind != authz.KindActAs {
		var err error
		if admin, err = s.cfg.IsAdmin(ctx, p); err != nil {
			return ops.FileTransfersListResult{}, fmt.Errorf("files: read the caller's role: %w", err)
		}
	}
	rows, next, err := s.cfg.Store.ListFileTransfers(ctx, store.FileTransferFilter{
		ViewerID: p.UserID, Admin: admin, WorkerID: args.WorkerID, TransferID: args.TransferID,
		Cursor: args.Cursor, Limit: args.EffectiveLimit(),
	})
	if errors.Is(err, store.ErrFileCursor) {
		return ops.FileTransfersListResult{}, proto.NewError(proto.CodeInvalidArgument, "the cursor is not a cursor of file_transfers_list")
	}
	if err != nil {
		return ops.FileTransfersListResult{}, err
	}
	out := ops.FileTransfersListResult{NextCursor: next, Transfers: make([]ops.FileTransferRecord, 0, len(rows))}
	for _, r := range rows {
		var entries []ops.FileTransferEntry
		if args.TransferID != "" {
			if entries, err = s.cfg.Store.FileTransferEntries(ctx, r.ID); err != nil {
				return ops.FileTransfersListResult{}, err
			}
		}
		out.Transfers = append(out.Transfers, r.Record(entries))
	}
	return out, nil
}
