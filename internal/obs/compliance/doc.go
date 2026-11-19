// Package compliance records the session content that passes through the
// Hub's two front doors (the REST dispatcher and the Worker call-stream
// server) and sends it to a collector of its own (A49 design v2, §5).
//
// The package has three parts. The policy (policy.go) is the version, level
// and input switch in force and the live notice rule. The recorder
// (matrix.go, recorder.go, dedupe.go) turns a call into records according to
// a typed matrix of op x path x level; it holds no state of a session and
// never blocks a call. The exporter (exporter.go) queues records under byte
// budgets and sends them over OTLP/HTTP with the Logs API, separately from
// every ordinary log sink.
//
// Content is masked at the source: only Minion's own exact secret formats
// (logx.RedactTokens), before it is cut to its size cap and queued. Nothing
// here writes to the local log.
//
// Owner: A49 PR 3 (T15 to T18).
package compliance
