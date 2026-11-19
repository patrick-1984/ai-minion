package config

import "slices"

// Compliance monitoring (A49 design v2, §5): the Hub records what it accepted
// from and returned to a caller at its two front doors and sends it to its
// own collector. The keys below can be changed only through the compliance_set
// operation (a fresh, direct administrator browser session) or the Hub's
// environment; settings_set refuses them.
const (
	KeyComplianceMonitoring     = "compliance_monitoring"
	KeyComplianceInput          = "compliance_input"
	KeyComplianceEndpoint       = "compliance_endpoint"
	KeyComplianceHeaders        = "compliance_headers"
	KeyComplianceCAFile         = "compliance_ca_file"
	KeyComplianceClientCertFile = "compliance_client_cert_file"
	KeyComplianceClientKeyFile  = "compliance_client_key_file"
)

// Values of compliance_monitoring and worker_traffic_log.
const (
	ComplianceOff     = "off"
	ComplianceActions = "actions"
	ComplianceOutput  = "output"

	// TrafficPayload is the worker_traffic_log level with the frame bodies. It
	// carries session content and is gated like compliance monitoring.
	TrafficPayload = "payload"
)

// ComplianceKeys are the settings that only compliance_set (or the Hub's
// environment) changes, in the order the settings page shows them.
var ComplianceKeys = []string{
	KeyComplianceMonitoring, KeyComplianceInput, KeyComplianceEndpoint, KeyComplianceHeaders,
	KeyComplianceCAFile, KeyComplianceClientCertFile, KeyComplianceClientKeyFile,
}

// GatedKeys are ComplianceKeys and worker_traffic_log, whose value payload is
// gated.
func GatedKeys() []string { return append(slices.Clone(ComplianceKeys), KeyTrafficLog) }

// IsComplianceKey reports whether key is one of ComplianceKeys.
func IsComplianceKey(key string) bool { return slices.Contains(ComplianceKeys, key) }

// SectionCompliance is the settings page section of the compliance keys.
const SectionCompliance = "Compliance monitoring"

func init() { declareCompliance(Default) }

func declareCompliance(r *Registry) {
	r.Declare(Setting{Key: KeyComplianceMonitoring, Scope: ScopeHub, Default: ComplianceOff,
		Choices: []string{ComplianceOff, ComplianceActions, ComplianceOutput},
		Help: "Record the sessions that pass through the Hub and send the record to compliance_endpoint: off; actions " +
			"(who did what to which session, when, with sizes and outcomes, no content); output (actions plus the screens, " +
			"captures and turns the Hub returned to callers). What happens in a local terminal, on the Worker's own control " +
			"socket, and what a program prints that nobody read through the Hub is not recorded. Changed only with a fresh " +
			"sign-in; every change is audited and shown to users"})
	r.Declare(Setting{Key: KeyComplianceInput, Scope: ScopeHub, Default: SinkOff, Choices: []string{SinkOff, SinkOn},
		Help: "Also record what the Hub accepted to send into sessions (text, keys, answers, tasks, messages, the command " +
			"of a new session): off or on. Needs compliance_monitoring. Typed and pasted secrets are in it: echo-off " +
			"protects typed passwords in screens, not in input, and only Minion's own key formats are masked"})
	r.Declare(Setting{Key: KeyComplianceEndpoint, Scope: ScopeHub,
		Help: "OTLP/HTTP base URL of the compliance collector, such as https://collector.example.com:4318 (/v1/logs is appended): " +
			"https, or http to a loopback address (a collector on this host). Never falls back to otel_endpoint. A destination " +
			"that is not loopback needs compliance_headers or a client certificate"})
	r.Declare(Setting{Key: KeyComplianceHeaders, Scope: ScopeHub, Secret: true, RegularFileOnly: true,
		Help: "HTTP headers of every compliance request, one \"Name: value\" per line (for example an Authorization header); encrypted at rest"})
	r.Declare(Setting{Key: KeyComplianceCAFile, Scope: ScopeHub, Env: "MINION_COMPLIANCE_CA_PATH",
		Help: "PEM file with the CA certificates that sign the compliance collector's certificate; unset: system roots"})
	r.Declare(Setting{Key: KeyComplianceClientCertFile, Scope: ScopeHub, Env: "MINION_COMPLIANCE_CLIENT_CERT_PATH",
		Help: "PEM client certificate for mutual TLS to the compliance collector; needs compliance_client_key_file"})
	r.Declare(Setting{Key: KeyComplianceClientKeyFile, Scope: ScopeHub, Env: "MINION_COMPLIANCE_CLIENT_KEY_PATH",
		Help: "PEM private key of compliance_client_cert_file; mode 0600, owned by the user that runs Minion"})
}
