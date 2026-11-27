package server

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sync"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/files"
	"github.com/patrick-1984/ai-minion/internal/hub/workers"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/transport"
)

// The Worker-session caller of a file transfer (files.v1, WP12, BRD 5.6):
//
//   - file_list, file_pull, file_push, app_install and app_install_status of a
//     session caller are served on its call stream
//     (workers.RegisterSessionControlCalls) and on the master lane, decided
//     by the same rule: a grant or an applicable master acceptance that
//     covers this caller, this target and this action;
//   - file_receipt of a Worker that received a pull is served on its call
//     stream, bound to the Worker the handshake authenticated and the leg
//     token (files.Service.Receipt);
//   - the caller leg a Worker opens on its connection (an inbound fx stream)
//     is served by files.Service.ServeWorkerLeg, bound to that connection.

// fileCallTimeout bounds the work of a file call stream op.
const fileCallTimeout = 10 * time.Second

// registerFileCalls registers the call stream ops of the file transfer and
// the handler of the Workers' data legs.
func registerFileCalls(svc *workers.Service, fs *files.Service) error {
	if !svc.HandleCallOp(ops.OpFileReceipt, func(ctx context.Context, c workers.Call) {
		ctx, cancel := context.WithTimeout(ctx, fileCallTimeout)
		defer cancel()
		var args ops.FileReceiptArgs
		if err := json.Unmarshal(c.Args, &args); err != nil {
			replyCall(c.Stream, nil, proto.NewError(proto.CodeInvalidArgument, "invalid file_receipt arguments"))
			return
		}
		res, err := fs.Receipt(ctx, files.WorkerIdentity(c.Worker.ID), args)
		replyCall(c.Stream, res, err)
	}) {
		return errFileCallTaken(ops.OpFileReceipt)
	}
	svc.Registry().OnAdd(func(info workers.Online, sess workers.OpSession) {
		if info.Pending || !info.HasFeature(proto.FeatureFilesV1) {
			return
		}
		host, ok := workers.HostOf(sess)
		if !ok {
			return
		}
		host.Handle(transport.KindFiles, &workerLegStreams{fs: fs, workerID: info.WorkerID, conn: slices.Clone(info.TranscriptHash)})
	})
	return nil
}

type fileCallTakenError string

func (e fileCallTakenError) Error() string {
	return "files: the call stream op " + string(e) + " is already served"
}

func errFileCallTaken(op string) error { return fileCallTakenError(op) }

// replyCall answers a call stream with res or err.
func replyCall(st *transport.Stream, res any, err error) {
	_ = st.SetWriteDeadline(time.Now().Add(fileCallTimeout))
	if err != nil {
		var pe *proto.Error
		if !errors.As(err, &pe) {
			if code := proto.CodeOf(err); code != "" && code != proto.CodeInternal {
				pe = proto.NewError(code, "the file operation was refused")
			} else {
				pe = proto.NewError(proto.CodeInternal, "the Hub could not complete the file operation")
			}
		}
		_ = st.WriteFrame(pe.Frame())
		return
	}
	raw, merr := json.Marshal(res)
	if merr != nil {
		_ = st.WriteFrame(proto.NewError(proto.CodeInternal, "the Hub could not encode the result").Frame())
		return
	}
	_ = st.WriteFrame(workers.OpResult{Type: proto.TypeOpResult, Result: raw})
}

// workerLegStreams serves the data legs one Worker connection opens: each is
// the caller leg of a transfer admitted for that Worker on that connection.
type workerLegStreams struct {
	fs       *files.Service
	workerID string
	conn     []byte
	wg       sync.WaitGroup
}

// Serve implements workers.StreamHandler.
func (h *workerLegStreams) Serve(ctx context.Context, st *transport.Stream, done func()) {
	h.wg.Add(1)
	go func() {
		defer h.wg.Done()
		defer done()
		leg := transport.NewLegConn(st)
		defer func() { _ = leg.Close() }()
		_ = h.fs.ServeWorkerLeg(ctx, h.workerID, h.conn, leg)
	}()
}

// Close implements workers.StreamHandler.
func (h *workerLegStreams) Close() { h.wg.Wait() }
