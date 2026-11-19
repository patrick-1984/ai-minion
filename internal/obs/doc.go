// Package obs exports Minion's own log records to remote sinks: a syslog
// server (subpackage syslog, RFC 5424) and an OpenTelemetry collector
// (subpackage otel, OTLP/HTTP). It builds the sink pipelines from the
// validated configuration of config.ResolveSinks, puts them into the remote
// slots of the process's logx.Fanout, swaps them atomically on a change and
// shuts them down within the process's stop budget. Every remote handler is
// built with logx.RemoteSafe, and the sinks' own diagnostics stay local and
// rate-limited.
//
// All OpenTelemetry imports of the module live under internal/obs, so that a
// breaking change of a v0 OpenTelemetry module stays local (a test checks it).
//
// Owner: A49 observability export, PR 1 (log sinks and redaction).
package obs
