package transport

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

func TestConnectURL(t *testing.T) {
	for in, want := range map[string]string{
		"https://hub.example:8443":         "wss://hub.example:8443/api/v1/worker/connect",
		"https://hub.example:8443/":        "wss://hub.example:8443/api/v1/worker/connect",
		"http://10.0.0.5:8080":             "ws://10.0.0.5:8080/api/v1/worker/connect",
		"wss://hub.example":                "wss://hub.example/api/v1/worker/connect",
		"ws://hub.example/":                "ws://hub.example/api/v1/worker/connect",
		"HTTPS://hub.example/minion/":      "wss://hub.example/minion/api/v1/worker/connect",
		"https://hub.example/?x=1#frag":    "wss://hub.example/api/v1/worker/connect",
		"https://user:pw@hub.example:1234": "wss://hub.example:1234/api/v1/worker/connect",
		" https://hub.example ":            "wss://hub.example/api/v1/worker/connect",
	} {
		got, err := ConnectURL(in)
		if err != nil || got != want {
			t.Errorf("ConnectURL(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "hub.example:8443", "ftp://hub.example", "https://", "://x"} {
		if got, err := ConnectURL(bad); err == nil {
			t.Errorf("ConnectURL(%q) = %q, want an error", bad, got)
		}
	}
}

// A Hub URL may carry credentials; no error text may repeat them (NFR1).
func TestConnectURLErrorsDoNotLeakSecrets(t *testing.T) {
	for _, in := range []string{
		"https://user:secret@",
		"ftp://user:secret@x",
		"ftp://user:secret@x/path?token=secret",
		"://user:secret@x",
		"https://user:secret@host:secret",
		"https://user:secret@ho st",
		"https://user:secret@host/%zz",
		"secret",
		"user:secret@hub.example",
		"http://user:secret@[::1",
	} {
		got, err := ConnectURL(in)
		if err == nil {
			t.Errorf("ConnectURL(%q) = %q, want an error", in, got)
			continue
		}
		if strings.Contains(err.Error(), "secret") {
			t.Errorf("ConnectURL(%q) error leaks the secret: %v", in, err)
		}
		if _, derr := Dial(context.Background(), in); derr == nil || strings.Contains(derr.Error(), "secret") {
			t.Errorf("Dial(%q) error %v", in, derr)
		}
	}
}

func TestConstants(t *testing.T) {
	if ConnectPath != "/api/v1/worker/connect" || Subprotocol != "minion.v1" {
		t.Fatalf("%s %s", ConnectPath, Subprotocol)
	}
}

// ------------------------------------------------------------ test certs

// testCert returns a self-signed certificate for the given DNS name and
// validity window.
func testCert(t *testing.T, dns string, notBefore, notAfter time.Time) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: dns},
		DNSNames:     []string{dns},
		NotBefore:    notBefore,
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// hubServer is a TLS test server whose connect endpoint runs AcceptWorker and
// hands every Session to sessions.
type hubServer struct {
	*httptest.Server
	sessions chan *Session
	errs     chan error
	hubKey   *identity.Key
	hubCfg   HubConfig
}

func newHubServer(t *testing.T, cert *tls.Certificate) *hubServer {
	t.Helper()
	hub := newHub(t)
	hk := hub.root
	h := &hubServer{
		sessions: make(chan *Session, 4),
		errs:     make(chan error, 4),
		hubKey:   hk,
		hubCfg:   hub.cfg(),
	}
	h.hubCfg.Timeout = testWait
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+ConnectPath, func(w http.ResponseWriter, r *http.Request) {
		s, _, err := AcceptWorker(w, r, h.hubCfg, SessionOptions{})
		if err != nil {
			h.errs <- err
			return
		}
		h.sessions <- s
	})
	h.Server = httptest.NewUnstartedServer(mux)
	if cert != nil {
		h.TLS = &tls.Config{Certificates: []tls.Certificate{*cert}}
		h.StartTLS()
	} else {
		h.StartTLS() // httptest's own certificate
	}
	t.Cleanup(h.Close)
	return h
}

func (h *hubServer) workerCfg(t *testing.T) WorkerConfig {
	t.Helper()
	return WorkerConfig{
		Key: mustKey(t), Name: "lab-pi", Ops: []string{"snapshot"}, Mode: ModeConnect,
		Pin: identity.Pin{Pub: h.hubKey.Public()}, Timeout: testWait,
	}
}

func (h *hubServer) nextSession(t *testing.T) *Session {
	t.Helper()
	select {
	case s := <-h.sessions:
		t.Cleanup(func() { _ = s.Close() })
		return s
	case err := <-h.errs:
		t.Fatalf("hub side failed: %v", err)
	case <-time.After(testWait):
		t.Fatal("hub never produced a session")
	}
	return nil
}

// echoOps answers every op stream with an op.result carrying the request's id.
func echoOps(ctx context.Context, w *Session) {
	for {
		st, err := w.Accept(ctx)
		if err != nil {
			return
		}
		go func() {
			body, err := st.ReadFrame()
			if err != nil {
				return
			}
			var req struct {
				ID int `json:"id"`
			}
			_ = proto.DecodeFrame(body, &req)
			_ = st.WriteFrame(map[string]any{"type": proto.TypeOpResult, "id": req.ID})
			_ = st.Close()
		}()
	}
}

// The lab case of A-15: a self-signed certificate with the wrong name (and an
// expired one), no flags at all, and the Worker still connects, because trust
// comes from the handshake. 50 concurrent op streams run over the connection.
func TestWorkerConnectsDespiteAnyCertificate(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name string
		cert *tls.Certificate
	}{
		{"httptest certificate (unknown CA)", nil},
		{"self-signed wrong name", ptr(testCert(t, "wrong-name.invalid", now.Add(-time.Hour), now.Add(time.Hour)))},
		{"self-signed expired", ptr(testCert(t, "localhost", now.Add(-48*time.Hour), now.Add(-24*time.Hour)))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hub := newHubServer(t, tc.cert)

			// Control: an ordinary TLS client refuses this server.
			if resp, err := http.Get(hub.URL + "/"); err == nil {
				resp.Body.Close()
				t.Fatal("control: the default HTTP client trusted the test certificate")
			}

			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			worker, res, err := ConnectWorker(ctx, hub.URL, hub.workerCfg(t), SessionOptions{})
			if err != nil {
				t.Fatalf("connect with no flags: %v", err)
			}
			defer worker.Close()
			if res.Version != proto.Current {
				t.Fatalf("version %v", res.Version)
			}
			hs := hub.nextSession(t)
			go echoOps(ctx, worker)

			const n = 50
			var wg sync.WaitGroup
			errc := make(chan error, n)
			for i := 0; i < n; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					errc <- roundTripOp(ctx, hs, i)
				}()
			}
			wg.Wait()
			close(errc)
			for err := range errc {
				if err != nil {
					t.Fatal(err)
				}
			}

			// Worker-opened kinds reach the Hub over the same connection.
			for _, k := range []Kind{KindCall, KindEvents} {
				st, err := worker.OpenStream(ctx, k)
				if err != nil {
					t.Fatal(err)
				}
				if err := st.WriteFrame(map[string]any{"type": proto.TypeEvent}); err != nil {
					t.Fatal(err)
				}
				in, err := hs.Accept(ctx)
				if err != nil || in.Kind() != k {
					t.Fatalf("hub accept %v: %v", k, err)
				}
			}
		})
	}
}

func ptr[T any](v T) *T { return &v }

func roundTripOp(ctx context.Context, hub *Session, id int) error {
	st, err := hub.OpenOp(ctx)
	if err != nil {
		return fmt.Errorf("op %d: open: %w", id, err)
	}
	defer st.Close()
	if err := st.WriteFrame(map[string]any{"type": proto.TypeOpRequest, "id": id}); err != nil {
		return fmt.Errorf("op %d: write: %w", id, err)
	}
	body, err := st.ReadFrame()
	if err != nil {
		return fmt.Errorf("op %d: read: %w", id, err)
	}
	var res struct {
		Type string `json:"type"`
		ID   int    `json:"id"`
	}
	if err := json.Unmarshal(body, &res); err != nil || res.Type != proto.TypeOpResult || res.ID != id {
		return fmt.Errorf("op %d: got %s (%v)", id, body, err)
	}
	return nil
}

// Channel trust comes only from the handshake: a certificate that is fine does
// not rescue a Hub with the wrong key.
func TestWrongHubKeyIsRefusedDespiteTLS(t *testing.T) {
	hub := newHubServer(t, nil)
	cfg := hub.workerCfg(t)
	cfg.Pin = identity.Pin{Pub: mustKey(t).Public()}
	ctx, cancel := context.WithTimeout(context.Background(), testWait)
	defer cancel()
	_, _, err := ConnectWorker(ctx, hub.URL, cfg, SessionOptions{})
	if !errors.Is(err, ErrPinMismatch) {
		t.Fatalf("got %v, want ErrPinMismatch", err)
	}
}

func TestDialRefusedAndBadURL(t *testing.T) {
	hub := newHubServer(t, nil)
	url := hub.URL
	hub.Close()
	ctx, cancel := context.WithTimeout(context.Background(), testWait)
	defer cancel()
	if _, err := Dial(ctx, url); err == nil {
		t.Fatal("dial to a closed server succeeded")
	}
	if _, err := Dial(ctx, "ftp://x"); err == nil {
		t.Fatal("bad scheme accepted")
	}
}

func TestConnectWorkerHonoursContext(t *testing.T) {
	hub := newHubServer(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := ConnectWorker(ctx, hub.URL, hub.workerCfg(t), SessionOptions{}); err == nil {
		t.Fatal("cancelled context connected")
	}
}

func TestDialRequiresSubprotocolFromServer(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil) // selects no subprotocol
		if err == nil {
			defer c.CloseNow()
			time.Sleep(testWait / 5)
		}
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), testWait)
	defer cancel()
	_, err := Dial(ctx, srv.URL)
	if err == nil || !strings.Contains(err.Error(), Subprotocol) {
		t.Fatalf("got %v", err)
	}
}

func TestAcceptRefusesNonWebSocketAndMissingSubprotocol(t *testing.T) {
	var gotErr = make(chan error, 2)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := Accept(w, r); err != nil {
			gotErr <- err
		}
	}))
	defer srv.Close()

	resp, err := srv.Client().Get(srv.URL + ConnectPath)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode < 400 {
		t.Fatalf("plain GET answered %d", resp.StatusCode)
	}
	if err := <-gotErr; !errors.Is(err, ErrNotWorkerConnect) {
		t.Fatalf("got %v", err)
	}

	// A WebSocket without the minion.v1 subprotocol is closed.
	ctx, cancel := context.WithTimeout(context.Background(), testWait)
	defer cancel()
	c, _, err := websocket.Dial(ctx, "wss"+strings.TrimPrefix(srv.URL, "https")+ConnectPath, &websocket.DialOptions{HTTPClient: srv.Client()})
	if err == nil {
		defer c.CloseNow()
		_, _, err = c.Read(ctx)
		if err == nil {
			t.Fatal("server kept a connection without the subprotocol")
		}
	}
	if err := <-gotErr; !errors.Is(err, ErrNotWorkerConnect) {
		t.Fatalf("got %v", err)
	}
}

// The websocket carries each AEAD record as one binary message.
func TestRecordsAreBinaryMessages(t *testing.T) {
	var rawConn *websocket.Conn
	ready, stop := make(chan struct{}), make(chan struct{})
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{Subprotocol}})
		if err != nil {
			return
		}
		rawConn = c
		close(ready)
		<-stop
	}))
	defer srv.Close()
	defer close(stop)
	ctx, cancel := context.WithTimeout(context.Background(), testWait)
	defer cancel()
	conn, err := Dial(ctx, srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	<-ready
	if _, err := conn.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	typ, data, err := rawConn.Read(ctx)
	if err != nil || typ != websocket.MessageBinary || string(data) != "hello" {
		t.Fatalf("type %v data %q err %v", typ, data, err)
	}
}

// The websocket read limit must be effective on both ends: the net.Conn
// wrapper must not lift it (websocket.NetConn resets it to unlimited).
func TestWebsocketReadLimitIsEnforced(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*testWait)
	defer cancel()
	big := make([]byte, wsReadLimit+1)
	ok := make([]byte, 4+proto.HandshakeFrameLimit)

	// readOf reads the message of n bytes. For the oversized message it asks
	// for one byte more: the library reports ErrMessageTooBig on the read
	// after the last permitted byte; anything else means no limit was applied.
	readOf := func(c net.Conn, n int) error {
		_ = c.SetReadDeadline(time.Now().Add(testWait))
		if n > wsReadLimit {
			n++
		}
		_, err := io.ReadFull(c, make([]byte, n))
		return err
	}

	t.Run("dial", func(t *testing.T) {
		for name, msg := range map[string][]byte{"oversized": big, "handshake-sized": ok} {
			t.Run(name, func(t *testing.T) {
				srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					c, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{Subprotocol}})
					if err != nil {
						return
					}
					defer c.CloseNow()
					_ = c.Write(r.Context(), websocket.MessageBinary, msg)
					_, _, _ = c.Read(r.Context()) // until the client goes away
				}))
				defer srv.Close()
				conn, err := Dial(ctx, srv.URL)
				if err != nil {
					t.Fatal(err)
				}
				defer conn.Close()
				err = readOf(conn, len(msg))
				if name == "oversized" && !errors.Is(err, websocket.ErrMessageTooBig) {
					t.Fatalf("oversized message: got %v, want ErrMessageTooBig", err)
				}
				if name != "oversized" && err != nil {
					t.Fatalf("message within the limit: %v", err)
				}
			})
		}
	})

	t.Run("accept", func(t *testing.T) {
		for name, msg := range map[string][]byte{"oversized": big, "handshake-sized": ok} {
			t.Run(name, func(t *testing.T) {
				res := make(chan error, 1)
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					conn, err := Accept(w, r)
					if err != nil {
						res <- err
						return
					}
					defer conn.Close()
					res <- readOf(conn, len(msg))
				}))
				defer srv.Close()
				c, _, err := websocket.Dial(ctx, srv.URL, &websocket.DialOptions{Subprotocols: []string{Subprotocol}})
				if err != nil {
					t.Fatal(err)
				}
				defer c.CloseNow()
				_ = c.Write(ctx, websocket.MessageBinary, msg)
				err = <-res
				if name == "oversized" && !errors.Is(err, websocket.ErrMessageTooBig) {
					t.Fatalf("oversized message: got %v, want ErrMessageTooBig", err)
				}
				if name != "oversized" && err != nil {
					t.Fatalf("message within the limit: %v", err)
				}
			})
		}
	})
}
