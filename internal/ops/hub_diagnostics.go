package ops

import "reflect"

// OpHubDiagnostics is the admin operation that shows how the Hub runs and how
// it sees the request that asks: for troubleshooting a reverse proxy, the
// public URL or Workers that do not connect.
const OpHubDiagnostics = "hub_diagnostics"

// HubDiagnosticsArgs are the arguments of hub_diagnostics (none).
type HubDiagnosticsArgs struct{}

// HubDiagnosticsHeader is one forwarding header of the request as received.
type HubDiagnosticsHeader struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// HubDiagnosticsRequest is how the Hub sees the request of hub_diagnostics
// itself, with the trusted-proxy rules applied.
type HubDiagnosticsRequest struct {
	// PeerAddress is the address of the direct peer of the connection.
	PeerAddress string `json:"peer_address"`
	// PeerTrustedProxy says the peer is inside the trusted-proxy networks, so
	// its forwarding headers are honoured.
	PeerTrustedProxy bool `json:"peer_trusted_proxy"`
	// ClientIP is the client address as the Hub resolves it.
	ClientIP string `json:"client_ip"`
	// ForwardedHeaders are the forwarding headers received (X-Forwarded-For,
	// X-Forwarded-Proto, X-Forwarded-Host, X-Real-Ip, Forwarded), whether
	// honoured or not.
	ForwardedHeaders []HubDiagnosticsHeader `json:"forwarded_headers"`
	// Scheme is the effective scheme, https or http.
	Scheme string `json:"scheme"`
	// EffectiveHTTPS says the request counts as HTTPS (for example for the
	// passphrase operations).
	EffectiveHTTPS bool `json:"effective_https"`
}

// HubDiagnosticsResult is the result of hub_diagnostics.
type HubDiagnosticsResult struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
	// StartedAt is when this Hub process started; UptimeSeconds counts from
	// it.
	StartedAt     Timestamp `json:"started_at"`
	UptimeSeconds int64     `json:"uptime_seconds"`
	// PublicURL is MINION_PUBLIC_URL, empty when not set.
	PublicURL string `json:"public_url"`
	// TLS says the Hub terminates TLS itself (MINION_TLS is not off).
	TLS bool `json:"tls"`
	// TrustedProxies is MINION_TRUSTED_PROXIES as parsed (CIDRs).
	TrustedProxies []string              `json:"trusted_proxies"`
	Request        HubDiagnosticsRequest `json:"request"`
	// WorkersConnected counts the Workers connected now; WorkersPending
	// those of them that wait for approval.
	WorkersConnected int `json:"workers_connected"`
	WorkersPending   int `json:"workers_pending"`
}

func init() {
	Register(Op{
		Name: OpHubDiagnostics,
		Summary: "Show the Hub's version, uptime, public URL and trusted proxies, how it sees this request " +
			"(peer, client IP, forwarded headers, effective scheme) and how many Workers are connected.",
		Scope: ScopeHub, Actions: MustActionSet(ActionManage),
		Args: reflect.TypeFor[HubDiagnosticsArgs](), Result: reflect.TypeFor[HubDiagnosticsResult](),
	})
}
