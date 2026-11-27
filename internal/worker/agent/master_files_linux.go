//go:build linux && !android

package agent

import (
	"context"
	"encoding/json"
	"time"

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
	return b.laneCall(ctx, session, pid, op, args, out, masterLaneFiles)
}

// appCall sends app_install or app_install_status of the calling session
// (v0.25.0) on the master lane under the same conditions as [fileCall],
// except that the Hub must route app installs on the lane (welcome
// app_install_v1) instead of carrying data legs. The Hub decides the call on
// the apps_install flag of the verified grants and acceptances and requires
// app.install.v1 of the target Worker; a claimed source never installs.
func (b *masterBroker) appCall(ctx context.Context, session string, pid int, op string, args any, out any) (handled bool, err error) {
	return b.laneCall(ctx, session, pid, op, args, out, masterLaneApps)
}

// masterLaneKind is the kind of op [masterBroker.laneCall] sends.
type masterLaneKind int

const (
	masterLaneFiles masterLaneKind = iota
	masterLaneApps
)

// masterAppCallTimeout bounds an app install call on the lane: the Hub waits
// up to two minutes for the Android Worker's answer, longer than one lease
// period. The end of the lease still ends the call.
const masterAppCallTimeout = 2*time.Minute + 15*time.Second

// laneServes reports whether the Hub of the lane serves ops of kind there.
func laneServes(w HubWelcome, kind masterLaneKind) bool {
	if kind == masterLaneApps {
		return w.AppInstallV1
	}
	return w.FilesV1
}

// laneCall is [fileCall] and [appCall].
func (b *masterBroker) laneCall(ctx context.Context, session string, pid int, op string, args any, out any, kind masterLaneKind) (handled bool, err error) {
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
	if a == nil || lease == nil || !laneServes(b.current.link.Welcome, kind) || !descendsFrom(pid, a.harness.pid) {
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
	deadline := lease.until
	if kind == masterLaneApps {
		deadline = time.Now().Add(masterAppCallTimeout)
	}
	callCtx, cancel := context.WithDeadline(ctx, deadline)
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
		return true, proto.NewError(proto.CodeInternal, "the Hub answered "+op+" with an unreadable result")
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
