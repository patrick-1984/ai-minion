package config

import "strings"

// Names of the log export settings (A49 design v2, §3 and §7): one syslog sink
// and one OpenTelemetry (OTLP/HTTP) log sink, each with its own switch,
// endpoint, level and trust. The Hub reads them from the environment and the
// panel (live), the Worker from flags, the environment and its config file.
const (
	KeySyslog               = "syslog"
	KeySyslogEndpoint       = "syslog_endpoint"
	KeySyslogLevel          = "syslog_level"
	KeySyslogMaxBytes       = "syslog_max_bytes"
	KeySyslogCAFile         = "syslog_ca_file"
	KeySyslogClientCertFile = "syslog_client_cert_file"
	KeySyslogClientKeyFile  = "syslog_client_key_file"

	KeyOTel               = "otel"
	KeyOTelEndpoint       = "otel_endpoint"
	KeyOTelLevel          = "otel_level"
	KeyOTelHeaders        = "otel_headers"
	KeyOTelHeadersPath    = "otel_headers_path"
	KeyOTelCAFile         = "otel_ca_file"
	KeyOTelClientCertFile = "otel_client_cert_file"
	KeyOTelClientKeyFile  = "otel_client_key_file"

	// The Worker traffic log (design §4.1): what the Hub sent to and received
	// from Workers, and the Worker's own view.
	KeyTrafficLog        = "worker_traffic_log"
	KeyTrafficLogTo      = "worker_traffic_log_to"
	KeyTrafficLogWorkers = "worker_traffic_log_workers"
	// KeyOTelMetrics is the level of the Hub's Worker traffic metrics.
	KeyOTelMetrics = "otel_metrics"
)

// Values of worker_traffic_log. The payload level (frame bodies) comes with
// the compliance gate of a later release and is not accepted yet.
const (
	TrafficOff      = "off"
	TrafficMetadata = "metadata"
)

// Destinations of worker_traffic_log_to.
const (
	TrafficToLocal  = "local"
	TrafficToSyslog = "syslog"
	TrafficToOTel   = "otel"
)

// Values of the sink switches.
const (
	SinkOff = "off"
	SinkOn  = "on"
)

// Bounds of syslog_max_bytes: 480 is the size every RFC 5426 receiver must
// accept over IPv4, 2048 the size RFC 5425 and RFC 5426 say it should accept.
const (
	SyslogMinBytes     = 480
	SyslogDefaultBytes = 2048
	SyslogMaxBytes     = 65000
)

// ObsSettingKeys lists the keys of the log export settings in the order the
// settings page shows them (the switch, the endpoint and the level first).
var ObsSettingKeys = []string{
	KeySyslog, KeySyslogEndpoint, KeySyslogLevel, KeySyslogMaxBytes,
	KeySyslogCAFile, KeySyslogClientCertFile, KeySyslogClientKeyFile,
	KeyOTel, KeyOTelEndpoint, KeyOTelLevel, KeyOTelHeaders, KeyOTelHeadersPath,
	KeyOTelCAFile, KeyOTelClientCertFile, KeyOTelClientKeyFile,
	KeyTrafficLog, KeyTrafficLogTo, KeyTrafficLogWorkers, KeyOTelMetrics,
}

var sinkLevels = []string{"debug", "info", "warn", "error"}

func init() { declareObs(Default) }

func declareObs(r *Registry) {
	r.Declare(Setting{Key: KeyOTelMetrics, Scope: ScopeHub, Default: "off", Choices: []string{"off", "basic", "detailed"},
		Help: "Worker traffic metrics to the OpenTelemetry endpoint (needs otel on): off; basic (Hub-wide counters: " +
			"connections, Workers online, WebSocket and frame bytes, frames, ops, calls, stream admission, turns); " +
			"detailed (basic per Worker, for the first 500 Workers, plus connection and op durations)"})
	r.Declare(Setting{Key: KeyTrafficLog, Scope: ScopeAny, Default: TrafficOff, Choices: []string{TrafficOff, TrafficMetadata},
		Help: "Log every frame between the Hub and its Workers (Hub: both directions per Worker; Worker: what it sent and " +
			"received): off or metadata (stream, frame type, op, request id, size, duration and outcome; never a body)"})
	r.Declare(Setting{Key: KeyTrafficLogTo, Scope: ScopeAny, Default: TrafficToLocal,
		Help: "Where the traffic log goes, a comma-separated list of local (standard error), syslog and otel; each " +
			"destination must be on. Never the in-memory log"})
	r.Declare(Setting{Key: KeyTrafficLogWorkers, Scope: ScopeHub,
		Help: "Limit the Hub's traffic log to these Workers: a comma-separated list of Worker ids or names (a name that contains a comma in double quotes); empty: all"})
	onOff := []string{SinkOff, SinkOn}
	// Path settings: the registry reserves names ending in _FILE for the
	// file form of secrets, so their variables end in _PATH (as
	// call_api_key_file, MINION_CALL_API_KEY_PATH).
	r.Declare(Setting{Key: KeySyslog, Scope: ScopeAny, Default: SinkOff, Choices: onOff,
		Help: "Send log records to a syslog server (RFC 5424): off or on. Needs syslog_endpoint"})
	r.Declare(Setting{Key: KeySyslogEndpoint, Scope: ScopeAny,
		Help: "Syslog server: udp://host:port (lossy, one datagram per record), tcp://host:port or tls://host:port " +
			"(octet counting, RFC 6587 and RFC 5425)"})
	r.Declare(Setting{Key: KeySyslogLevel, Scope: ScopeAny, Default: "info", Choices: sinkLevels,
		Help: "Lowest level sent to syslog: debug, info, warn or error (independent of log_level)"})
	r.Declare(Setting{Key: KeySyslogMaxBytes, Scope: ScopeAny, Default: "2048",
		Help: "Largest syslog message in bytes, header included (480 to 65000); larger records lose attributes first"})
	r.Declare(Setting{Key: KeySyslogCAFile, Scope: ScopeAny, Env: "MINION_SYSLOG_CA_PATH",
		Help: "PEM file with the CA certificates that sign the syslog server's certificate (tls:// only); unset: system roots"})
	r.Declare(Setting{Key: KeySyslogClientCertFile, Scope: ScopeAny, Env: "MINION_SYSLOG_CLIENT_CERT_PATH",
		Help: "PEM client certificate for mutual TLS to the syslog server (tls:// only); needs syslog_client_key_file"})
	r.Declare(Setting{Key: KeySyslogClientKeyFile, Scope: ScopeAny, Env: "MINION_SYSLOG_CLIENT_KEY_PATH",
		Help: "PEM private key of syslog_client_cert_file; mode 0600, owned by the user that runs Minion"})

	r.Declare(Setting{Key: KeyOTel, Scope: ScopeAny, Default: SinkOff, Choices: onOff,
		Help: "Send log records to an OpenTelemetry collector over OTLP/HTTP: off or on. Needs otel_endpoint " +
			"or OTEL_EXPORTER_OTLP_ENDPOINT"})
	r.Declare(Setting{Key: KeyOTelEndpoint, Scope: ScopeAny,
		Help: "OTLP/HTTP base URL, such as http://collector:4318; /v1/logs is appended. Unset: " +
			"OTEL_EXPORTER_OTLP_LOGS_ENDPOINT (used as is), then OTEL_EXPORTER_OTLP_ENDPOINT"})
	r.Declare(Setting{Key: KeyOTelLevel, Scope: ScopeAny, Default: "info", Choices: sinkLevels,
		Help: "Lowest level sent to OpenTelemetry: debug, info, warn or error (independent of log_level)"})
	r.Declare(Setting{Key: KeyOTelHeaders, Scope: ScopeHub, Secret: true, RegularFileOnly: true,
		Help: "HTTP headers of every OTLP request, one \"Name: value\" per line (for example an Authorization header); " +
			"encrypted at rest. OTEL_EXPORTER_OTLP_HEADERS is ignored"})
	r.Declare(Setting{Key: KeyOTelHeadersPath, Scope: ScopeWorker,
		Help: "File with the HTTP headers of every OTLP request, one \"Name: value\" per line; mode 0600, owned by the " +
			"Worker's user, read at start. OTEL_EXPORTER_OTLP_HEADERS is ignored"})
	r.Declare(Setting{Key: KeyOTelCAFile, Scope: ScopeAny, Env: "MINION_OTEL_CA_PATH",
		Help: "PEM file with the CA certificates that sign the collector's certificate (https only); unset: system roots"})
	r.Declare(Setting{Key: KeyOTelClientCertFile, Scope: ScopeAny, Env: "MINION_OTEL_CLIENT_CERT_PATH",
		Help: "PEM client certificate for mutual TLS to the collector (https only); needs otel_client_key_file"})
	r.Declare(Setting{Key: KeyOTelClientKeyFile, Scope: ScopeAny, Env: "MINION_OTEL_CLIENT_KEY_PATH",
		Help: "PEM private key of otel_client_cert_file; mode 0600, owned by the user that runs Minion"})
}

// privatePathKeys name files that hold a secret (a TLS client key, the
// Worker's header list).
var privatePathKeys = map[string]bool{
	KeySyslogClientKeyFile: true, KeyOTelClientKeyFile: true, KeyOTelHeadersPath: true,
}

// SecretPathValue reports whether value, set for the path setting key, looks
// like the secret itself instead of a path (a pasted PEM key or header
// text): such a value is never shown.
func SecretPathValue(key, value string) bool {
	if !privatePathKeys[key] {
		return false
	}
	return strings.ContainsAny(value, "\n\r") || strings.Contains(value, "-----BEGIN") || strings.Contains(value, ": ") || len(value) > 4096
}
