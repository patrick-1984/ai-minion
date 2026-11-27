package agent

// The local daemon as the caller end of a session caller's transfer (WP8,
// BRD FR-4, FR-10, FR-20): a file_pull or file_push on the local socket whose
// target is a session of another Worker. The root is the folder of the
// calling pane (its session must be shared, FR-10), the admission goes to
// the Hub on a call stream with the calling session as source_session, and
// the bytes move on an fx data leg of the same Hub connection. Progress is
// sent as op.progress elements ([FileProgress]); the answer is the final
// FileTransferResult. A file_list of another Worker is forwarded like the
// session-control reads (forwardsToHub).

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/patrick-1984/ai-minion/internal/filexfer"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/transport"
	"github.com/patrick-1984/ai-minion/internal/worker/store"
)

// legOpener is the part of the Hub session that opens a data leg
// (*transport.Session).
type legOpener interface {
	OpenLeg(ctx context.Context) (*transport.LegConn, error)
}

// isFileTransfer reports whether op is an admission the daemon runs as the
// caller end.
func isFileTransfer(op string) bool { return op == ops.OpFilePull || op == ops.OpFilePush }

// fileTarget reads the target of a file op's arguments.
func fileTarget(raw json.RawMessage) ops.Target {
	var a struct {
		Target ops.Target `json:"target"`
	}
	_ = json.Unmarshal(raw, &a)
	return a.Target
}

// forwardsFileList reports whether a local file_list names another Worker.
func forwardsFileList(op string, raw json.RawMessage, self string) bool {
	if op != ops.OpFileList {
		return false
	}
	t := fileTarget(raw)
	return t.WorkerID != "" && (self == "" || t.WorkerID != self)
}

// fileCall runs a local file_pull or file_push as the caller end.
func (l localExecutor) fileCall(ctx context.Context, rq Request, caller callerPane) (json.RawMessage, error) {
	op, ok := ops.Lookup(rq.Op)
	if !ok {
		return nil, apiErr(proto.CodeUnsupportedOp, "", "the Worker does not implement this operation")
	}
	if err := authorize(op.Actions, rq.Actions); err != nil {
		return nil, err
	}
	target := fileTarget(rq.Args)
	if self := l.ex.policy.WorkerID(); target.WorkerID == "" || target.WorkerID == self {
		return nil, apiErr(proto.CodeInvalidArgument, "", "a file transfer copies between two Workers: name the target Worker (worker:session); a transfer within one Worker is not in this version")
	}
	f := l.ex.Files()
	if f == nil {
		return nil, apiErr(proto.CodeFilesRootUnknown, "", "file transfer is not running on this Worker")
	}
	if err := f.requireAvailable(); err != nil {
		return nil, err
	}
	var cc *CallClient
	if l.calls != nil {
		cc, _ = l.calls().(*CallClient)
	}
	if cc == nil || !cc.Connected() {
		return nil, apiErr(proto.CodeWorkerOffline, "", "the target is on another Worker, which only the Hub can reach, and this Worker is not connected to a Hub")
	}
	session := l.ex.sourceSession(ctx, caller)
	if session == "" {
		return nil, apiErr(proto.CodeFilesRootUnknown, "", "the caller does not run in a session of this Worker, so it has no folder of its own; run the command inside a session")
	}
	fc := FileCaller{
		Allow: f.requireEffect,
		Admit: func(ctx context.Context, op string, args any) (ops.FileTransferResult, error) {
			var res ops.FileTransferResult
			if err := cc.Do(WithSourceSession(ctx, session), op, args, &res); err != nil {
				return res, forwardError(err)
			}
			return res, nil
		},
		Leg: func(ctx context.Context, adm ops.FileTransferResult) (*FileLeg, error) {
			link, err := cc.link()
			if err != nil {
				return nil, err
			}
			lo, ok := link.Session.(legOpener)
			if !ok {
				return nil, apiErr(proto.CodeUnsupportedOp, "", "the Hub connection carries no data legs")
			}
			conn, err := lo.OpenLeg(ctx)
			if err != nil {
				return nil, err
			}
			stop := context.AfterFunc(ctx, func() { conn.Stream().Reset() })
			return &FileLeg{
				Client: &http.Client{Transport: transport.NewLegTransport(conn)},
				URL:    transport.LegURL(proto.FilesLegPath(adm.TransferID)),
				Close:  func() { stop(); _ = conn.Close() },
			}, nil
		},
		Progress: func(p FileProgress) { _ = EmitProgress(ctx, p) },
	}
	if l.receipts != nil {
		fc.Receipt = l.receipts.Deliver
	} else {
		fc.Receipt = func(ctx context.Context, a ops.FileReceiptArgs) { _ = cc.Do(ctx, ops.OpFileReceipt, a, nil) }
	}
	var res ops.FileTransferResult
	err := l.withCallerRoot(ctx, f, session, func(root *filexfer.Root) error {
		fc.Root = root
		var err error
		if rq.Op == ops.OpFilePull {
			var a ops.FilePullArgs
			if err := decodeFileArgs(rq.Args, &a); err != nil {
				return err
			}
			res, err = CallerPull(ctx, fc, a)
		} else {
			var a ops.FilePushArgs
			if err := decodeFileArgs(rq.Args, &a); err != nil {
				return err
			}
			res, err = CallerPush(ctx, fc, a)
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	b, err := json.Marshal(res)
	if err != nil {
		return nil, internalErr("encode the result", err)
	}
	return b, nil
}

// decodeFileArgs decodes and validates the arguments of a local file op. The
// Hub's members are refused: a local caller cannot set them.
func decodeFileArgs(raw json.RawMessage, v ops.Validator) error {
	if err := json.Unmarshal(raw, v); err != nil {
		return apiErr(proto.CodeInvalidArgument, "", "the arguments of the file transfer cannot be read")
	}
	var leg ops.FileLegArgs
	switch a := v.(type) {
	case *ops.FilePullArgs:
		leg = a.FileLegArgs
	case *ops.FilePushArgs:
		leg = a.FileLegArgs
		a.Kind, a.Size, a.Entries = "", 0, 0
	}
	if leg != (ops.FileLegArgs{}) {
		return apiErr(proto.CodeInvalidArgument, "", "transfer_id, leg_token, max_bytes and max_entries are set by the Hub")
	}
	if err := v.Validate(); err != nil {
		return err
	}
	return nil
}

// withCallerRoot pins the folder of the calling session's pane (FR-4, FR-2,
// FR-3) under the share rule (not_shared when the session is not shared) and
// runs fn with it.
func (l localExecutor) withCallerRoot(ctx context.Context, f *Files, session string, fn func(*filexfer.Root) error) error {
	var made []*filexfer.Root
	root, err := readStable(l.ex, ctx, session, func(store.ShareState) (*filexfer.Root, error) {
		r, _, err := f.openRoot(ctx, ops.Target{SessionName: session})
		if r != nil {
			made = append(made, r)
		}
		return r, err
	})
	for _, m := range made {
		if m != root || err != nil {
			_ = m.Close()
		}
	}
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	return fn(root)
}
