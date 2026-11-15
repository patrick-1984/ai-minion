package web

import (
	"net/http"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/ops"
)

func sampleDiagnostics() ops.HubDiagnosticsResult {
	return ops.HubDiagnosticsResult{
		Version: "v0.9.0", Commit: "abc1234", StartedAt: adminTime, UptimeSeconds: 3725,
		PublicURL: "https://hub.example", TLS: false, TrustedProxies: []string{"10.0.0.0/8"},
		WorkersConnected: 3, WorkersPending: 1,
		Request: ops.HubDiagnosticsRequest{
			PeerAddress: "10.1.2.3", PeerTrustedProxy: true, ClientIP: "203.0.113.7",
			ForwardedHeaders: []ops.HubDiagnosticsHeader{{Name: "X-Forwarded-For", Value: "203.0.113.7"}, {Name: "X-Forwarded-Proto", Value: "https"}},
			Scheme:           "https", EffectiveHTTPS: true,
		},
	}
}

func TestDiagnosticsIsForAdminsOnly(t *testing.T) {
	f := newFakeOps().reply(ops.OpHubDiagnostics, sampleDiagnostics())
	connect(t, f)
	s := adminServer(t)
	if rec := get(t, s, pathAdminDiagnostics, "X-Test-Session", userHeader); rec.Code != http.StatusNotFound {
		t.Errorf("GET as user = %d, want 404", rec.Code)
	}
	if f.total() != 0 {
		t.Errorf("a user's request reached the Hub operations: %+v", f.calls)
	}
	mustNotHave(t, get(t, s, "/", "X-Test-Session", userHeader).Body.String(), pathAdminDiagnostics)
	mustHave(t, get(t, s, "/", "X-Test-Session", adminHeader).Body.String(), `href="/admin/diagnostics">`, `<span>Diagnostics</span>`)
}

func TestDiagnosticsShowsTheHubAndTheRequest(t *testing.T) {
	f := newFakeOps().reply(ops.OpHubDiagnostics, sampleDiagnostics())
	connect(t, f)
	s := adminServer(t)
	rec := get(t, s, pathAdminDiagnostics, "X-Test-Session", adminHeader)
	if rec.Code != 200 {
		t.Fatalf("GET = %d\n%s", rec.Code, rec.Body)
	}
	mustHave(t, rec.Body.String(),
		"<dt>Version</dt><dd>v0.9.0</dd>", "<dt>Commit</dt><dd>abc1234</dd>", "<dt>Uptime</dt><dd>1h2m5s</dd>",
		"<dt>Public URL</dt><dd>https://hub.example</dd>", "<dt>Hub terminates TLS</dt><dd>no</dd>",
		"<dt>Trusted proxies</dt><dd>10.0.0.0/8</dd>", "<dt>Workers connected</dt><dd>3</dd>",
		"<dt>Workers waiting for approval</dt><dd>1</dd>",
		"<dt>Peer address</dt><dd>10.1.2.3</dd>", "<dt>Peer is a trusted proxy</dt><dd>yes: the forwarding headers are honoured</dd>",
		"<dt>Client IP</dt><dd>203.0.113.7</dd>",
		"<dt>Forwarding headers received</dt><dd>X-Forwarded-For: 203.0.113.7; X-Forwarded-Proto: https</dd>",
		"<dt>Effective scheme</dt><dd>https</dd>", "<dt>Effective HTTPS</dt><dd>yes</dd>")
	if f.total() != 1 {
		t.Errorf("calls = %+v, want one hub_diagnostics", f.calls)
	}

	d := sampleDiagnostics()
	d.PublicURL, d.TrustedProxies = "", nil
	d.Request = ops.HubDiagnosticsRequest{PeerAddress: "198.51.100.9", ClientIP: "198.51.100.9", Scheme: "http",
		ForwardedHeaders: []ops.HubDiagnosticsHeader{{Name: "X-Forwarded-Proto", Value: "https"}}}
	connect(t, newFakeOps().reply(ops.OpHubDiagnostics, d))
	body := get(t, s, pathAdminDiagnostics, "X-Test-Session", adminHeader).Body.String()
	mustHave(t, body, "<dt>Public URL</dt><dd>not set (MINION_PUBLIC_URL)</dd>",
		"<dt>Trusted proxies</dt><dd>none (MINION_TRUSTED_PROXIES)</dd>",
		"<dt>Peer is a trusted proxy</dt><dd>no: the forwarding headers are ignored</dd>",
		"<dt>Effective HTTPS</dt><dd>no</dd>")
}
