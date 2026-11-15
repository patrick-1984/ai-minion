package api

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/auth/session"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// gateCall is a browser call to a Hub behind a proxy the Hub does not trust:
// the browser spoke HTTPS to the proxy, the Hub sees cleartext.
func gateCall(origin, contentType string, authAge time.Duration) *Call {
	r := httptest.NewRequest("POST", "http://hub.example.com/api/v1/ops/hub_identity_reissue", nil)
	r.Header.Set("Content-Type", contentType)
	r.Header.Set("Origin", origin)
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	sess := &session.Session{AuthAt: time.Now().Add(-authAge), AuthMethod: session.AuthMethodLocal}
	return &Call{Identity: Identity{Via: ViaSession, Session: sess}, Request: r}
}

func gateCode(err error) proto.Code {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}

func TestIdentityGateTrustedProxyConfirmation(t *testing.T) {
	g := IdentityGate{EffectiveHTTPS: func(*http.Request) bool { return false }}
	const own = "https://hub.example.com"
	cases := []struct {
		name    string
		call    *Call
		confirm bool
		want    proto.Code // "" means accepted
	}{
		{"without the confirmation the https origin is not this site", gateCall(own, "application/json", time.Second), false, proto.CodeForbidden},
		{"without the confirmation cleartext is tls_required", gateCall("http://hub.example.com", "application/json", time.Second), false, proto.CodeTLSRequired},
		{"confirmed: accepted", gateCall(own, "application/json", time.Second), true, ""},
		{"confirmed: the http origin is still accepted", gateCall("http://hub.example.com", "application/json", time.Second), true, ""},
		{"confirmed: a foreign origin is refused", gateCall("https://evil.example", "application/json", time.Second), true, proto.CodeForbidden},
		{"confirmed: another port is refused", gateCall("https://hub.example.com:8443", "application/json", time.Second), true, proto.CodeForbidden},
		{"confirmed: a stale sign-in needs re-authentication", gateCall(own, "application/json", 6*time.Minute), true, proto.CodeReauthRequired},
		{"confirmed: a form body is refused", gateCall(own, "application/x-www-form-urlencoded", time.Second), true, proto.CodeForbidden},
	}
	for _, c := range cases {
		if got := gateCode(g.Check(c.call, c.confirm)); got != c.want {
			t.Errorf("%s: code %q, want %q", c.name, got, c.want)
		}
	}
	// A cross-site fetch stays refused.
	c := gateCall(own, "application/json", time.Second)
	c.Request.Header.Set("Sec-Fetch-Site", "cross-site")
	if got := gateCode(g.Check(c, true)); got != proto.CodeForbidden {
		t.Errorf("cross-site with confirmation: code %q", got)
	}
}
