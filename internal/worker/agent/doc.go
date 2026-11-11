// Package agent is the Worker runtime: Hub connector, op server, call client and event stream (AD-3, AD-12).
//
// Story 2.6 adds the op executor, the one place where Worker ops run. The Hub
// op server, the local socket and the pipe all go through it, so that every
// rule of the action contract (AD-15) is enforced once, on the Worker, whatever
// the caller sends (AD-12).
//
//   - exec.go: [Executor], [Config], [Request] (op name, the caller's effective
//     actions and the JSON arguments), the dispatcher by op name ([Executor.Execute],
//     [Executor.Handle] for ops that later stories add), [APIError] and the
//     action checks. Permission, not-found, invalid-argument, conflict and
//     internal failures are API errors, never outcomes;
//   - exec_sessions.go: sessions_list, session_create, session_snapshot and
//     session_capture, the pane resolution and the classification step;
//   - exec_write.go: the frame of every write (policy, duplicate cache, result
//     cache, local audit), the pane writes send_text, send_keys and answer, and
//     set_task;
//   - locks.go: [Locks], the per-pane writer locks with a wait timeout, shared by
//     every entry point that writes to a pane;
//   - policy.go: [Policy], the local policy (preview-only and the share
//     selection).
//
// Guard classes (AD-15, R87): the target re-check, the writer lock, the dialog
// binding of an answer (the pane, process, session generation and dialog the
// caller reviewed, EPICS A-22) and real errors are KEEP and never
// overridable; an unknown pane state only adds a warning (WARN); a shell pane,
// an open dialog and a busy agent refuse with failed_not_sent and
// forceable=true unless the same call carries force=true (FORCE). Once any
// key or text went out the outcome is never failed_not_sent, which includes
// the Enter of send_text with submit.
//
// A write carries a request_id (UUIDv7). The result is cached in worker.db for
// ten minutes and a duplicate is answered from the cache without touching the
// pane, a refusal included: repeating a refused write with force=true is a new
// request with a new id. Reusing an id for a different request is a conflict.
// Every write handled after validation also writes a local audit row (op,
// target, outcome, reason, request id, time) that never holds content (AD-20).
//
// Story 2.7 adds the runtime that composes the Worker from modules:
//
//   - runtime.go: [Registry] with [RegisterModule] and [RegisterPreStart],
//     [Runtime] (start in order, stop in reverse within [DefaultStopTimeout],
//     SIGINT and SIGTERM), [Env] with [Provide] and [Require] for the
//     services that modules hand to later modules, and [Settings];
//   - mod_core.go: the core module (order [OrderCore]) that opens the state,
//     starts the terminal host on the socket that config resolved once,
//     applies share_existing and preview_only and provides *store.Store,
//     term.SessionHost, *Policy and *Executor;
//   - host_other.go and host_windows.go: the tmux host on Unix, the ConPTY host
//     on Windows.
//
// A new module is a file mod_<name>.go whose init function calls
// [RegisterModule]; runtime.go is not edited. Pre-start hooks run once, in
// registration order, before the first module starts; `minion worker up`
// runs them through [Registry.RunPreStart].
//
// Story 4.4 adds the Hub connector, the Worker side of AD-3 and AD-6:
//
//   - connector.go: [Connector], the state machine that keeps the Worker
//     connected to its Hub forever. It dials out in connect mode with the
//     stored pin, retries with the AD-3 backoff (capped at 60 s, no time
//     limit) after every failure, loss and restart, and treats welcome,
//     not_enrolled and revoke as the AD-9 rules say: a welcome stores worker
//     id, owner and Hub policy and applies the policy on top of the local one
//     (local restrictions win); not_enrolled keeps everything and retries;
//     only a revoke signed by the pinned Hub key over this connection's
//     transcript wipes the pin and the enrollment, and an unsigned or wrongly
//     signed one is ignored and logged. Nothing on the channel expires. The
//     ports are [Dialer], [PolicySink] and [Enrollments]; [Connector.OnLink]
//     hands every welcomed [Link] (yamux session, welcome) to the handlers of
//     later stories, whose in-flight ops end as outcome unknown when the link
//     context ends with [ErrLinkLost];
//   - connector_transport.go: [NewHubDialer], the production [Dialer]: WSS dial
//     that accepts any certificate (A-15), the handshake, the control frame
//     and the yamux session;
//   - enrollment.go: [Enrollments], the one API for the Worker key pair
//     (worker.key), the Hub pin and the enrollment state that enrollment,
//     reset and removal use;
//   - mod_connector.go: the connector module (order [OrderConnector]) that
//     provides *Enrollments and *Connector. Without the server setting it
//     stays idle and the Worker runs alone (Solo).
//
// Story 5.2 adds what the Worker does on a welcomed link, the Worker side of
// the op and ev streams of AD-3:
//
//   - opserver.go: [OpServer], which serves every op stream the Hub opens
//     through the same [Executor] as the local socket. Per stream: one
//     op.request in ([OpRequestFrame]; its actions are intersected with the
//     local policy by [IntersectActions], so a preview-only Worker takes no
//     operate or message whatever the Hub sends), then op.progress frames that
//     an op reports with [Progress], then exactly one op.result or one error
//     frame, then the stream closes. A write is never cut off, neither by a
//     lost link nor by a Hub that resets the stream (its result stays in the
//     result cache for the Hub's duplicate); a read ends with the link, with
//     [OpServer.Stop] and when the Hub abandons the stream (any read result on
//     the stream after the request, such as a reset), so an abandoned
//     pipe_wait frees its slot at once. The budget of [OpServerConfig.MaxOps]
//     slots belongs to the server and not to a link, refused streams are
//     bounded until they are gone ([OpServerConfig.MaxRefused]; the link is
//     closed when the Hub piles them up), and an unexpected error reaches the
//     Hub only as a generic internal error;
//   - events.go: [EventPublisher], which opens one ev stream per link and sends
//     events.snapshot ([SnapshotFrame], seq 0) first, then an event
//     ([EventFrame]) per change with seq rising by one. If the Hub ends the
//     stream on a live link, a new one starts with a fresh snapshot. A later
//     module receives the frames the Hub sends on the ev stream (the turn.ack
//     of Story 7.4) with [EventPublisher.OnFrame]: it takes the publisher with
//     Require[*EventPublisher](env) and registers a handler per frame type,
//     before or while the publisher serves;
//   - watch.go: [Observer] and its production form [HostObserver] (the shared
//     sessions with kind, state, task and a hash of the sanitised screen,
//     never content), [Diff], [ScreenHash] and the setting
//     session_poll_interval (flag --session-poll-interval, 100 ms to 1 m,
//     default 1 s);
//   - mod_opserver.go: the op server module (order [OrderOpServer]) that
//     registers both handlers with [Connector.OnLink] and stops the op server
//     (a write in flight finishes) before the connector, the store and the
//     host are stopped.
//
// Wire shapes and who owns them. The op stream shapes are wire-compatible with
// the Hub side that Story 5.1 defines in internal/hub/workers/route.go:
// OpRequest{type, op, request_id, args, actions, origin} where origin is the
// Hub-derived provenance (protocol 2.0, required for message-class ops) and request_id is always
// sent (a fresh UUIDv7 for a read, the arguments' request_id for a write) and
// actions is an ops.ActionSet, a JSON array; OpProgress{type, data} and
// OpResult{type, result}. [OpRequestFrame], [OpProgressFrame] and
// [OpResultFrame] decode and encode exactly these; the Worker adds only an
// optional message to op.progress, which the Hub ignores. For a streaming op
// (pipe_wait) the rule of Story 5.1 holds: each element is sent as the data of
// one op.progress frame, then one op.result without an element ends the op.
//
// The events.snapshot and event shapes ([SnapshotFrame], [EventFrame]) are the
// contract for Story 5.3 (internal/hub/workers/events.go), not for Story 5.1.
// Every session entry carries name, kind, state and task (always present, ""
// when the session has no task; a change of the task is a state_changed
// event) and a screen_hash that is a hash and never content. The Hub must not
// close its side of the ev stream unless it wants the Worker to open a new one
// after two seconds ([DefaultReopenDelay]) with a fresh snapshot and seq 0,
// which is also how a gap in seq is repaired.
//
// Only shared sessions are visible: the executor refuses reads of an unshared
// session, and the observer leaves it out, so it appears in no op result and no
// event. A session that becomes shared is announced as session_added and one
// that stops being shared as session_removed.
//
// The rule behind that, and not only its effect, is declared to the Hub: the
// events.snapshot carries the share rule (all, none, or the names selected) with
// the Worker's durable revision of it, and an event frame carries it when it
// changed (a share_changed event of its own when the look had no other event).
// The rule always precedes the events it makes visible on the same ordered
// stream. With it the Hub decides, for a session the Worker does not list
// (closed, offline, unshared), whether it may still show what it stored of it;
// without it it shows nothing of the stored history. share_get and share_set
// answer with the same revision when the Hub asks for it. Reads of private
// content (sessions_list, session_snapshot, session_capture) are delivered only
// if the rule did not change while they ran. share_get and share_set act only on
// the Worker that names itself in worker_id.
//
// Owner: Stories 2.6, 2.7, 4.4 and 5.2.
package agent
