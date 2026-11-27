package files

import (
	"context"
	"crypto/subtle"
	"errors"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// awaitReceipt waits for the receipt of a pull whose bytes and trailers the
// receiver got: without one in time the record is unknown (the receiver may
// have published); a late receipt still resolves it (the store moves unknown
// to the real outcome once).
func (s *Service) awaitReceipt(t *transfer) {
	wait := dur(s.cfg.ReceiptWait, 2*time.Minute)
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.finished {
		return
	}
	t.timer = time.AfterFunc(wait, func() {
		s.finish(context.Background(), t, store.FileOutcome{State: ops.FileStateUnknown, WireBytes: t.wire.Load()})
	})
}

// Receipt takes the receiver's terminal receipt of a pull (file_receipt).
// receiver is the identity the call authenticated as ([IdentityOf] of its
// principal): it must be the receiving leg's identity of the record, and the
// leg token must be that leg's token (checked against the stored hash, so a
// receipt is taken after a Hub restart too). Anything else is forbidden and
// changes nothing; an unknown transfer is not_found. A repeated receipt is
// acknowledged with the state the record reached.
func (s *Service) Receipt(ctx context.Context, receiver string, args ops.FileReceiptArgs) (ops.FileReceiptResult, error) {
	if err := args.Validate(); err != nil {
		return ops.FileReceiptResult{}, err
	}
	row, err := s.cfg.Store.FileTransfer(ctx, args.TransferID)
	if errors.Is(err, store.ErrNotFound) {
		return ops.FileReceiptResult{}, proto.NewError(proto.CodeNotFound, "not found")
	}
	if err != nil {
		return ops.FileReceiptResult{}, err
	}
	hash := tokenHash(args.LegToken)
	if receiver == "" || row.Receiver != receiver || row.ReceiptHash == "" ||
		subtle.ConstantTimeCompare([]byte(hash), []byte(row.ReceiptHash)) != 1 {
		s.record(ctx, row, AuditDenied)
		return ops.FileReceiptResult{}, proto.NewError(proto.CodeForbidden, "this receipt is not from the receiving end of the transfer")
	}
	if row.Direction != ops.FileDirectionPull {
		return ops.FileReceiptResult{}, proto.NewError(proto.CodeInvalidArgument, "a push is acknowledged by its target on the data leg")
	}
	out := receiptOutcome(args.FileReceipt)
	out.WireBytes = row.WireBytes
	if t := s.get(args.TransferID); t != nil {
		out.WireBytes = t.wire.Load()
		t.mu.Lock()
		used := t.used
		t.mu.Unlock()
		if !used {
			// No leg was opened: nothing can have been received.
			return ops.FileReceiptResult{}, proto.NewError(proto.CodeConflict, "the transfer has not moved any bytes yet")
		}
		t.mu.Lock()
		t.finished = true
		t.mu.Unlock()
		s.forget(t)
	}
	got, _, err := s.finishID(ctx, args.TransferID, out)
	if err != nil {
		return ops.FileReceiptResult{}, err
	}
	return ops.FileReceiptResult{TransferID: got.ID, State: got.State}, nil
}

// ReceiptFor is [Service.Receipt] for a principal: the REST file_receipt of
// a CLI with an API key, or a Worker daemon's receipt on its call stream.
func (s *Service) ReceiptFor(ctx context.Context, p authz.Principal, args ops.FileReceiptArgs) (ops.FileReceiptResult, error) {
	return s.Receipt(ctx, identityOf(p), args)
}
