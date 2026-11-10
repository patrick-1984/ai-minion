// Package workers is the Hub side of the Worker connection: the permanent Hub
// identity, the registry of online Workers and the connect endpoint (AD-3 to
// AD-6, AD-9, AD-17).
//
// Story 4.3 provides:
//
//   - identity.go: [Identity], the Hub's Ed25519 key in <data>/identity.key,
//     created on the first start and checked against the hub_identity row on
//     every later start, so it never changes on restart, certificate rotation
//     or restore. [ReadIdentity] reads it without creating anything (used by
//     "minion hub identity").
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
//	 "policy":{"preview_only":false},"proto_min":"1.0","proto_max":"1.0"}
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
// # Identity rotation (Story 4.9)
//
// rotate.go holds [Rotator], the planned rotation of the Hub identity (AD-5):
// a new key, a transition record signed by the old key, the audit row and the
// replaced hub_identity row in one transaction, then the switch of the running
// Hub and the revocation of every outstanding invitation key. [ChainSource]
// feeds the stored chain to the connect endpoint, which sends it in connect
// mode; a Worker pinned to any older key follows it (the transport handshake
// skips the links before the pinned key and verifies the rest). An interrupted rotation is completed at the next start
// ([LoadOrCreateIdentity]).
//
// Owner: Story 4.3 and later stories of epic 4.
package workers
