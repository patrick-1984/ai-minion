//go:build linux && !android

package agent

import (
	"context"
	"encoding/json"

	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// maxMasterAncestry bounds the walk from a local caller up to the harness of
// its session.
const maxMasterAncestry = 64

// fileCall sends a file op (file_list, file_pull, file_push) of the calling
// session on the master lane (files.v1, WP12, BRD 5.6): the Hub serves a
// session caller's file op only for a verified source, the incarnation of a
// bound lease. It is the lane only when the session has a live attachment
// with a current lease, the Hub connection carries data legs, and the local
// caller (pid, from the kernel) is the harness of that pane or runs below it
// (an agent's tool, the CLI in its shell). Otherwise handled is false and
// the caller uses the ordinary call stream, where the Hub refuses file
// authority to the claimed source with a message that says what to do. The
// result is decoded into out.
func (b *masterBroker) fileCall(ctx context.Context, session string, pid int, op string, args any, out any) (handled bool, err error) {
	if pid <= 0 || session == "" {
		return false, nil
	}
	raw, err := json.Marshal(args)
	if err != nil {
		return false, nil
	}
	b.share.Lock()
	b.mu.Lock()
	var a *masterAttachment
	for _, c := range b.attachments {
		if c.terminal.SessionName == session && c.peer != nil {
			a = c
			break
		}
	}
	var lease *masterLease
	if a != nil {
		lease = b.currentLease(a)
	}
	if a == nil || lease == nil || !b.current.link.Welcome.FilesV1 || !descendsFrom(pid, a.harness.pid) {
		b.mu.Unlock()
		b.share.Unlock()
		return false, nil
	}
	if shared, err := b.policy.Shared(ctx, session); err != nil || !shared {
		b.mu.Unlock()
		b.share.Unlock()
		return true, proto.NewError(proto.CodeNotShared, "the calling session is not shared with the Hub")
	}
	link := b.current.link
	frame := proto.MasterCallRequest{Type: proto.TypeOpRequest, Op: proto.OpMasterCallV1, RequestID: ops.NewUUIDv7(),
		Args: proto.MasterCallArgs{IncarnationID: a.incarnation, Generation: lease.wire.Generation, LeaseID: lease.wire.LeaseID,
			Operation: op, Arguments: raw}}
	callCtx, cancel := context.WithDeadline(ctx, lease.until)
	stop := context.AfterFunc(lease.ctx, cancel)
	b.mu.Unlock()
	b.share.Unlock()
	defer cancel()
	defer stop()
	res, err := b.call(callCtx, link, frame)
	if err != nil {
		return true, err
	}
	if err := json.Unmarshal(res.Result, out); err != nil {
		return true, proto.NewError(proto.CodeInternal, "the Hub answered the file operation with an unreadable result")
	}
	return true, nil
}

// descendsFrom reports whether the process pid is ancestor or runs below it.
func descendsFrom(pid, ancestor int) bool {
	for range maxMasterAncestry {
		if pid == ancestor {
			return true
		}
		if pid <= 1 {
			return false
		}
		parent, _, err := masterProcessInfo(pid)
		if err != nil {
			return false
		}
		pid = parent
	}
	return false
}
