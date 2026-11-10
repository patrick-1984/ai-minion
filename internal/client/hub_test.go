package client

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// hubFixture is a Hub stand-in over TLS (httptest) with a key file and the CA
// file of its certificate.
type hubFixture struct {
	srv     *httptest.Server
	key     string
	keyFile string
	caFile  string
	hits    atomic.Int32
}

func writeFile(t *testing.T, dir, name string, data []byte) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func certPEM(t *testing.T, srv *httptest.Server) []byte {
	t.Helper()
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
}

func newHubFixture(t *testing.T, h func(f *hubFixture, w http.ResponseWriter, r *http.Request)) *hubFixture {
	t.Helper()
	f := &hubFixture{key: identity.NewAPIKey().String()}
	f.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.hits.Add(1)
		// Read the body first: the server notices a vanished client only
		// once the body is consumed.
		body, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(body))
		h(f, w, r)
	}))
	t.Cleanup(f.srv.Close)
	dir := t.TempDir()
	f.keyFile = writeFile(t, dir, "key", []byte(f.key+"\n"))
	f.caFile = writeFile(t, dir, "ca.pem", certPEM(t, f.srv))
	return f
}

func (f *hubFixture) cfg() HubConfig {
	return HubConfig{URL: f.srv.URL, APIKeyFile: f.keyFile, CAFile: f.caFile}
}

func (f *hubFixture) hub(t *testing.T, opts ...HubOption) *HubBackend {
	t.Helper()
	b, err := NewHub(f.cfg(), opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(b.Close)
	return b
}

// otherCAPEM returns a self-signed CA certificate that signed nothing here.
func otherCAPEM(t *testing.T) []byte {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(7), Subject: pkix.Name{CommonName: "some other CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func envelopeJSON(code proto.Code, msg string) string {
	b, _ := json.Marshal(map[string]any{"error": map[string]any{"code": code, "message": msg, "details": map[string]any{}}})
	return string(b)
}

func jsonReply(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

func ndjson(w http.ResponseWriter) http.Flusher {
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(http.StatusOK)
	fl := w.(http.Flusher)
	fl.Flush()
	return fl
}

func TestHubRequestShape(t *testing.T) {
	type seen struct {
		method, path, rawQuery, auth, ctype, accept string
		authCount                                   int
		body                                        string
		fullURL                                     string
	}
	var got seen
	f := newHubFixture(t, func(f *hubFixture, w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = seen{r.Method, r.URL.Path, r.URL.RawQuery, r.Header.Get("Authorization"), r.Header.Get("Content-Type"),
			r.Header.Get("Accept"), len(r.Header.Values("Authorization")), string(b), r.URL.String()}
		jsonReply(w, 200, `{"sessions":[]}`)
	})
	cfg := f.cfg()
	cfg.URL += "/hub/prefix/" // a reverse proxy prefix, with a trailing slash
	b, err := NewHub(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	res, err := b.Call(context.Background(), ops.OpSessionsList, nil)
	if err != nil {
		t.Fatal(err)
	}
	if string(res) != `{"sessions":[]}` {
		t.Errorf("result = %s", res)
	}
	if got.method != "POST" || got.path != "/hub/prefix/api/v1/ops/sessions_list" || got.rawQuery != "" {
		t.Errorf("request line = %s %s ?%s", got.method, got.path, got.rawQuery)
	}
	if got.auth != "Bearer "+f.key || got.authCount != 1 {
		t.Errorf("Authorization is not exactly one Bearer header with the key (%d headers)", got.authCount)
	}
	if got.ctype != "application/json" || got.body != "{}" {
		t.Errorf("content type %q, body %q", got.ctype, got.body)
	}
	if strings.Contains(got.fullURL, f.key) {
		t.Error("the key is in the URL")
	}
}

func TestHubSendsTheArgumentsAsTheBodyObject(t *testing.T) {
	var body string
	f := newHubFixture(t, func(f *hubFixture, w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		jsonReply(w, 200, `{"outcome":"delivered"}`)
	})
	_, err := f.hub(t).Call(context.Background(), ops.OpSendText,
		map[string]any{"request_id": "r1", "target": map[string]any{"worker_id": "w", "session_name": "s"}, "text": "hi"})
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if json.Unmarshal([]byte(body), &m) != nil || m["text"] != "hi" || m["request_id"] != "r1" {
		t.Errorf("body = %s", body)
	}
}

func TestHubKeyIsReadFromTheFileForEveryRequest(t *testing.T) {
	var auths []string
	var mu sync.Mutex
	f := newHubFixture(t, func(f *hubFixture, w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		auths = append(auths, r.Header.Get("Authorization"))
		mu.Unlock()
		jsonReply(w, 200, `{}`)
	})
	b := f.hub(t)
	if _, err := b.Call(context.Background(), ops.OpSessionsList, nil); err != nil {
		t.Fatal(err)
	}
	rotated := identity.NewAPIKey().String()
	if err := os.WriteFile(f.keyFile, []byte(rotated), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Call(context.Background(), ops.OpSessionsList, nil); err != nil {
		t.Fatal(err)
	}
	if len(auths) != 2 || auths[0] != "Bearer "+f.key || auths[1] != "Bearer "+rotated {
		t.Errorf("Authorization headers = %v", auths)
	}
	// The key file is gone: the call fails before anything is sent.
	hits := f.hits.Load()
	_ = os.Remove(f.keyFile)
	if _, err := b.Call(context.Background(), ops.OpSessionsList, nil); !errors.Is(err, ErrAPIKeyFile) || f.hits.Load() != hits {
		t.Errorf("missing key file: %v (hits %d, want %d)", err, f.hits.Load(), hits)
	}
}

func TestHubErrorEnvelopeMapsToTheCodeOfTheCatalog(t *testing.T) {
	for _, tc := range []struct {
		status int
		code   proto.Code
	}{
		{401, proto.CodeUnauthenticated}, {403, proto.CodeForbidden}, {404, proto.CodeNotFound},
		{400, proto.CodeInvalidArgument}, {409, proto.CodeBusy}, {429, proto.CodeRateLimited},
		{503, proto.CodeWorkerOffline}, {501, proto.CodeUnsupportedOp},
	} {
		f := newHubFixture(t, func(f *hubFixture, w http.ResponseWriter, r *http.Request) {
			jsonReply(w, tc.status, envelopeJSON(tc.code, "the Hub says no"))
		})
		_, err := f.hub(t).Call(context.Background(), ops.OpSessionsList, nil)
		var pe *proto.Error
		if !errors.As(err, &pe) || pe.Code != tc.code || pe.Message != "the Hub says no" {
			t.Errorf("status %d: error = %v", tc.status, err)
		}
		if errors.Is(err, ErrConnectionLost) {
			t.Errorf("status %d: a definite refusal reads as unknown", tc.status)
		}
	}
}

func TestHubWriteThatMayHaveRunIsUnknownNotUnsent(t *testing.T) {
	write := map[string]any{"request_id": "r", "target": map[string]any{"session_name": "s"}, "text": "x"}
	cases := map[string]func(f *hubFixture, w http.ResponseWriter, r *http.Request){
		"connection_lost envelope": func(f *hubFixture, w http.ResponseWriter, r *http.Request) {
			jsonReply(w, 502, envelopeJSON(proto.CodeConnectionLost, "lost"))
		},
		"internal envelope": func(f *hubFixture, w http.ResponseWriter, r *http.Request) {
			jsonReply(w, 500, envelopeJSON(proto.CodeInternal, "boom"))
		},
		"proxy page": func(f *hubFixture, w http.ResponseWriter, r *http.Request) {
			http.Error(w, "<html>bad gateway</html>", http.StatusBadGateway)
		},
		"connection dropped after the request": func(f *hubFixture, w http.ResponseWriter, r *http.Request) {
			_, _ = io.Copy(io.Discard, r.Body)
			c, _, _ := w.(http.Hijacker).Hijack()
			_ = c.Close()
		},
		"body cut": func(f *hubFixture, w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Length", "1000")
			w.WriteHeader(200)
			_, _ = io.WriteString(w, `{"outcome":`)
			c, _, _ := w.(http.Hijacker).Hijack()
			_ = c.Close()
		},
	}
	for name, h := range cases {
		f := newHubFixture(t, h)
		_, err := f.hub(t).Call(context.Background(), ops.OpSendText, write)
		if !errors.Is(err, ErrConnectionLost) {
			t.Errorf("%s: error = %v, want the outcome unknown", name, err)
		}
		if n := f.hits.Load(); n != 1 {
			t.Errorf("%s: the Hub saw %d requests; a write is never sent twice", name, n)
		}
	}
}

func TestHubDoesNotRetryAnyRequestOnATransportFailure(t *testing.T) {
	f := newHubFixture(t, func(f *hubFixture, w http.ResponseWriter, r *http.Request) {
		c, _, _ := w.(http.Hijacker).Hijack()
		_ = c.Close()
	})
	b := f.hub(t)
	for i := 0; i < 3; i++ {
		before := f.hits.Load()
		if _, err := b.Call(context.Background(), ops.OpSessionsList, nil); err == nil {
			t.Fatal("expected an error")
		}
		if got := f.hits.Load() - before; got != 1 {
			t.Fatalf("call %d reached the Hub %d times", i, got)
		}
	}
}

func TestHubNothingSentWhenTheConnectionCannotBeMade(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close() // nothing listens there now
	dir := t.TempDir()
	b, err := NewHub(HubConfig{URL: "https://" + addr, APIKeyFile: writeFile(t, dir, "k", []byte(identity.NewAPIKey().String()))})
	if err != nil {
		t.Fatal(err)
	}
	_, err = b.Call(context.Background(), ops.OpSendText, map[string]any{"request_id": "r"})
	if !errors.Is(err, ErrHubUnreachable) || errors.Is(err, ErrConnectionLost) {
		t.Errorf("error = %v, want a definite not-sent", err)
	}
}

func TestHubCallTimeoutAfterSendIsUnknownForAWrite(t *testing.T) {
	f := newHubFixture(t, func(f *hubFixture, w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	})
	b := f.hub(t, WithHubCallTimeout(150*time.Millisecond))
	_, err := b.Call(context.Background(), ops.OpSendText, map[string]any{"request_id": "r"})
	if !errors.Is(err, ErrConnectionLost) {
		t.Errorf("error = %v, want the outcome unknown", err)
	}
	if f.hits.Load() != 1 {
		t.Errorf("hits = %d", f.hits.Load())
	}
}

func TestHubCallerCancelReturnsTheContextError(t *testing.T) {
	started := make(chan struct{})
	f := newHubFixture(t, func(f *hubFixture, w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
	})
	ctx, cancel := context.WithCancel(context.Background())
	go func() { <-started; cancel() }()
	_, err := f.hub(t).Call(ctx, ops.OpSendText, map[string]any{"request_id": "r"})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v", err)
	}
}

func TestHubRedirectIsNeverFollowedAndTheKeyStaysHome(t *testing.T) {
	var foreignHits atomic.Int32
	var foreignAuth atomic.Value
	foreign := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		foreignHits.Add(1)
		foreignAuth.Store(r.Header.Get("Authorization"))
		jsonReply(w, 200, `{}`)
	}))
	defer foreign.Close()
	for _, status := range []int{301, 302, 307, 308} {
		f := newHubFixture(t, func(f *hubFixture, w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, foreign.URL+"/api/v1/ops/send_text", status)
		})
		cfg := f.cfg()
		cfg.CAFile, cfg.Insecure = "", true // two private certificates: skip verification for the test
		b, err := NewHub(cfg)
		if err != nil {
			t.Fatal(err)
		}
		_, err = b.Call(context.Background(), ops.OpSendText, map[string]any{"request_id": "r"})
		if !errors.Is(err, ErrHubRedirect) || errors.Is(err, ErrConnectionLost) {
			t.Errorf("status %d: error = %v", status, err)
		}
		if strings.Contains(err.Error(), f.key) || strings.Contains(err.Error(), foreign.URL) {
			t.Errorf("status %d: the error repeats a secret or the redirect target: %v", status, err)
		}
		if f.hits.Load() != 1 {
			t.Errorf("status %d: the origin saw %d requests", status, f.hits.Load())
		}
		b.Close()
	}
	if foreignHits.Load() != 0 {
		t.Errorf("the redirect target saw %d requests (Authorization %v)", foreignHits.Load(), foreignAuth.Load())
	}
}

func TestHubTLSVerifiesByDefaultAndSendsNothingToAnUntrustedHub(t *testing.T) {
	f := newHubFixture(t, func(f *hubFixture, w http.ResponseWriter, r *http.Request) { jsonReply(w, 200, `{}`) })
	cfg := f.cfg()
	cfg.CAFile = "" // system roots only
	b, err := NewHub(cfg)
	if err != nil {
		t.Fatal(err)
	}
	_, err = b.Call(context.Background(), ops.OpSendText, map[string]any{"request_id": "r"})
	if !errors.Is(err, ErrHubCertificate) || errors.Is(err, ErrConnectionLost) {
		t.Errorf("error = %v", err)
	}
	if !strings.Contains(err.Error(), "--hub-ca-file") {
		t.Errorf("the error does not say how to fix it: %v", err)
	}
	if f.hits.Load() != 0 {
		t.Errorf("a request reached a Hub whose certificate was not trusted")
	}
}

func TestHubTLSPrivateCAAcceptsItsHubAndRejectsOthers(t *testing.T) {
	f := newHubFixture(t, func(f *hubFixture, w http.ResponseWriter, r *http.Request) { jsonReply(w, 200, `{"ok":true}`) })
	if _, err := f.hub(t).Call(context.Background(), ops.OpSessionsList, nil); err != nil {
		t.Fatalf("with its CA: %v", err)
	}
	// Some other CA: unknown authority. (httptest serves one built-in
	// certificate to every server, so the other CA is generated.)
	cfg := f.cfg()
	cfg.CAFile = writeFile(t, t.TempDir(), "other.pem", otherCAPEM(t))
	b, err := NewHub(cfg)
	if err != nil {
		t.Fatal(err)
	}
	hits := f.hits.Load()
	if _, err := b.Call(context.Background(), ops.OpSessionsList, nil); !errors.Is(err, ErrHubCertificate) || f.hits.Load() != hits {
		t.Errorf("wrong CA: %v (hits %d -> %d)", err, hits, f.hits.Load())
	}
	// Right CA, wrong host name: refused too.
	cfg = f.cfg()
	_, port, _ := net.SplitHostPort(f.srv.Listener.Addr().String())
	cfg.URL = "https://localhost:" + port
	b, err = NewHub(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Call(context.Background(), ops.OpSessionsList, nil); !errors.Is(err, ErrHubCertificate) {
		t.Errorf("wrong host name: %v", err)
	}
}

func TestHubCAFileProblemsAreTypedErrors(t *testing.T) {
	dir := t.TempDir()
	key := writeFile(t, dir, "k", []byte(identity.NewAPIKey().String()))
	for name, ca := range map[string]string{
		"missing":     filepath.Join(dir, "none.pem"),
		"no cert":     writeFile(t, dir, "junk.pem", []byte("CANARY-NOT-A-CERT")),
		"a directory": dir,
		"too large":   writeFile(t, dir, "big.pem", bytes.Repeat([]byte("x"), maxCAFileBytes+1)),
		"empty":       writeFile(t, dir, "empty.pem", nil),
	} {
		_, err := NewHub(HubConfig{URL: "https://hub.example", APIKeyFile: key, CAFile: ca})
		if !errors.Is(err, ErrHubCA) {
			t.Errorf("%s: error = %v", name, err)
		}
		if err != nil && strings.Contains(err.Error(), "CANARY") {
			t.Errorf("%s: the error repeats the file content", name)
		}
	}
	if _, err := NewHub(HubConfig{URL: "https://hub.example", APIKeyFile: key, CAFile: "x", Insecure: true}); !errors.Is(err, ErrHubTLSConflict) {
		t.Errorf("CA and insecure: %v", err)
	}
}

func TestHubInsecureSkipsVerificationWarnsAndChangesNothingGlobal(t *testing.T) {
	f := newHubFixture(t, func(f *hubFixture, w http.ResponseWriter, r *http.Request) { jsonReply(w, 200, `{"ok":true}`) })
	def := http.DefaultTransport.(*http.Transport)
	defCfg := def.TLSClientConfig
	var warns []string
	cfg := f.cfg()
	cfg.CAFile, cfg.Insecure = "", true
	b, err := NewHub(cfg, WithHubWarn(func(s string) { warns = append(warns, s) }))
	if err != nil {
		t.Fatal(err)
	}
	if len(warns) != 1 || !strings.Contains(warns[0], "--hub-insecure") || !strings.Contains(warns[0], "does not verify") {
		t.Errorf("warnings = %q", warns)
	}
	if _, err := b.Call(context.Background(), ops.OpSessionsList, nil); err != nil {
		t.Fatal(err)
	}
	// Nothing global changed: the default transport is untouched and a plain
	// client still refuses the same certificate.
	if def.TLSClientConfig != defCfg || (def.TLSClientConfig != nil && def.TLSClientConfig.InsecureSkipVerify) {
		t.Error("the default transport's TLS configuration changed")
	}
	if resp, err := http.Get(f.srv.URL); err == nil {
		_ = resp.Body.Close()
		t.Error("a default client accepted a certificate it must not trust")
	}
	// Without the switch there is no warning.
	warns = nil
	if _, err := NewHub(f.cfg(), WithHubWarn(func(s string) { warns = append(warns, s) })); err != nil || len(warns) != 0 {
		t.Errorf("warnings without the switch: %q, %v", warns, err)
	}
}

func TestHubURLPolicy(t *testing.T) {
	dir := t.TempDir()
	key := writeFile(t, dir, "k", []byte(identity.NewAPIKey().String()))
	for _, tc := range []struct {
		url  string
		want error
	}{
		{"", ErrHubURL},
		{"hub.example", ErrHubURL},
		{"ftp://hub.example", ErrHubURL},
		{"https://", ErrHubURL},
		{"https://user:pw@hub.example", ErrHubURL},
		{"https://hub.example/?token=1", ErrHubURL},
		{"https://hub.example/#frag", ErrHubURL},
		{"http://hub.example", ErrHubInsecureScheme},
		{"http://192.168.1.5:8080", ErrHubInsecureScheme},
		{"wss://hub.example", ErrHubURL},
		{"https://hub.example", nil},
		{"https://hub.example:8443/prefix", nil},
		{"http://localhost:8080", nil},
		{"http://127.0.0.1:8080", nil},
		{"http://[::1]:8080", nil},
	} {
		_, err := NewHub(HubConfig{URL: tc.url, APIKeyFile: key})
		if tc.want == nil && err != nil || tc.want != nil && !errors.Is(err, tc.want) {
			t.Errorf("%q: error = %v, want %v", tc.url, err, tc.want)
		}
		if err != nil && (strings.Contains(err.Error(), "pw@") || strings.Contains(err.Error(), "token=1")) {
			t.Errorf("%q: the error repeats part of the URL: %v", tc.url, err)
		}
	}
	if _, err := NewHub(HubConfig{URL: "https://hub.example"}); !errors.Is(err, ErrAPIKeyMissing) {
		t.Errorf("no key file: %v", err)
	}
}

func TestHubPlainHTTPToThisMachineWorks(t *testing.T) {
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		jsonReply(w, 200, `{}`)
	}))
	defer srv.Close()
	key := identity.NewAPIKey().String()
	b, err := NewHub(HubConfig{URL: srv.URL, APIKeyFile: writeFile(t, t.TempDir(), "k", []byte(key))})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Call(context.Background(), ops.OpSessionsList, nil); err != nil || auth != "Bearer "+key {
		t.Errorf("err %v, auth %q", err, auth)
	}
}

func TestHubAPIKeyFileChecks(t *testing.T) {
	dir := t.TempDir()
	good := identity.NewAPIKey().String()
	for name, content := range map[string][]byte{
		"empty":            nil,
		"only a newline":   []byte("\n"),
		"not a key":        []byte("CANARY-PASSWORD-123"),
		"key plus text":    []byte(good + " extra"),
		"two lines":        []byte(good + "\n" + good),
		"leading space":    []byte(" " + good),
		"too large":        bytes.Repeat([]byte("a"), maxAPIKeyFileBytes+1),
		"header injection": []byte(good + "\r\nX-Evil: 1"),
	} {
		p := writeFile(t, dir, strings.ReplaceAll(name, " ", "_"), content)
		_, err := NewHub(HubConfig{URL: "https://hub.example", APIKeyFile: p})
		if !errors.Is(err, ErrAPIKeyFile) {
			t.Errorf("%s: error = %v", name, err)
		}
		if err != nil && (strings.Contains(err.Error(), "CANARY") || strings.Contains(err.Error(), good)) {
			t.Errorf("%s: the error repeats the file content: %v", name, err)
		}
	}
	// A directory and a missing file.
	for name, p := range map[string]string{"directory": dir, "missing": filepath.Join(dir, "nope")} {
		if _, err := NewHub(HubConfig{URL: "https://hub.example", APIKeyFile: p}); !errors.Is(err, ErrAPIKeyFile) {
			t.Errorf("%s: error = %v", name, err)
		}
	}
	// CRLF and one trailing newline are accepted, and a symbolic link is followed.
	crlf := writeFile(t, dir, "crlf", []byte(good+"\r\n"))
	if _, err := NewHub(HubConfig{URL: "https://hub.example", APIKeyFile: crlf}); err != nil {
		t.Errorf("CRLF: %v", err)
	}
}

func TestHubScrubsTheKeyFromWhatTheOtherSideSays(t *testing.T) {
	for name, h := range map[string]func(f *hubFixture, w http.ResponseWriter, r *http.Request){
		"enveloped message": func(f *hubFixture, w http.ResponseWriter, r *http.Request) {
			jsonReply(w, 401, envelopeJSON(proto.CodeUnauthenticated, "bad key "+f.key+" / "+r.Header.Get("Authorization")))
		},
		"enveloped details": func(f *hubFixture, w http.ResponseWriter, r *http.Request) {
			jsonReply(w, 400, `{"error":{"code":"invalid_argument","message":"x","details":{"echo":"`+f.key+`"}}}`)
		},
		"proxy page": func(f *hubFixture, w http.ResponseWriter, r *http.Request) {
			http.Error(w, "<html>CANARY-PAGE "+f.key+"</html>", http.StatusBadGateway)
		},
		"key shaped text of another key": func(f *hubFixture, w http.ResponseWriter, r *http.Request) {
			jsonReply(w, 403, envelopeJSON(proto.CodeForbidden, "key "+identity.NewAPIKey().String()+" may not"))
		},
		"control sequences": func(f *hubFixture, w http.ResponseWriter, r *http.Request) {
			jsonReply(w, 403, envelopeJSON(proto.CodeForbidden, "\x1b[2J\x1b]52;c;ZXZpbA==\x07forbidden"))
		},
	} {
		f := newHubFixture(t, h)
		_, err := f.hub(t).Call(context.Background(), ops.OpSessionsList, nil)
		if err == nil {
			t.Fatalf("%s: no error", name)
		}
		text := err.Error()
		var pe *proto.Error
		if errors.As(err, &pe) {
			text += string(pe.Details)
		}
		for _, bad := range []string{f.key, "Bearer ", "CANARY-PAGE", "\x1b", "mn_"} {
			if strings.Contains(text, bad) {
				t.Errorf("%s: the error contains %q: %q", name, bad, text)
			}
		}
	}
}

func TestHubTransportErrorsDoNotCarryTheKeyOrTheFullURL(t *testing.T) {
	f := newHubFixture(t, func(f *hubFixture, w http.ResponseWriter, r *http.Request) {
		c, _, _ := w.(http.Hijacker).Hijack()
		_ = c.Close()
	})
	cfg := f.cfg()
	cfg.URL += "/secret-path-token"
	b, err := NewHub(cfg)
	if err != nil {
		t.Fatal(err)
	}
	_, err = b.Call(context.Background(), ops.OpSessionsList, nil)
	if err == nil || strings.Contains(err.Error(), f.key) || strings.Contains(err.Error(), "secret-path-token") {
		t.Errorf("error = %v", err)
	}
}

func TestHubBoundsTheAnswers(t *testing.T) {
	big := newHubFixture(t, func(f *hubFixture, w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(bytes.Repeat([]byte(" "), maxResultBytes+1024))
	})
	if _, err := big.hub(t).Call(context.Background(), ops.OpSessionsList, nil); !errors.Is(err, ErrProtocol) {
		t.Errorf("oversized result: %v", err)
	}
	var written atomic.Int64
	errBody := newHubFixture(t, func(f *hubFixture, w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(500)
		chunk := bytes.Repeat([]byte("e"), 32<<10)
		for i := 0; i < 2000; i++ { // 64 MiB offered
			n, err := w.Write(chunk)
			written.Add(int64(n))
			if err != nil {
				return
			}
		}
	})
	_, err := errBody.hub(t).Call(context.Background(), ops.OpSessionsList, nil)
	if err == nil {
		t.Fatal("no error")
	}
	if len(err.Error()) > 4<<10 {
		t.Errorf("error text of %d bytes", len(err.Error()))
	}
	if written.Load() > 8<<20 {
		t.Errorf("the client let the Hub send %d bytes of an error body", written.Load())
	}
	notJSON := newHubFixture(t, func(f *hubFixture, w http.ResponseWriter, r *http.Request) { jsonReply(w, 200, "<html>") })
	if _, err := notJSON.hub(t).Call(context.Background(), ops.OpSessionsList, nil); !errors.Is(err, ErrProtocol) {
		t.Errorf("not JSON: %v", err)
	}
}

func TestHubDefaultWorkerIsAddedOnlyWhereTheArgumentsNameNone(t *testing.T) {
	var body map[string]any
	f := newHubFixture(t, func(f *hubFixture, w http.ResponseWriter, r *http.Request) {
		body = nil
		_ = json.NewDecoder(r.Body).Decode(&body)
		jsonReply(w, 200, `{}`)
	})
	const def, other = "01930000-0000-7000-8000-000000000001", "01930000-0000-7000-8000-000000000002"
	cfg := f.cfg()
	cfg.Worker = def
	b, err := NewHub(cfg)
	if err != nil {
		t.Fatal(err)
	}
	call := func(op string, args any) map[string]any {
		t.Helper()
		if _, err := b.Call(context.Background(), op, args); err != nil {
			t.Fatal(err)
		}
		return body
	}
	if got := call(ops.OpSessionsList, nil); got["worker_id"] != def {
		t.Errorf("sessions_list body = %v", got)
	}
	if got := call(ops.OpSessionsList, map[string]any{"worker_id": other}); got["worker_id"] != other {
		t.Errorf("an explicit worker_id was replaced: %v", got)
	}
	tgt := func(m map[string]any) map[string]any { return m["target"].(map[string]any) }
	if got := call(ops.OpSessionSnapshot, map[string]any{"target": map[string]any{"session_name": "s"}}); tgt(got)["worker_id"] != def || tgt(got)["session_name"] != "s" {
		t.Errorf("a target without Worker: %v", got)
	}
	if got := call(ops.OpSessionSnapshot, map[string]any{"target": map[string]any{"worker_id": other, "session_name": "s"}}); tgt(got)["worker_id"] != other {
		t.Errorf("a qualified target was retargeted: %v", got)
	}
	if got := call(ops.OpSessionSnapshot, map[string]any{"target": "lab:s"}); got["target"] != "lab:s" || got["worker_id"] != nil {
		t.Errorf("a human-form target was changed: %v", got)
	}
	if got := call(ops.OpWorkersList, nil); got["worker_id"] != nil {
		t.Errorf("a Hub op got a worker_id: %v", got)
	}
}

func TestHubStreamDeliversElementsAsTheyArriveAndIgnoresKeepAliveLines(t *testing.T) {
	release := make(chan struct{})
	f := newHubFixture(t, func(f *hubFixture, w http.ResponseWriter, r *http.Request) {
		fl := ndjson(w)
		_, _ = io.WriteString(w, "\n\n"+`{"turn_id":"1","text":"a"}`+"\n")
		fl.Flush()
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		_, _ = io.WriteString(w, "\n"+`{"turn_id":"2","text":"b"}`+"\n")
	})
	b := f.hub(t, WithHubCallTimeout(50*time.Millisecond)) // a scalar bound that must not apply to a stream
	first := make(chan string, 1)
	var got []string
	done := make(chan error, 1)
	go func() {
		_, err := b.Stream(context.Background(), ops.OpPipeWait, map[string]any{"target": map[string]any{"session_name": "s"}}, func(el json.RawMessage) error {
			got = append(got, string(el))
			if len(got) == 1 {
				first <- string(el)
			}
			return nil
		})
		done <- err
	}()
	select {
	case el := <-first:
		if el != `{"turn_id":"1","text":"a"}` {
			t.Errorf("first element = %s", el)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the first element did not arrive while the stream was still open")
	}
	time.Sleep(200 * time.Millisecond) // longer than the scalar bound: the stream stays up
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[1] != `{"turn_id":"2","text":"b"}` {
		t.Errorf("elements = %q", got)
	}
}

func TestHubStreamWaitLongerThanTheCallTimeoutIsLegitimate(t *testing.T) {
	f := newHubFixture(t, func(f *hubFixture, w http.ResponseWriter, r *http.Request) {
		ndjson(w)
		time.Sleep(400 * time.Millisecond)
		_, _ = io.WriteString(w, `{"turn_id":"1"}`+"\n")
	})
	b := f.hub(t, WithHubCallTimeout(100*time.Millisecond))
	var n int
	if _, err := b.Stream(context.Background(), ops.OpPipeWait, map[string]any{}, func(json.RawMessage) error { n++; return nil }); err != nil || n != 1 {
		t.Errorf("err %v, %d elements", err, n)
	}
	// Call on a streaming op discards the elements and succeeds the same way.
	if _, err := b.Call(context.Background(), ops.OpPipeWait, map[string]any{}); err != nil {
		t.Errorf("Call on a stream op: %v", err)
	}
}

func TestHubStreamErrorLineEndsTheStreamWithTheCode(t *testing.T) {
	f := newHubFixture(t, func(f *hubFixture, w http.ResponseWriter, r *http.Request) {
		fl := ndjson(w)
		_, _ = io.WriteString(w, `{"turn_id":"1"}`+"\n")
		fl.Flush()
		_, _ = io.WriteString(w, envelopeJSON(proto.CodeForbidden, "your access ended")+"\n")
	})
	var n int
	_, err := f.hub(t).Stream(context.Background(), ops.OpPipeWait, map[string]any{}, func(json.RawMessage) error { n++; return nil })
	if proto.CodeOf(err) != proto.CodeForbidden || n != 1 || errors.Is(err, ErrConnectionLost) {
		t.Errorf("err %v, %d elements", err, n)
	}
}

func TestHubStreamFailureBeforeItStartsIsTheStatusEnvelope(t *testing.T) {
	f := newHubFixture(t, func(f *hubFixture, w http.ResponseWriter, r *http.Request) {
		jsonReply(w, 404, envelopeJSON(proto.CodeNotFound, "not found"))
	})
	_, err := f.hub(t).Stream(context.Background(), ops.OpPipeWait, map[string]any{}, nil)
	if proto.CodeOf(err) != proto.CodeNotFound {
		t.Errorf("err = %v", err)
	}
}

func TestHubStreamCancellationStopsTheRequest(t *testing.T) {
	gone := make(chan struct{})
	f := newHubFixture(t, func(f *hubFixture, w http.ResponseWriter, r *http.Request) {
		fl := ndjson(w)
		_, _ = io.WriteString(w, `{"turn_id":"1"}`+"\n")
		fl.Flush()
		<-r.Context().Done()
		close(gone)
	})
	ctx, cancel := context.WithCancel(context.Background())
	_, err := f.hub(t).Stream(ctx, ops.OpPipeWait, map[string]any{}, func(json.RawMessage) error {
		cancel()
		time.Sleep(50 * time.Millisecond)
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v", err)
	}
	select {
	case <-gone:
	case <-time.After(5 * time.Second):
		t.Error("the Hub did not see the client go")
	}
}

func TestHubStreamCallbackErrorStopsIt(t *testing.T) {
	f := newHubFixture(t, func(f *hubFixture, w http.ResponseWriter, r *http.Request) {
		fl := ndjson(w)
		for i := 0; i < 5; i++ {
			_, _ = io.WriteString(w, `{"turn_id":"x"}`+"\n")
			fl.Flush()
		}
		<-r.Context().Done()
	})
	stop := errors.New("enough")
	var n int
	_, err := f.hub(t).Stream(context.Background(), ops.OpPipeWait, map[string]any{}, func(json.RawMessage) error { n++; return stop })
	if !errors.Is(err, stop) || n != 1 {
		t.Errorf("err %v after %d elements", err, n)
	}
}

func TestHubStreamBounds(t *testing.T) {
	for name, tc := range map[string]struct {
		h    func(f *hubFixture, w http.ResponseWriter, r *http.Request)
		want error
	}{
		"line too long": {func(f *hubFixture, w http.ResponseWriter, r *http.Request) {
			ndjson(w)
			_, _ = w.Write(append(bytes.Repeat([]byte("a"), maxStreamLineBytes+100), '\n'))
		}, ErrProtocol},
		"no newline for ever": {func(f *hubFixture, w http.ResponseWriter, r *http.Request) {
			ndjson(w)
			chunk := bytes.Repeat([]byte("a"), 64<<10)
			for i := 0; i < 200; i++ {
				if _, err := w.Write(chunk); err != nil {
					return
				}
			}
		}, ErrProtocol},
		"not JSON": {func(f *hubFixture, w http.ResponseWriter, r *http.Request) {
			ndjson(w)
			_, _ = io.WriteString(w, "hello\n")
		}, ErrProtocol},
		"wrong content type": {func(f *hubFixture, w http.ResponseWriter, r *http.Request) {
			jsonReply(w, 200, `{"turn_id":"1"}`)
		}, ErrProtocol},
		"cut in a line": {func(f *hubFixture, w http.ResponseWriter, r *http.Request) {
			ndjson(w)
			_, _ = io.WriteString(w, `{"turn_id":"1`)
		}, ErrConnectionLost},
		"connection dropped": {func(f *hubFixture, w http.ResponseWriter, r *http.Request) {
			fl := ndjson(w)
			_, _ = io.WriteString(w, `{"turn_id":"1"}`+"\n")
			fl.Flush()
			c, _, _ := w.(http.Hijacker).Hijack()
			_ = c.Close()
		}, ErrConnectionLost},
		"unknown error code": {func(f *hubFixture, w http.ResponseWriter, r *http.Request) {
			ndjson(w)
			_, _ = io.WriteString(w, `{"error":{"code":"from_the_future","message":"CANARY"}}`+"\n")
		}, ErrProtocol},
	} {
		f := newHubFixture(t, tc.h)
		_, err := f.hub(t).Stream(context.Background(), ops.OpPipeWait, map[string]any{}, nil)
		if !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", name, err, tc.want)
		}
		if err != nil && strings.Contains(err.Error(), "CANARY") {
			t.Errorf("%s: the error echoes the Hub's text", name)
		}
	}
}

func TestHubSafeForConcurrentUse(t *testing.T) {
	f := newHubFixture(t, func(f *hubFixture, w http.ResponseWriter, r *http.Request) { jsonReply(w, 200, `{"ok":true}`) })
	b := f.hub(t)
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 5; j++ {
				if _, err := b.Call(context.Background(), ops.OpSessionsList, nil); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	wg.Wait()
}

func TestHubRejectsAnUnusableOpOrArguments(t *testing.T) {
	f := newHubFixture(t, func(f *hubFixture, w http.ResponseWriter, r *http.Request) { jsonReply(w, 200, `{}`) })
	b := f.hub(t)
	for _, op := range []string{"", "../x", "a/b", "A", "x y", "a?b"} {
		if _, err := b.Call(context.Background(), op, nil); proto.CodeOf(err) != proto.CodeInvalidArgument {
			t.Errorf("op %q: %v", op, err)
		}
	}
	if _, err := b.Call(context.Background(), ops.OpSessionsList, []int{1}); proto.CodeOf(err) != proto.CodeInvalidArgument {
		t.Errorf("array arguments: %v", err)
	}
	huge := map[string]string{"text": strings.Repeat("a", maxRequestBytes)}
	if _, err := b.Call(context.Background(), ops.OpSendText, huge); proto.CodeOf(err) != proto.CodeMessageTooLarge {
		t.Errorf("huge request: %v", err)
	}
	if f.hits.Load() != 0 {
		t.Errorf("%d unusable requests were sent", f.hits.Load())
	}
}
