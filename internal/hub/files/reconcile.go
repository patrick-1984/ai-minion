package files

import (
	"context"
	"math"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// Recover ends, at Hub start, every record that a previous process left
// open: its legs ended with that process, so each one is unknown (a late
// receipt of the receiver still resolves it). It must run before the service
// admits transfers. It returns the ids it changed.
func (s *Service) Recover(ctx context.Context) ([]string, error) {
	ids, err := s.cfg.Store.ReconcileFileTransfers(ctx, math.MaxInt64)
	s.auditReconciled(ctx, ids)
	return ids, err
}

// Reconcile turns the open records past their deadline into unknown and
// purges the records older than the retention. Live transfers of this
// process have a deadline far ahead and are not touched.
func (s *Service) Reconcile(ctx context.Context) ([]string, error) {
	now := s.now()
	ids, err := s.cfg.Store.ReconcileFileTransfers(ctx, now.UnixMilli())
	if err != nil {
		return nil, err
	}
	s.auditReconciled(ctx, ids)
	keep := store.DefaultFileTransferRetention
	if s.cfg.Retention != nil {
		if d := s.cfg.Retention(); d > 0 {
			keep = d
		}
	}
	if _, err := s.cfg.Store.PurgeFileTransfers(ctx, now.Add(-keep).UnixMilli()); err != nil {
		return ids, err
	}
	return ids, nil
}

func (s *Service) auditReconciled(ctx context.Context, ids []string) {
	for _, id := range ids {
		if row, err := s.cfg.Store.FileTransfer(ctx, id); err == nil {
			s.auditEnd(ctx, row)
		}
	}
}

// Run recovers the records of the previous process and then reconciles
// every interval (0: proto.ReconcileInterval) until ctx ends.
func (s *Service) Run(ctx context.Context, interval time.Duration) {
	if _, err := s.Recover(ctx); err != nil && ctx.Err() == nil {
		s.log.Error("files: the open transfer records could not be recovered", "error", err.Error())
	}
	tk := time.NewTicker(dur(interval, proto.ReconcileInterval))
	defer tk.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tk.C:
		}
		if _, err := s.Reconcile(ctx); err != nil && ctx.Err() == nil {
			s.log.Error("files: transfer reconciliation failed", "error", err.Error())
		}
	}
}
