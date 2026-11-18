package config

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
}

var sinkLevels = []string{"debug", "info", "warn", "error"}

func init() { declareObs(Default) }

func declareObs(r *Registry) {
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
