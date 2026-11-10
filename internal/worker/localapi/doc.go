// Package localapi serves the local control socket (unix socket, named pipe
// on Windows) used by the CLI, `mn` and Solo MCP (AD-22, R16, R22).
//
// The socket carries the same op frames as the Hub-to-Worker `op` stream
// (AD-3): one connection is one request. The client sends one `op.request`
// frame, the server answers with zero or more `op.progress` frames and then
// exactly one `op.result` or `error` frame, and closes the connection. All
// frames use the internal/proto framing (uint32 big-endian length, one JSON
// object with a `type` field, at most 1 MiB).
//
// The OS user who owns the socket is the local owner and holds every action
// ([FullActions]); the server never reads an action set from the request. The
// protection is the file system: the socket is 0600 in a 0700 directory owned
// by the user, so another OS user cannot connect (socket_unix.go). On Windows
// the Worker serves a named pipe whose DACL admits only that user and SYSTEM
// and whose name carries the user's SID (pipe_windows.go). A client
// must also not talk to a socket it does not own: [VerifySocket] checks the
// socket and its directory before a client dials, and [SocketCandidates] gives
// the paths where a Worker may listen (with and without XDG_RUNTIME_DIR).
//
// The package is small on purpose and knows no tmux, no store and no agent
// code. It reaches the Worker through the [Executor] port; the module
// internal/worker/agent/mod_localapi.go adapts the Worker executor to it.
//
// Files:
//
//   - server.go: the frames, [Executor], [Server], [SocketPath],
//     [SocketCandidates], error mapping;
//   - socket_unix.go: [Listen] for unix systems (private directory, start
//     lock, stale socket handling, mode 0600) and [VerifySocket];
//   - socket_other.go: [Listen] and [VerifySocket] on systems with neither
//     unix sockets nor Windows named pipes;
//   - pipe.go: the Windows control pipe without any Windows API: the name
//     \\.\pipe\minion-worker-<user SID> ([PipeName], [ParsePipeName]), its
//     SDDL ([PipeSDDL]: the user and SYSTEM, nobody else) and the rule for who
//     may own a pipe a client connects to ([TrustedPipeOwner]);
//   - pipe_windows.go: [Listen] on the named pipe (exclusive first instance,
//     remote clients refused), [VerifySocket], [VerifyConn] (the client checks
//     the pipe owner on the open connection), [Dial] and [DialContext];
//   - pipe_other.go: [Dial] and [DialContext] on unix systems.
//
// Owner: Stories 2.8 (unix) and 8.2 (Windows named pipe).
package localapi
