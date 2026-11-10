// Package transport implements the WebSocket transport, the handshake, the encrypted secure channel, yamux multiplexing and the stream kinds between Worker and Hub (AD-3, AD-6). It is the only package that imports yamux and websocket.
//
// Story 4.1 (handshake and secure channel) provides:
//
//   - handshake.go: the Worker and Hub state machines of the AD-6 handshake,
//     WorkerHandshake and HubHandshake, for the modes connect, enroll_invite
//     and pair_code. They run on any net.Conn (a WebSocket NetConn, net.Pipe
//     in tests), exchange raw handshake frames (uint32 length plus JSON, at
//     most 64 KiB) and return a Channel. Trust never depends on TLS: the
//     Worker checks the Hub key against its pin (after applying the Hub's
//     transition chain), against the invitation fingerprint, or through CPace
//     confirmation (AD-8, internal/pake). sig_worker is sent only after
//     the Hub is authenticated, in all three modes. In pair_code mode the
//     Worker's pake.confirm goes first, because the Hub, which alone counts
//     attempts, never reveals its tag to an unverified peer.
//   - transcript.go: transcript_hash over the raw hello bytes as sent.
//   - kdf.go: HKDF-SHA256 key schedule, two directional keys.
//   - channel.go: the ChaCha20-Poly1305 record layer with an implicit 64-bit
//     counter nonce per direction. Channel implements net.Conn so yamux can
//     run on it. A replayed, reordered or modified record closes it.
//
// Story 4.2 adds, on top of Channel:
//
//   - ws.go: Dial (Worker) and Accept (Hub) for GET /api/v1/worker/connect
//     with subprotocol minion.v1 and binary messages, exposed as net.Conn
//     through coder/websocket NetConn. The WSS dial accepts any server
//     certificate (A-15): trust is the AD-6 handshake, never TLS.
//   - mux.go: Session, the yamux client (Worker) or server (Hub) over the
//     Channel, with ConnectWorker and AcceptWorker running dial, handshake
//     and session in one call.
//   - streams.go: the two-byte stream kind prefix op, ca (the call kind) and
//     ev (A-7), Stream with frame helpers (uint32 length, at most 1 MiB), and
//     the reset of streams with an unknown kind or a kind from the wrong side.
//   - keepalive.go: a ping every 30 s and a dead connection after
//     2 min without traffic, on an injectable Clock.
//   - backoff.go: reconnect delays growing to a 60 s cap with jitter, never
//     giving up.
//
// Owner: Stories 4.1 and 4.2.
package transport
