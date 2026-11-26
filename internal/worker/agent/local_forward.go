package agent

// SC-5: the Worker-side forward of session control (AD-22, AD-11).
//
// A local request (the CLI, `mn`, MCP on this host) whose target names another
// Worker cannot be served by this Worker's terminals. For the session-control
// ops ([forwardsToHub]) the local socket forwards it unchanged to the Hub on a
// call stream, like the pipe ops in Hub mode, and names the calling session in
// the call frame (source_session): the pane id the client sent ($TMUX_PANE) is
// mapped to its session through this Worker's own terminal host. The Hub
// decides with the session-control grant of that session; this Worker does not
// check one. A pane that cannot be mapped sends no source_session, and the Hub
// then answers as it does for any call without a grant.
//
// A target on this Worker (no Worker named, or this Worker's own id) stays on
// the local socket, unchanged. Without a Hub connection a forwarded request
// fails at once with worker_offline and nothing is queued.

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"

	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/worker/term/tmux"
)

// forwardsToHub reports whether the local request of op with args goes to the
// Hub: workers_list always (it is a Hub op), sessions_list when its worker_id
// names another Worker, and the read and write ops of session control when
// their target names another Worker. self is this Worker's id ("" when it has
// none, then every named Worker is another one).
func forwardsToHub(op string, args json.RawMessage, self string) bool {
	if op == ops.OpWorkersList {
		return true
	}
	if !ops.NamesWorker(op, args) {
		return false
	}
	var a struct {
		WorkerID string      `json:"worker_id"`
		Target   *ops.Target `json:"target"`
	}
	_ = json.Unmarshal(args, &a)
	id := a.WorkerID
	if op != ops.OpSessionsList && a.Target != nil {
		id = a.Target.WorkerID
	}
	return self == "" || id != self
}

// callerPane is the pane a local caller says it runs in: the pane id from its
// $TMUX_PANE, the socket and the pid of the tmux server from its $TMUX, and
// whether a current client could not read its $TMUX (SocketUnknown). A client
// that predates the socket, or that has no $TMUX, sends none of the three.
type callerPane struct {
	Pane          string
	Socket        string
	ServerPID     int64
	SocketUnknown bool
}

// forwardSessionControl sends the local request rq to the Hub with the calling
// session as its source_session: the session of the caller's pane, when it can
// be told ([Executor.sourceSession]).
func (l localExecutor) forwardSessionControl(ctx context.Context, rq Request, caller callerPane) (json.RawMessage, error) {
	op, ok := ops.Lookup(rq.Op)
	if !ok {
		return nil, apiErr(proto.CodeUnsupportedOp, "", "the Worker does not implement this operation")
	}
	if err := authorize(op.Actions, rq.Actions); err != nil {
		return nil, err
	}
	var cc PipeCaller
	if l.calls != nil {
		cc = l.calls()
	}
	if c, ok := cc.(interface{ Connected() bool }); cc == nil || (ok && !c.Connected()) {
		return nil, apiErr(proto.CodeWorkerOffline, "", "the request names another Worker, which only the Hub can reach, and this Worker is not connected to a Hub")
	}
	ctx = WithSourceSession(ctx, l.ex.sourceSession(ctx, caller))
	return forwardCall(ctx, cc, op, rq.Args)
}

// tmuxServerHost is the optional port of a terminal host that serves one tmux
// server: Socket is the absolute path of the socket of that server, or "" when
// the host has no single server to name. The tmux host implements it.
type tmuxServerHost interface {
	Socket() string
}

// maxTmuxSocketPath bounds the socket path a caller may send.
const maxTmuxSocketPath = 4096

// panesWithoutServer counts, for this process, the forwarded requests whose
// pane id came without the tmux server it is of (a client that predates the
// tmux_socket member of the request). The debug log carries the count.
var panesWithoutServer atomic.Int64

// sourceSession returns the calling session of a local request: the session of
// this Worker's terminal host that holds the caller's pane id, when the
// caller's tmux server (its socket, from its $TMUX) is the one this Worker
// serves. It returns "" when the caller is on another server, when the host of
// this Worker is not a tmux server and so cannot be the caller's, when a
// current client could not read its $TMUX, when the caller sent the pid of its
// server and this Worker's server has another one (a server restarted on the
// same socket: [Executor.servesServerPID]), and in every case
// [Executor.sessionOfPane] names none.
//
// A request with neither a socket nor the unknown marker comes from a client
// that predates both, or from one without $TMUX: its pane is mapped as before,
// and the debug log counts it. psmux (Windows) has no socket to compare, so there a current client that
// could not tell its server is mapped as before too.
func (e *Executor) sourceSession(ctx context.Context, c callerPane) string {
	pane, socket := c.Pane, c.Socket
	if pane == "" {
		return ""
	}
	if socket == "" && c.SocketUnknown {
		if e.psmuxHost() {
			return e.sessionOfPane(ctx, pane)
		}
		e.log.Debug("no calling session: the client could not tell the tmux server of its pane", "pane", pane)
		return ""
	}
	if socket == "" {
		e.log.Debug("the calling session is taken from a pane id that came without its tmux server (the client predates tmux_socket)",
			"pane", pane, "count", panesWithoutServer.Add(1))
		return e.sessionOfPane(ctx, pane)
	}
	if !e.servesTmuxServer(socket) {
		e.log.Debug("no calling session: the pane of the caller is not on the tmux server this Worker serves",
			"pane", pane, "caller_socket", socket[:min(len(socket), 200)])
		return ""
	}
	name := e.sessionOfPane(ctx, pane)
	if name != "" && c.ServerPID > 0 && !e.psmuxHost() && !e.servesServerPID(ctx, c.ServerPID) {
		e.log.Debug("no calling session: the tmux server on the caller's socket is not the one the caller ran in (restarted)",
			"pane", pane, "caller_server_pid", c.ServerPID)
		return ""
	}
	return name
}

// tmuxPaneLister is the port of the tmux host that lists the identity of its
// panes, the pid of its server included.
type tmuxPaneLister interface {
	MasterPanes(context.Context) ([]tmux.MasterPane, error)
}

// servesServerPID reports whether the tmux server this Worker serves can be
// the one with the pid pid. It lists after [Executor.sessionOfPane], so a
// server that is restarted in between is never taken for the caller's: the
// caller's server was alive when the request was sent, and a listing of it
// after a listing of another server would need it to come back. A listing
// that names another pid, or no pane at all, is not the caller's server. When
// the host cannot tell its pid (it has no such listing, or the listing fails,
// as for a tmux that runs as another user), the pid is not known and the pane
// is mapped as before.
func (e *Executor) servesServerPID(ctx context.Context, pid int64) bool {
	l, ok := e.host.(tmuxPaneLister)
	if !ok {
		return true
	}
	panes, err := l.MasterPanes(ctx)
	if err != nil {
		return true
	}
	if len(panes) == 0 {
		return false
	}
	for _, p := range panes {
		if int64(p.ServerPID) != pid {
			return false
		}
	}
	return true
}

// psmuxHost reports whether the terminal host of this Worker is psmux.
func (e *Executor) psmuxHost() bool {
	p, ok := e.host.(interface{ Psmux() bool })
	return ok && p.Psmux()
}

// servesTmuxServer reports whether the terminal host of this Worker is the
// tmux server whose socket is socket. A host that is not a tmux server, or
// that names no socket, serves none.
//
// psmux (Windows) is the exception: it has one server per session and no
// socket that names them all, so there is nothing to compare; its panes are
// mapped as before (sessionOfPane refuses a pane id that two sessions hold).
func (e *Executor) servesTmuxServer(socket string) bool {
	h, ok := e.host.(tmuxServerHost)
	if !ok {
		return false
	}
	if e.psmuxHost() {
		return true
	}
	own := h.Socket()
	return own != "" && sameSocketPath(own, socket)
}

// sameSocketPath reports whether the caller's socket path other names the
// socket at own: both paths, resolved by the file system (symbolic links and
// "..", in that order), reach the same file. A path is never compared as text:
// /a/link/../sock is not /a/sock when link points elsewhere, and tmux resolves
// the directory of its default socket, so /tmp/tmux-1000/default and
// /private/tmp/tmux-1000/default are one server. A path that cannot be
// resolved, its socket file gone included, is not the same.
func sameSocketPath(own, other string) bool {
	if len(other) > maxTmuxSocketPath || !filepath.IsAbs(other) || strings.ContainsRune(other, 0) {
		return false
	}
	a, err := os.Stat(own)
	if err != nil {
		return false
	}
	b, err := os.Stat(other)
	if err != nil {
		return false
	}
	return os.SameFile(a, b)
}

// sessionOfPane returns the name of the session of this Worker's terminal host
// that holds the pane id pane, or "" when there is none, the host cannot be
// listed, or more than one session has that pane id (psmux numbers panes per
// session, v0.9.2): among the sessions of this host the caller's session is
// never guessed. It does not know which server the pane id is of;
// [Executor.sourceSession] checks that first.
func (e *Executor) sessionOfPane(ctx context.Context, pane string) string {
	if pane == "" {
		return ""
	}
	all, err := e.host.ListSessions(ctx)
	if err != nil {
		return ""
	}
	found := ""
	for _, s := range all {
		for _, p := range s.Panes {
			if p.Ref.ID == pane {
				if found != "" && found != s.Name {
					return ""
				}
				found = s.Name
			}
		}
	}
	return found
}

// forwardCall sends a local request of op to the Hub as it is, with cc, and
// returns the Hub's answer. Arguments are not decoded: the request id of the
// caller stays the one the Hub records. A write whose call ended after it was
// sent has the outcome unknown (the Hub may have applied it): it is answered
// as that outcome, with a warning, and never repeated here (AD-15).
func forwardCall(ctx context.Context, cc PipeCaller, op ops.Op, raw json.RawMessage) (json.RawMessage, error) {
	var args any
	if len(raw) > 0 {
		args = raw
	}
	if op.Stream {
		err := cc.Stream(ctx, op.Name, args, func(el json.RawMessage) error { return EmitProgress(ctx, el) })
		if err != nil {
			return nil, forwardError(err)
		}
		return json.RawMessage(`{}`), nil
	}
	var res json.RawMessage
	if err := cc.Do(ctx, op.Name, args, &res); err != nil {
		var lost *CallLostError
		if op.Write && errors.As(err, &lost) {
			b, merr := json.Marshal(ops.WriteResult{
				Outcome: ops.OutcomeUnknown, Reason: proto.CodeWorkerOffline.String(),
				Warnings: []ops.Warning{{Code: "hub_call_lost", Message: "the connection to the Hub ended before it answered; the request may have been applied and is not sent again"}},
			})
			if merr != nil {
				return nil, internalErr("encode result", merr)
			}
			return b, nil
		}
		return nil, forwardError(err)
	}
	if len(res) == 0 {
		res = json.RawMessage(`null`)
	}
	return res, nil
}
