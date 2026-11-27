// Package files is the Hub's file transfer service (files.v1, BRD
// minion-file-transfer v2, WP7).
//
// The Hub never stores file content: it admits a transfer, relays its bytes
// between the caller's data leg and the target's data leg, and keeps the
// durable record and the audit. The parts:
//
//   - admit.go: the admission of file_pull and file_push ([Service.AdmitFile],
//     a workers.FileAdmitter installed with Router.SetFileAdmitter). It decides
//     both ends (authz.Authorizer.DecideFile and authz.CheckFileEnds, the
//     caller session's share), applies the concurrency and admission rate
//     limits, persists the record (state admitted) before anything is sent to
//     the target Worker (an insert that fails refuses the transfer), refuses
//     a caller request that sets the Hub's hidden members (ops.FileLegArgs) and
//     sets them itself: the transfer id, the target leg's token and the Hub's
//     limits. The caller gets the admission with its own single-use leg token.
//   - gate.go: the decision of both ends, made at admission, before the target
//     leg opens, every [proto.RedecideInterval] while bytes move, and at the
//     commit gate: when the sender's body has ended, the final trailers (and
//     with them the receiver's ability to verify and publish, FR-9) are
//     forwarded only after a fresh decision allows the transfer.
//   - relay.go: the caller leg's HTTP handler ([Service.ServeLeg]): token,
//     direction, identity, connection and expiry binding; the target leg
//     (LegOpener) with net/http over a transport.LegConn; body relay without
//     decoding with byte counting, the wire limit and the idle timeout; Range
//     and If-Range are relayed, never served from the Hub.
//   - receipt.go: file_receipt, the receiver's terminal statement of a pull,
//     accepted only from the receiving leg's identity with its leg token
//     (checked against the stored hash, so it works after a restart); a push
//     ends with the target's 201 receipt on its leg.
//   - reconcile.go: at start every open record of the previous process
//     becomes unknown; every [proto.ReconcileInterval] the ones past their
//     deadline do, and records older than the retention are purged.
//   - http.go: the public endpoint GET/PUT /files/v1/transfers/<id>/content
//     for the web UI and REST callers, under the normal session (and CSRF) or
//     API key authentication.
//   - history.go: file_transfers_list, owner-scoped.
//   - audit.go: audit events file.pull / file.push whose Detail names only the
//     transfer id, direction, counts, outcome, reason and manifest digest:
//     never a name, a path or a token.
//
// Nothing here logs a path, a name, a token or content.
package files
