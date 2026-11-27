package agent

// The receipt outbox of a receiving daemon (files.v1, BRD FR-15).
//
// Only the receiver acknowledges the commit of a pull: after it published,
// the daemon sends file_receipt with the caller leg's token. A connection that
// ends between the publish and the Hub's answer would leave the Hub's record
// unknown forever, so every receipt is written to the outbox in the state
// directory before it is sent, and removed only when the Hub answered it
// (acknowledged, or refused for good). The outbox is sent again on every new
// Hub connection and every outboxRetry while receipts wait, also after a
// daemon restart, until each is acknowledged or proto.ReceiptOutboxTTL old.
//
// The file holds transfer ids, leg tokens and outcomes, never a path or a
// name; it is private to the Worker user (0600 in the 0700 state directory).

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// ReceiptOutboxFile is the outbox in the Worker state directory.
const ReceiptOutboxFile = "file-receipts.json"

// maxOutbox bounds the receipts kept; the oldest go first.
const maxOutbox = 1000

// outboxRetry is the period of resends while receipts wait on a connection.
const outboxRetry = 30 * time.Second

// outboxEntry is one receipt waiting for the Hub.
type outboxEntry struct {
	Args ops.FileReceiptArgs `json:"args"`
	At   time.Time           `json:"at"`
}

// ReceiptOutbox keeps the unacknowledged receipts of this Worker's pulls.
// It is safe for concurrent use.
type ReceiptOutbox struct {
	path string
	send func(ctx context.Context, a ops.FileReceiptArgs) error
	log  *slog.Logger
	now  func() time.Time

	mu      sync.Mutex
	flushMu sync.Mutex
}

// NewReceiptOutbox returns the outbox in stateDir ("" keeps none: receipts
// are sent once). send delivers one receipt to the Hub.
func NewReceiptOutbox(stateDir string, send func(ctx context.Context, a ops.FileReceiptArgs) error, log *slog.Logger) *ReceiptOutbox {
	o := &ReceiptOutbox{send: send, log: log, now: time.Now}
	if stateDir != "" {
		o.path = filepath.Join(stateDir, ReceiptOutboxFile)
	}
	if o.log == nil {
		o.log = slog.New(slog.DiscardHandler)
	}
	return o
}

func (o *ReceiptOutbox) load() []outboxEntry {
	if o.path == "" {
		return nil
	}
	b, err := os.ReadFile(o.path)
	if err != nil {
		return nil
	}
	var list []outboxEntry
	if json.Unmarshal(b, &list) != nil {
		return nil
	}
	return list
}

func (o *ReceiptOutbox) save(list []outboxEntry) error {
	if o.path == "" {
		return nil
	}
	if len(list) == 0 {
		if err := os.Remove(o.path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	b, err := json.Marshal(list)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(o.path), ".file-receipts-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, o.path)
}

// update changes the stored list under the lock.
func (o *ReceiptOutbox) update(f func([]outboxEntry) []outboxEntry) {
	o.mu.Lock()
	defer o.mu.Unlock()
	list := f(o.load())
	if len(list) > maxOutbox {
		list = list[len(list)-maxOutbox:]
	}
	if err := o.save(list); err != nil {
		o.log.Warn("cannot write the file receipt outbox", "error", err.Error())
	}
}

func (o *ReceiptOutbox) remove(id string) {
	o.update(func(list []outboxEntry) []outboxEntry {
		out := list[:0]
		for _, e := range list {
			if e.Args.TransferID != id {
				out = append(out, e)
			}
		}
		return out
	})
}

// Pending returns the number of receipts waiting.
func (o *ReceiptOutbox) Pending() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.load())
}

// retryable reports whether a failed send is to be repeated: the Hub was not
// reached or could not answer now. Any other answer is final (a receipt the
// Hub refuses would be refused again).
func retryable(err error) bool {
	switch proto.CodeOf(err) {
	case proto.CodeWorkerOffline, proto.CodeConnectionLost, proto.CodeInternal, proto.CodeRateLimited,
		proto.CodePaused, proto.CodeWorkerPending, proto.CodeUnsupportedOp, "":
		return true
	}
	return false
}

// Deliver stores the receipt a and sends it once now. A receipt the Hub has
// not acknowledged stays for [ReceiptOutbox.Flush].
func (o *ReceiptOutbox) Deliver(ctx context.Context, a ops.FileReceiptArgs) {
	o.update(func(list []outboxEntry) []outboxEntry {
		for _, e := range list {
			if e.Args.TransferID == a.TransferID {
				return list
			}
		}
		return append(list, outboxEntry{Args: a, At: o.now()})
	})
	o.try(ctx, a)
}

// try sends one receipt and forgets it unless it is to be sent again.
func (o *ReceiptOutbox) try(ctx context.Context, a ops.FileReceiptArgs) bool {
	if o.send == nil {
		return false
	}
	err := o.send(ctx, a)
	if err != nil && retryable(err) {
		return false
	}
	if err != nil {
		o.log.Info("the Hub refused a file receipt", "transfer", a.TransferID, "code", string(proto.CodeOf(err)))
	}
	o.remove(a.TransferID)
	return true
}

// Flush sends every waiting receipt and drops those older than
// proto.ReceiptOutboxTTL. It returns the number still waiting.
func (o *ReceiptOutbox) Flush(ctx context.Context) int {
	o.flushMu.Lock()
	defer o.flushMu.Unlock()
	now := o.now()
	var due []ops.FileReceiptArgs
	o.update(func(list []outboxEntry) []outboxEntry {
		out := list[:0]
		for _, e := range list {
			if now.Sub(e.At) <= proto.ReceiptOutboxTTL {
				out = append(out, e)
				due = append(due, e.Args)
			}
		}
		return out
	})
	left := 0
	for _, a := range due {
		if ctx.Err() != nil {
			return len(due)
		}
		if !o.try(ctx, a) {
			left++
		}
	}
	return left
}

// Serve resends the outbox on a new Hub connection, and every outboxRetry
// while receipts wait, until the link ends (a [LinkHandler]).
func (o *ReceiptOutbox) Serve(l *Link) {
	ctx := l.Context()
	t := time.NewTicker(outboxRetry)
	defer t.Stop()
	for {
		if o.Pending() > 0 {
			o.Flush(ctx)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
