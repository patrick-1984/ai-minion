// Package workers is the Hub side of the Worker connection: the permanent Hub
// identity, the registry of online Workers and the connect endpoint (AD-3 to
// AD-6, AD-9, AD-17).
//
// Story 4.3 provides:
//
//   - identity.go: [Identity], the Hub's two-tier identity as this package sees
//     it (design hub-identity-v2, protocol 3.0): the ROOT key that Workers pin
//     and an OPERATIONAL key with its certificate, which signs sig_hub, the
//     pairing confirmation and every revoke. The keys live in the Hub identity
//     service (internal/hub/hubid); a connection takes the snapshot (root,
//     operational key, certificate, activation generation) once, and the
//     registry admits it only while its generation is current. [ReadIdentity]
//     reads a plain 0.6 root file without creating anything.
//   - registry.go: [Registry], the online Workers with their negotiated
//     version and announced ops, and [Registry.OpenOp], the only way the Hub
//     opens an op stream. It refuses an op the Worker did not announce with
//     unsupported_op and an offline Worker with worker_offline.
//   - connect.go: [Handler], the handler of GET /api/v1/worker/connect, and
//     [Service], which ties identity, registry and handler together.
//
// # Connect phase
//
// The endpoint applies the "connect" rate limit by client IP before the
// WebSocket upgrade, then runs the AD-6 handshake. After the handshake, and
// before yamux starts, the Hub answers inside the encrypted channel with
// exactly one frame, a uint32 big-endian length plus a JSON object (at most
// proto.StreamFrameLimit bytes), written to and read from the transport
// Channel:
//
//	{"type":"welcome","worker_id":"...","name":"...","owner":"...","kind":"private|infrastructure",
//	 "policy":{"preview_only":false},"proto_min":"3.0","proto_max":"3.0"}
//	{"type":"revoke","sig":"<base64url>"}   // sig: purpose "revoke" over transcript_hash, by the Hub key
//	{"type":"not_enrolled"}                 // non-destructive: the Worker keeps its state
//
// After welcome both sides start yamux on the same Channel (Worker client, Hub
// server). After revoke or not_enrolled the Hub closes the connection. A Worker
// therefore uses transport.Dial, transport.WorkerHandshake, reads this frame
// from the Channel and then transport.NewSession. proto_min and proto_max are
// the Hub's supported protocol range (current minor and N-1) that
// "minion update" compares against (AD-4).
//
// Only the connect handshake mode is served here. Stories 4.5 (enroll_invite)
// and 4.8 (pair_code) register their [ModeHandler] on the [Handler].
//
// # Transition chain
//
// rotate.go holds [ChainSource], which feeds the stored transition chain to the
// connect endpoint. It is sent in connect mode and cut at the root of the
// connection's snapshot; a Worker pinned to any older root follows it (the
// transport handshake skips the links before the pinned root and verifies the
// rest). The master-key rotation itself is hubid.Service.RotateMaster.
//
// # Pairing recovery
//
// pairing.go resumes a pairing the Hub committed but the Worker never recorded
// (same code, same Worker key, same owner, until the code's expiry), and
// pairing_reissue.go lets the owner re-issue the pairing of a Worker that never
// connected (design 12.2 and 12.3).
//
// Owner: Story 4.3 and later stories of epic 4.
package workers
