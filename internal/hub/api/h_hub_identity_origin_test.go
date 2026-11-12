package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// F7: the Origin must equal the whole origin (scheme, host, port) of the
// request as the Hub sees it, or the configured public URL.
func TestIdentityGateComparesFullCanonicalOrigins(t *testing.T) {
	cases := []struct {
		name, origin, host string
		https              bool
		trusted            []string
		want               bool
	}{
		{"same scheme and host", "https://hub.example.com", "hub.example.com", true, nil, true},
		{"default port written out", "https://hub.example.com:443", "hub.example.com", true, nil, true},
		{"case differs", "HTTPS://Hub.Example.com", "hub.example.com", true, nil, true},
		{"http origin to an https hub", "http://hub.example.com", "hub.example.com", true, nil, false},
		{"https origin to a plain http hub", "https://hub.example.com", "hub.example.com", false, nil, false},
		{"other port", "https://hub.example.com:8443", "hub.example.com", true, nil, false},
		{"other port in Host", "https://hub.example.com", "hub.example.com:8443", true, nil, false},
		{"explicit port both sides", "https://hub.example.com:8443", "hub.example.com:8443", true, nil, true},
		{"public URL", "https://hub.example.com", "10.0.0.5:8080", true, []string{"https://hub.example.com/"}, true},
		{"public URL with another scheme", "http://hub.example.com", "10.0.0.5:8080", true, []string{"https://hub.example.com"}, false},
		{"public URL with another port", "https://hub.example.com:444", "10.0.0.5:8080", true, []string{"https://hub.example.com"}, false},
		{"origin with a path", "https://hub.example.com/x", "hub.example.com", true, nil, false},
		{"origin with user info", "https://u@hub.example.com", "hub.example.com", true, nil, false},
		{"null", "null", "hub.example.com", true, nil, false},
		{"another site", "https://evil.example", "hub.example.com", true, nil, false},
	}
	for _, c := range cases {
		g := IdentityGate{TrustedOrigins: c.trusted, EffectiveHTTPS: func(*http.Request) bool { return c.https }}
		r := httptest.NewRequest("POST", "http://"+c.host+"/api/v1/ops/x", nil)
		r.Host = c.host
		r.Header.Set("Origin", c.origin)
		if got := g.sameOrigin(r); got != c.want {
			t.Errorf("%s: sameOrigin = %v, want %v", c.name, got, c.want)
		}
	}
}

// The cleartext management listener is accepted by EffectiveHTTPS, but the
// browser's Origin there is http.
func TestIdentityGateSchemeOverrideForTheManagementListener(t *testing.T) {
	g := IdentityGate{EffectiveHTTPS: func(*http.Request) bool { return true }, Scheme: func(*http.Request) string { return "http" }}
	r := httptest.NewRequest("POST", "http://127.0.0.1:8444/x", nil)
	r.Header.Set("Origin", "http://127.0.0.1:8444")
	if !g.sameOrigin(r) {
		t.Error("management origin refused")
	}
	r.Header.Set("Origin", "https://127.0.0.1:8444")
	if g.sameOrigin(r) {
		t.Error("an https origin on the cleartext listener was accepted")
	}
}
