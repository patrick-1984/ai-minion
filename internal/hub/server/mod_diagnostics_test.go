package server

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"github.com/patrick-1984/ai-minion/internal/hub/audit"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/hub/workers"
	"github.com/patrick-1984/ai-minion/internal/ops"
)

func diagNetwork(t *testing.T, proxies string) *Network {
	t.Helper()
	p, err := ParseTrustedProxies(proxies)
	if err != nil {
		t.Fatal(err)
	}
	nw, err := NewNetwork("https://hub.example", false, p)
	if err != nil {
		t.Fatal(err)
	}
	return nw
}

func forwarded(remote string) *http.Request {
	r := httptest.NewRequest("POST", "/api/v1/ops/hub_diagnostics", nil)
	r.RemoteAddr = remote
	r.Header.Set("X-Forwarded-For", "203.0.113.7")
	r.Header.Set("X-Forwarded-Proto", "https")
	return r
}

// The request facts for a direct request, a request through a trusted proxy
// and one from a peer that is not a trusted proxy.
func TestDiagnoseDirectTrustedAndUntrustedProxy(t *testing.T) {
	nw := diagNetwork(t, "10.0.0.0/8")

	direct := httptest.NewRequest("GET", "/", nil)
	direct.RemoteAddr = "192.0.2.5:5555"
	direct.TLS = &tls.ConnectionState{}
	d := nw.Diagnose(direct)
	if d.PeerAddress != "192.0.2.5" || d.ClientIP != "192.0.2.5" || d.PeerTrustedProxy ||
		d.Scheme != "https" || !d.EffectiveHTTPS || len(d.ForwardedHeaders) != 0 {
		t.Errorf("direct = %+v", d)
	}

	d = nw.Diagnose(forwarded("10.1.2.3:4444"))
	if d.PeerAddress != "10.1.2.3" || !d.PeerTrustedProxy || d.ClientIP != "203.0.113.7" ||
		d.Scheme != "https" || !d.EffectiveHTTPS || len(d.ForwardedHeaders) != 2 ||
		d.ForwardedHeaders[0] != (ops.HubDiagnosticsHeader{Name: "X-Forwarded-For", Value: "203.0.113.7"}) {
		t.Errorf("trusted proxy = %+v", d)
	}

	d = nw.Diagnose(forwarded("198.51.100.9:4444"))
	if d.PeerAddress != "198.51.100.9" || d.PeerTrustedProxy || d.ClientIP != "198.51.100.9" ||
		d.Scheme != "http" || d.EffectiveHTTPS || len(d.ForwardedHeaders) != 2 {
		t.Errorf("untrusted proxy = %+v (the headers are reported but not honoured)", d)
	}
}

// The request log writes one line per request at debug level only, with the
// scheme decision and without the query.
func TestRequestDebugLog(t *testing.T) {
	nw := diagNetwork(t, "10.0.0.0/8")
	for _, level := range []slog.Level{slog.LevelInfo, slog.LevelDebug} {
		var buf bytes.Buffer
		log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: level}))
		served := false
		h := requestDebugLog(nw, log)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { served = true }))
		r := forwarded("10.1.2.3:4444")
		r.URL.RawQuery = "code=secret-code"
		h.ServeHTTP(httptest.NewRecorder(), r)
		if !served {
			t.Fatal("the request was not served")
		}
		got := buf.String()
		if level == slog.LevelInfo {
			if got != "" {
				t.Errorf("info level wrote %q", got)
			}
			continue
		}
		for _, want := range []string{"http request", "peer=10.1.2.3", "peer_trusted_proxy=true", "client_ip=203.0.113.7",
			"forwarded_headers=X-Forwarded-For,X-Forwarded-Proto", "scheme=https", "effective_https=true"} {
			if !strings.Contains(got, want) {
				t.Errorf("debug line %q lacks %q", got, want)
			}
		}
		if strings.Contains(got, "secret-code") {
			t.Errorf("the query reached the log: %q", got)
		}
	}
}

func TestCountWorkers(t *testing.T) {
	if c, p := countWorkers(nil); c != 0 || p != 0 {
		t.Errorf("empty = %d, %d", c, p)
	}
	if c, p := countWorkers([]workers.Online{{}, {Pending: true}, {}}); c != 3 || p != 1 {
		t.Errorf("three with one pending = %d, %d", c, p)
	}
}

// On the real Hub hub_diagnostics is for admins only and reports how the Hub
// sees the request: here a direct HTTPS request from loopback that carries
// forwarding headers, which are reported but not honoured (no trusted proxy).
func TestHubDiagnosticsOnTheRealHub(t *testing.T) {
	ah := startRemoveHub(t)
	ah.user(t, "alice")
	op := func(user string) apiResp {
		tok, csrf := ah.login(t, user)
		h := asSession(tok, csrf)
		h["X-Forwarded-For"] = "203.0.113.7"
		return ah.call(t, "POST", "/api/v1/ops/"+ops.OpHubDiagnostics, map[string]any{}, h)
	}
	if r := op("alice"); r.code != 403 {
		t.Fatalf("hub_diagnostics as a user = %d %s, want 403", r.code, r.body)
	}
	r := op("root")
	if r.code != 200 {
		t.Fatalf("hub_diagnostics as admin = %d %s", r.code, r.body)
	}
	var res ops.HubDiagnosticsResult
	if err := json.Unmarshal([]byte(r.body), &res); err != nil {
		t.Fatal(err)
	}
	peer, err := netip.ParseAddr(res.Request.PeerAddress)
	if err != nil || !peer.IsLoopback() || res.Request.ClientIP != res.Request.PeerAddress || res.Request.PeerTrustedProxy ||
		res.Request.Scheme != "https" || !res.Request.EffectiveHTTPS || !res.TLS || res.Version == "" ||
		len(res.Request.ForwardedHeaders) != 1 || res.Request.ForwardedHeaders[0].Value != "203.0.113.7" ||
		res.TrustedProxies == nil || res.WorkersConnected != 0 {
		t.Errorf("hub_diagnostics = %s", r.body)
	}
	if res.StartedAt.Time().IsZero() || res.UptimeSeconds < 0 {
		t.Errorf("started_at/uptime = %s", r.body)
	}
}

func TestRequestIDMiddlewareSetsHeaderAndContext(t *testing.T) {
	var got string
	h := requestIDMiddleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { got = audit.RequestID(r.Context()) }))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Request-Id", "client-chosen")
	h.ServeHTTP(rec, req)
	if got == "" || got == "client-chosen" || rec.Header().Get("X-Request-Id") != got {
		t.Errorf("ctx id %q, header %q", got, rec.Header().Get("X-Request-Id"))
	}
}
