package hubdial

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestParse(t *testing.T) {
	for _, tc := range []struct {
		in, want string
	}{
		{"", ""},
		{"   ", ""},
		{"minion.example=192.0.2.10", "minion.example=192.0.2.10"},
		{" Minion.Example. = 192.0.2.10 ", "minion.example=192.0.2.10"},
		{"hub.example=2001:db8::1", "hub.example=2001:db8::1"},
		{"hub.example=[2001:db8::1]", "hub.example=2001:db8::1"},
		{"hub.example=::ffff:10.0.0.1", "hub.example=10.0.0.1"},
		{"localhost=127.0.0.1", "localhost=127.0.0.1"},
		{"a-b.c1.example=192.0.2.2", "a-b.c1.example=192.0.2.2"},
	} {
		o, err := Parse(tc.in)
		if err != nil {
			t.Fatalf("Parse(%q): %v", tc.in, err)
		}
		got := ""
		if o != nil {
			got = o.String()
		}
		if got != tc.want {
			t.Errorf("Parse(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	for _, in := range []string{
		"minion.example", "=10.0.0.1", "minion.example=", "minion.example=hub.other",
		"10.0.0.1=10.0.0.2", "bad_name.example=10.0.0.1", "-a.example=10.0.0.1", "a..example=10.0.0.1",
		"hub.example=10.0.0.1:443", "hub.example=fe80::1%eth0", "hub.123=10.0.0.1",
		"https://hub.example=10.0.0.1", strings.Repeat("a", 64) + ".example=10.0.0.1",
	} {
		if o, err := Parse(in); err == nil {
			t.Errorf("Parse(%q) = %v, want an error", in, o)
		}
	}
}

func TestTargetOnlyNamedHostKeepsPort(t *testing.T) {
	o, err := Parse("minion.example=192.0.2.10")
	if err != nil {
		t.Fatal(err)
	}
	for in, want := range map[string]string{
		"minion.example:443":     "192.0.2.10:443",
		"MINION.example.:80":     "192.0.2.10:80",
		"minion.example:8443":    "192.0.2.10:8443",
		"other.example:443":      "other.example:443",
		"sub.minion.example:443": "sub.minion.example:443",
		"proxy.lan:3128":         "proxy.lan:3128",
		"10.0.0.9:443":           "10.0.0.9:443",
		"minion.example":         "minion.example", // no port: left to the dialer's error
	} {
		if got := o.Target(in); got != want {
			t.Errorf("Target(%q) = %q, want %q", in, got, want)
		}
	}
	v6, _ := Parse("minion.example=2001:db8::1")
	if got := v6.Target("minion.example:443"); got != "[2001:db8::1]:443" {
		t.Errorf("IPv6 Target = %q", got)
	}
}

func TestWrapPassesNetworkAndRewrittenAddress(t *testing.T) {
	o, _ := Parse("minion.example=192.0.2.10")
	var gotNet, gotAddr string
	sentinel := errors.New("recorded")
	dial := o.Wrap(func(_ context.Context, network, addr string) (net.Conn, error) {
		gotNet, gotAddr = network, addr
		return nil, sentinel
	})
	if _, err := dial(context.Background(), "tcp", "minion.example:443"); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v", err)
	}
	if gotNet != "tcp" || gotAddr != "192.0.2.10:443" {
		t.Fatalf("dialled %s %s", gotNet, gotAddr)
	}
}

func TestFailingReportsTheCause(t *testing.T) {
	_, perr := Parse("nonsense")
	if _, err := failing(perr)(context.Background(), "tcp", "x:1"); err == nil || !strings.Contains(err.Error(), EnvName) {
		t.Fatalf("err = %v", err)
	}
}

// tlsServer serves the httptest certificate (valid for example.com and
// 127.0.0.1) and records the SNI and Host it saw.
func tlsServer(t *testing.T) (srv *httptest.Server, port string, seen func() (sni, host string)) {
	t.Helper()
	var mu sync.Mutex
	var sni, host string
	srv = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		host = r.Host
		mu.Unlock()
		_, _ = io.WriteString(w, "ok")
	}))
	srv.TLS = &tls.Config{GetConfigForClient: func(h *tls.ClientHelloInfo) (*tls.Config, error) {
		mu.Lock()
		sni = h.ServerName
		mu.Unlock()
		return nil, nil
	}}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	_, port, err := net.SplitHostPort(srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	return srv, port, func() (string, string) { mu.Lock(); defer mu.Unlock(); return sni, host }
}

func clientThrough(srv *httptest.Server, o *Override) *http.Client {
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	return &http.Client{Transport: &http.Transport{
		DialContext:     o.Wrap((&net.Dialer{}).DialContext),
		TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
		Proxy:           nil,
	}}
}

func TestTLSStillVerifiesTheHostname(t *testing.T) {
	srv, port, seen := tlsServer(t)
	// example.com would resolve elsewhere: only the override reaches the server.
	o, err := Parse("example.com=127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	resp, err := clientThrough(srv, o).Get("https://example.com:" + port + "/")
	if err != nil {
		t.Fatalf("GET through the override: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if string(body) != "ok" {
		t.Fatalf("body %q", body)
	}
	sni, host := seen()
	if sni != "example.com" || host != "example.com:"+port {
		t.Fatalf("server saw SNI %q and Host %q, want the hostname", sni, host)
	}
}

func TestTLSWrongCertificateNameFails(t *testing.T) {
	srv, port, _ := tlsServer(t)
	o, err := Parse("hub.invalid-name.test=127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	_, err = clientThrough(srv, o).Get("https://hub.invalid-name.test:" + port + "/")
	var hostErr x509.HostnameError
	if err == nil || !errors.As(err, &hostErr) {
		t.Fatalf("err = %v, want an x509.HostnameError", err)
	}
}

func TestDialWith(t *testing.T) {
	if dialWith(nil, nil, nil) != nil {
		t.Fatal("no override must keep a nil dialer nil")
	}
	_, perr := Parse("minion.example=")
	if _, err := dialWith(nil, perr, nil)(context.Background(), "tcp", "minion.example:443"); err == nil {
		t.Fatal("an invalid override must fail the dial")
	}
	o, _ := Parse("minion.example=192.0.2.10")
	var got string
	_, _ = dialWith(o, nil, func(_ context.Context, _, addr string) (net.Conn, error) {
		got = addr
		return nil, errors.New("stop")
	})(context.Background(), "tcp", "minion.example:443")
	if got != "192.0.2.10:443" {
		t.Fatalf("dialled %q", got)
	}
}
