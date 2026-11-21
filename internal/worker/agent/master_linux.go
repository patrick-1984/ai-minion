//go:build linux && !android

package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/worker/localapi"
	"github.com/patrick-1984/ai-minion/internal/worker/store"
	"github.com/patrick-1984/ai-minion/internal/worker/term/tmux"
)

type masterHost interface {
	MasterPanes(context.Context) ([]tmux.MasterPane, error)
}

type masterOwner struct {
	workerID, ownerID string
	epoch             uint64
}

func linkMasterOwner(l *Link) masterOwner {
	return masterOwner{l.Welcome.WorkerID, l.Welcome.OwnerID, l.gen}
}

type masterAttachment struct {
	terminal           tmux.MasterPane
	owner              masterOwner
	incarnation        string
	harness, mux, peer *masterProcess
	claude             os.FileInfo
	startBy            time.Time
	execObserved       bool
	catalog            string
	lease              *masterLease
}

type masterLease struct {
	wire   proto.MasterBinding
	until  time.Time
	ctx    context.Context
	cancel context.CancelFunc
}

// masterBroker is a dedicated bridge between one verified local harness
// process and this Worker's captured authenticated Hub link. It never calls
// the ordinary local executor or resolves a call key.
type masterBroker struct {
	host   masterHost
	policy *Policy
	share  *sync.Mutex
	self   os.FileInfo
	caller *CallClient

	mu          sync.Mutex
	attachments map[int]*masterAttachment
	current     *masterLane
	changed     chan struct{}
}

func newMasterBroker(host masterHost, executor *Executor, self os.FileInfo) *masterBroker {
	return &masterBroker{
		host: host, policy: executor.Policy(), share: &executor.shareMu, self: self, caller: NewCallClient(CallConfig{}),
		attachments: make(map[int]*masterAttachment), changed: make(chan struct{}, 1),
	}
}

func masterRefused() error {
	return proto.NewError(proto.CodeForbidden, "this invocation has no active Minion master attachment")
}

func (b *masterBroker) serveLocal(parent context.Context, conn *net.UnixConn) {
	stop := context.AfterFunc(parent, func() { _ = conn.Close() })
	defer stop()
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
	process, err := masterPeer(conn)
	if err != nil {
		return
	}
	defer process.close()
	body, err := proto.ReadFrame(conn, proto.MasterFrameLimit)
	if err != nil {
		return
	}
	var request localapi.MasterRequest
	if err := proto.DecodeMasterJSON(body, &request); err != nil {
		masterLocalReply(conn, localapi.MasterResponse{}, err)
		return
	}
	if request.Type != localapi.TypeMasterLocalRequest {
		masterLocalReply(conn, localapi.MasterResponse{}, proto.NewError(proto.CodeInvalidArgument, "unexpected local master request"))
		return
	}
	ctx, cancel := context.WithTimeout(parent, 30*time.Second)
	defer cancel()
	// Any additional byte or shutdown of this one-request socket abandons the
	// call. After dispatch that maps to an unknown write outcome.
	_ = conn.SetReadDeadline(time.Time{})
	go func() {
		var one [1]byte
		_, _ = conn.Read(one[:])
		cancel()
	}()
	res, err := b.handleLocal(ctx, process, request)
	_ = conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
	masterLocalReply(conn, res, err)
}

func masterLocalReply(conn *net.UnixConn, res localapi.MasterResponse, err error) {
	if err != nil {
		var refusal *proto.Error
		if !errors.As(err, &refusal) {
			refusal = proto.NewError(proto.CodeWorkerOffline, "the scoped Minion master call could not complete")
		}
		f := refusal.Frame()
		res = localapi.MasterResponse{Error: &f}
	}
	res.Type = localapi.TypeMasterLocalResponse
	_ = proto.WriteFrame(conn, proto.StreamFrameLimit, res)
}

func (b *masterBroker) handleLocal(ctx context.Context, process *masterProcess, request localapi.MasterRequest) (localapi.MasterResponse, error) {
	if request.Kind == localapi.MasterRegister {
		return localapi.MasterResponse{}, b.register(ctx, process, request.Executable)
	}
	if request.Kind == localapi.MasterCall {
		if err := validateLocalMasterCall(request); err != nil {
			return localapi.MasterResponse{}, err
		}
		b.mu.Lock()
		authenticated := b.peerAttachment(process, false) != nil
		b.mu.Unlock()
		if !authenticated {
			return localapi.MasterResponse{}, masterRefused()
		}
		// Re-read terminal identity for every operation. Catalog polling by
		// the harness relies on the shared periodic observation.
		b.reconcile(ctx)
		return b.admitLocalCall(ctx, process, request)
	}
	b.mu.Lock()
	a := b.peerAttachment(process, request.Kind == localapi.MasterStatus)
	if a == nil {
		b.mu.Unlock()
		return localapi.MasterResponse{}, masterRefused()
	}
	lease := b.currentLease(a)
	gen := uint64(0)
	if lease != nil {
		gen = lease.wire.Generation
	}
	switch request.Kind {
	case localapi.MasterStatus:
		b.mu.Unlock()
		return localapi.MasterResponse{Generation: gen}, nil
	case localapi.MasterCatalog:
		if request.Generation == gen {
			b.applyCatalog(a, gen, request.Report)
		}
		b.mu.Unlock()
		return localapi.MasterResponse{}, nil
	default:
		b.mu.Unlock()
		return localapi.MasterResponse{}, proto.NewError(proto.CodeInvalidArgument, "unknown local master request")
	}
}

func (b *masterBroker) admitLocalCall(ctx context.Context, process *masterProcess, request localapi.MasterRequest) (localapi.MasterResponse, error) {
	b.share.Lock()
	b.mu.Lock()
	a := b.peerAttachment(process, false)
	if a == nil {
		b.mu.Unlock()
		b.share.Unlock()
		return localapi.MasterResponse{}, masterRefused()
	}
	lease := b.currentLease(a)
	shared, err := b.policy.Shared(ctx, a.terminal.SessionName)
	if lease == nil || request.Generation != lease.wire.Generation || !shared || err != nil {
		b.mu.Unlock()
		b.share.Unlock()
		return localapi.MasterResponse{}, masterRefused()
	}
	link := b.current.link
	frame := proto.MasterCallRequest{Type: proto.TypeOpRequest, Op: proto.OpMasterCallV1, RequestID: request.RequestID,
		Args: proto.MasterCallArgs{IncarnationID: a.incarnation, Generation: request.Generation, LeaseID: lease.wire.LeaseID,
			Operation: request.Operation, Arguments: request.Arguments}}
	callCtx, cancel := context.WithDeadline(ctx, lease.until)
	stop := context.AfterFunc(lease.ctx, cancel)
	b.mu.Unlock()
	b.share.Unlock()
	defer cancel()
	defer stop()
	if lease.ctx.Err() != nil {
		return localapi.MasterResponse{}, masterRefused()
	}
	return b.call(callCtx, link, frame)
}

func (b *masterBroker) revokeUnshared(selection store.ShareSelection) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, a := range b.attachments {
		if !selection.Includes(a.terminal.SessionName) {
			b.retire(a)
		}
	}
}

func (b *masterBroker) register(ctx context.Context, process *masterProcess, executable string) error {
	if !process.imageIs(b.self) || !filepath.IsAbs(executable) || executable == "" || len(executable) > 4096 {
		return masterRefused()
	}
	image, err := os.Stat(executable)
	if err != nil || !image.Mode().IsRegular() || image.Mode().Perm()&0o111 == 0 || os.SameFile(image, b.self) {
		return proto.NewError(proto.CodeInvalidArgument, "the Claude Code executable cannot be pinned")
	}
	panes, err := b.host.MasterPanes(ctx)
	if err != nil {
		return proto.NewError(proto.CodeConflict, "the Worker could not verify its tmux host")
	}
	var pane tmux.MasterPane
	for _, row := range panes {
		if row.PanePID != process.pid {
			continue
		}
		if pane.PaneID != "" {
			return proto.NewError(proto.CodeConflict, "this pane is shared by more than one terminal session")
		}
		pane = row
	}
	if pane.PaneID == "" || !proto.ValidMasterSessionName(pane.SessionName) {
		return proto.NewError(proto.CodeConflict, "use exec minion master claude as the process of a Worker-visible tmux pane")
	}
	b.share.Lock()
	defer b.share.Unlock()
	shared, err := b.policy.Shared(ctx, pane.SessionName)
	if err != nil || !shared {
		return proto.NewError(proto.CodeForbidden, "share this terminal session with the Hub before preparing it")
	}
	server, err := masterPinPID(pane.ServerPID)
	if err != nil {
		return masterRefused()
	}
	harness, err := masterPinPID(process.pid)
	if err != nil {
		server.close()
		return masterRefused()
	}
	a := &masterAttachment{terminal: pane, incarnation: ops.NewUUIDv7(),
		harness: harness, mux: server, claude: image, startBy: time.Now().Add(10 * time.Second)}
	if err := b.adopt(a, process); err != nil {
		server.close()
		harness.close()
		return err
	}
	return nil
}

func (b *masterBroker) adopt(a *masterAttachment, process *masterProcess) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.current == nil || b.current.ctx.Err() != nil || !b.sameOwner(linkMasterOwner(b.current.link)) {
		return proto.NewError(proto.CodeWorkerOffline, "the Worker needs an approved Hub with session master support")
	}
	if !process.same(a.harness) || !a.mux.live() {
		return masterRefused()
	}
	if len(b.attachments) >= proto.MaxMasterCandidates {
		return proto.NewError(proto.CodeRateLimited, "too many prepared master sessions")
	}
	for _, existing := range b.attachments {
		if existing.terminal.SessionID == a.terminal.SessionID || existing.harness.pid == a.harness.pid {
			return proto.NewError(proto.CodeConflict, "this terminal session already has a prepared Minion master invocation")
		}
	}
	a.owner = linkMasterOwner(b.current.link)
	b.attachments[a.harness.pid] = a
	return nil
}

// peerAttachment must be called while holding mu. A new process after the
// original helper's exit never inherits the role, even as another direct child.
func (b *masterBroker) peerAttachment(process *masterProcess, initial bool) *masterAttachment {
	parent, _, err := masterProcessInfo(process.pid)
	if err != nil || !process.live() || !process.imageIs(b.self) {
		return nil
	}
	a := b.attachments[parent]
	if a == nil || !b.sameOwner(a.owner) || !a.harness.live() || !a.mux.live() || !a.harness.imageIs(a.claude) {
		return nil
	}
	if a.peer == nil && initial {
		pin, err := masterPinPID(process.pid)
		if err != nil {
			return nil
		}
		if !pin.same(process) {
			pin.close()
			return nil
		}
		a.peer = pin
		a.execObserved = true
		b.signal()
	}
	if !a.peer.same(process) {
		return nil
	}
	return a
}

func (b *masterBroker) sameOwner(o masterOwner) bool {
	return o.workerID != "" && o.ownerID != "" && b.policy.WorkerID() == o.workerID && b.policy.OwnerID() == o.ownerID
}

func (b *masterBroker) currentLease(a *masterAttachment) *masterLease {
	lease := a.lease
	if lease == nil || b.current == nil || b.current.ctx.Err() != nil ||
		a.owner != linkMasterOwner(b.current.link) || !time.Now().Before(lease.until) {
		return nil
	}
	return lease
}

func (b *masterBroker) applyCatalog(a *masterAttachment, gen uint64, report string) {
	switch report {
	case proto.MasterCatalogActive:
		if gen == 0 {
			return
		}
	case proto.MasterCatalogEmpty:
		if gen != 0 {
			return
		}
	case proto.MasterCatalogFailed:
	default:
		return
	}
	if a.catalog != report {
		a.catalog = report
		b.signal()
	}
}

func (b *masterBroker) signal() {
	select {
	case b.changed <- struct{}{}:
	default:
	}
}

func (b *masterBroker) retire(a *masterAttachment) {
	b.clearLease(a)
	delete(b.attachments, a.harness.pid)
	a.peer.close()
	a.harness.close()
	a.mux.close()
	b.signal()
}

func (*masterBroker) clearLease(a *masterAttachment) {
	if a.lease != nil {
		a.lease.cancel()
		a.lease = nil
	}
}

func (b *masterBroker) reconcile(ctx context.Context) {
	b.mu.Lock()
	known := make([]*masterAttachment, 0, len(b.attachments))
	for _, a := range b.attachments {
		known = append(known, a)
	}
	b.mu.Unlock()
	if len(known) == 0 {
		return
	}
	panes, hostErr := b.host.MasterPanes(ctx)
	shares, shareErr := b.policy.Selection(ctx)
	if ctx.Err() != nil {
		return // an abandoned request is not evidence about other attachments
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, a := range known {
		if b.attachments[a.harness.pid] != a {
			continue
		}
		if hostErr != nil || shareErr != nil || !b.liveAttachment(a, panes, shares) {
			b.retire(a)
			continue
		}
		if a.lease != nil && b.currentLease(a) == nil {
			b.clearLease(a)
		}
	}
}

func (b *masterBroker) liveAttachment(a *masterAttachment, panes []tmux.MasterPane, shares store.ShareSelection) bool {
	if !b.sameOwner(a.owner) || !a.mux.live() || !a.harness.live() || !shares.Includes(a.terminal.SessionName) {
		return false
	}
	if a.harness.imageIs(a.claude) {
		a.execObserved = true
	} else if a.execObserved || !time.Now().Before(a.startBy) || !a.harness.imageIs(b.self) {
		return false
	}
	if a.peer != nil && !a.peer.live() {
		return false
	}
	seen := 0
	for _, p := range panes {
		if p.PaneID != a.terminal.PaneID {
			continue
		}
		if p != a.terminal {
			return false
		}
		seen++
	}
	return seen == 1
}

func (b *masterBroker) candidates() []proto.MasterCandidate {
	candidates := make([]proto.MasterCandidate, 0, len(b.attachments))
	for _, a := range b.attachments {
		if a.peer != nil {
			candidates = append(candidates, proto.MasterCandidate{SessionName: a.terminal.SessionName,
				IncarnationID: a.incarnation, Adapter: proto.MasterAdapterClaude, CatalogResult: a.catalog})
		}
	}
	slices.SortFunc(candidates, func(a, b proto.MasterCandidate) int {
		if a.SessionName < b.SessionName {
			return -1
		}
		if a.SessionName > b.SessionName {
			return 1
		}
		return 0
	})
	return candidates
}

func validateLocalMasterCall(req localapi.MasterRequest) error {
	if req.Generation == 0 || req.Generation > proto.MaxMasterGeneration || !ops.ValidUUIDv7(req.RequestID) {
		return proto.NewError(proto.CodeInvalidArgument, "invalid master call identity")
	}
	invalid := proto.NewError(proto.CodeInvalidArgument, "invalid scoped master operation arguments")
	switch req.Operation {
	case ops.OpWorkersList:
		return proto.DecodeMasterJSON(req.Arguments, &struct{}{})
	case ops.OpSessionsList:
		var a struct {
			WorkerID string `json:"worker_id"`
		}
		if proto.DecodeMasterJSON(req.Arguments, &a) == nil && ops.ValidUUIDv7(a.WorkerID) {
			return nil
		}
	case ops.OpSessionSnapshot:
		var a ops.SessionSnapshotArgs
		if proto.DecodeMasterJSON(req.Arguments, &a) == nil && a.Target.WorkerID != "" && a.Validate() == nil {
			return nil
		}
	case ops.OpSendText:
		var a ops.SendTextArgs
		if proto.DecodeMasterJSON(req.Arguments, &a) == nil && a.Target.WorkerID != "" &&
			a.RequestID == req.RequestID && a.Validate() == nil && !a.Force && a.Submit && !a.AgentOnly {
			return nil
		}
	default:
		return proto.NewError(proto.CodeForbidden, "operation is outside the master tool catalog")
	}
	return invalid
}

func (b *masterBroker) call(ctx context.Context, link *Link, req proto.MasterCallRequest) (localapi.MasterResponse, error) {
	var res localapi.MasterResponse
	if err := req.Validate(); err != nil {
		return res, err
	}
	if err := proto.WriteFrame(io.Discard, proto.MasterFrameLimit, req); err != nil {
		return res, err
	}
	write := req.Args.Operation == ops.OpSendText
	err := b.caller.runRequest(ctx, link, req, write, nil, func(body json.RawMessage) { res.Result = body })
	var lost *CallLostError
	if write && errors.As(err, &lost) {
		res.Result, _ = json.Marshal(ops.WriteResult{
			Outcome: ops.OutcomeUnknown, Reason: string(proto.CodeWorkerOffline),
			Warnings: []ops.Warning{{Code: "hub_call_lost", Message: "the Hub call ended before its answer. Check the target before retrying with the same request ID"}},
		})
		return res, nil
	}
	return res, err
}
