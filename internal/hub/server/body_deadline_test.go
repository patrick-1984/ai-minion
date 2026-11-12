package server

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"github.com/patrick-1984/ai-minion/internal/testutil/hubstore"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/hub/workers"
	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/testutil/stallh2"
	"github.com/patrick-1984/ai-minion/internal/transport"
)

// Security regression (finding "Unauthenticated login bodies have no read
// deadline"): a client that sends complete headers and a partial body, and
// then stops, must not hold the connection and the handler of ANY route,
// authenticated or not, for longer than the body deadline. These tests run the
// production composition (Run, the real auth module, TLS with HTTP/1.1 and
// HTTP/2, and plain HTTP behind a proxy) with a short body deadline.
const (
	testBodyTimeout = 400 * time.Millisecond
	// cutWithin is how long the client waits for the Hub to give up on a
	// stalled body: the deadline plus the time TLS, the scheduler and a slow CI
	// machine take.
	cutWithin = testBodyTimeout + 4*time.Second
)

// bodyRegistry is the Hub of the auth module plus routes that behave like the
// long-lived and the body-reading routes of the real Hub.
func bodyRegistry(t *testing.T) *Registry {
	t.Helper()
	var svc authServices
	r := authRegistry(t, &svc)
	err := r.Register("bodyprobe", 70, func(_ context.Context, env *Env) error {
		mux, err := Require[*http.ServeMux](env)
		if err != nil {
			return err
		}
		// An event stream: headers, then idle until the client goes away.
		mux.HandleFunc("GET /idle", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, ": open\n\n")
			w.(http.Flusher).Flush()
			select {
			case <-r.Context().Done():
			case <-time.After(time.Minute):
			}
		})
		// The Worker's WebSocket: the connection is taken over and idles.
		mux.HandleFunc("GET /hijack", func(w http.ResponseWriter, r *http.Request) {
			conn, buf, err := http.NewResponseController(w).Hijack()
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			defer func() { _ = conn.Close() }()
			_, _ = buf.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: test\r\n\r\n")
			_ = buf.Flush()
			_, _ = io.Copy(io.Discard, conn)
		})
		mux.HandleFunc("POST /slurp", func(w http.ResponseWriter, r *http.Request) {
			n, err := io.Copy(io.Discard, r.Body)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			w.Header().Set("X-Read", fmt.Sprint(n))
			w.WriteHeader(http.StatusNoContent)
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func bodyHubTLS(t *testing.T) *running {
	t.Helper()
	t.Setenv("MINION_ADMIN_USERNAME", "root")
	t.Setenv("MINION_ADMIN_PASSWORD", "integration-password-1")
	return startHub(t, &Config{DataDir: hubstore.Dir(t)}, bodyRegistry(t), WithBodyTimeout(testBodyTimeout))
}

func bodyHubPlain(t *testing.T) *running {
	t.Helper()
	t.Setenv("MINION_ADMIN_USERNAME", "root")
	t.Setenv("MINION_ADMIN_PASSWORD", "integration-password-1")
	cfg := &Config{DataDir: hubstore.Dir(t), TLSOff: true, PublicURL: "https://hub.example.com", TrustedProxies: mustPrefixes(t, "127.0.0.0/8, ::1")}
	return startHub(t, cfg, bodyRegistry(t), WithBodyTimeout(testBodyTimeout))
}

func dialH1(t *testing.T, addr string, useTLS bool) net.Conn {
	t.Helper()
	d := net.Dialer{Timeout: 3 * time.Second}
	var (
		c   net.Conn
		err error
	)
	if useTLS {
		c, err = tls.DialWithDialer(&d, "tcp", addr, &tls.Config{InsecureSkipVerify: true, NextProtos: []string{"http/1.1"}}) //nolint:gosec // self-signed test Hub
	} else {
		c, err = d.Dial("tcp", addr)
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// h1Stall sends request (headers and a part of the body) and reports how long
// the Hub takes to end the connection, and whether it did within cutWithin.
func h1Stall(t *testing.T, addr string, useTLS bool, request string) (time.Duration, bool) {
	t.Helper()
	c := dialH1(t, addr, useTLS)
	start := time.Now()
	if _, err := io.WriteString(c, request); err != nil {
		t.Fatal(err)
	}
	_ = c.SetReadDeadline(start.Add(cutWithin))
	if _, err := io.Copy(io.Discard, c); err != nil {
		if ne, ok := err.(net.Error); ok && ne.Timeout() {
			return time.Since(start), false
		}
	}
	return time.Since(start), true // EOF or reset: the Hub ended the connection
}

type stallCase struct {
	name string
	// request is the HTTP/1.1 request with headers and a partial body.
	request string
	method  string
	path    string
	hdr     map[string]string
	length  int
	part    string
}

func stallCases() []stallCase {
	const jsonLogin = `{"username":"root","password":"integration-pass`
	form := "username=root&password=integration-pass"
	mk := func(name, method, path, ct string, length int, part string) stallCase {
		hdr := map[string]string{}
		req := method + " " + path + " HTTP/1.1\r\nHost: hub.test\r\n"
		if ct != "" {
			req += "Content-Type: " + ct + "\r\n"
			hdr["content-type"] = ct
		}
		if length > 0 {
			req += fmt.Sprintf("Content-Length: %d\r\n", length)
		}
		req += "\r\n" + part
		return stallCase{name: name, request: req, method: method, path: path, hdr: hdr, length: length, part: part}
	}
	cases := []stallCase{
		mk("login json", "POST", "/auth/login", "application/json", 512, jsonLogin),
		mk("login form", "POST", "/auth/login", "application/x-www-form-urlencoded", 512, form),
		mk("login with no body byte", "POST", "/auth/login", "application/json", 512, ""),
		mk("session route", "POST", "/api/probe", "application/json", 512, `{"x":`),
		mk("route that does not exist", "POST", "/no/such/route", "application/json", 512, `{"x":`),
		mk("health probe with a body", "GET", "/healthz", "application/json", 512, `{"x":`),
		mk("GET with a body", "GET", "/api/probe", "application/json", 512, `{"x":`),
		// The body that is declared is longer than what was sent: the JSON value
		// or the form is complete, the rest never comes. A parser that stops at
		// the first complete value must not take the body for complete.
		mk("login with a complete empty object, tail withheld", "POST", "/auth/login", "application/json", 512, `{}`),
		mk("login with complete wrong credentials, tail withheld", "POST", "/auth/login", "application/json", 512, `{"username":"root","password":"wrong"}`),
		mk("login with complete right credentials, tail withheld", "POST", "/auth/login", "application/json", 512, `{"username":"root","password":"integration-password-1"}`),
		mk("login with a complete form, tail withheld", "POST", "/auth/login", "application/x-www-form-urlencoded", 512, form+"&x=1"),
		mk("login with a complete object and a space, tail withheld", "POST", "/auth/login", "application/json", 512, `{"username":"root","password":"wrong"} `),
	}
	return cases
}

// A stalled partial body ends within the deadline on every route over TLS with
// HTTP/1.1, and a chunked one too.
func TestStalledBodyIsCutOnEveryRouteOverTLSHTTP1(t *testing.T) {
	h := bodyHubTLS(t)
	addr := h.srv.Addr()
	for _, c := range stallCases() {
		t.Run(c.name, func(t *testing.T) {
			d, ok := h1Stall(t, addr, true, c.request)
			t.Logf("ended after %v", d.Round(10*time.Millisecond))
			if !ok {
				t.Fatalf("the Hub held a stalled body for %v (deadline %v)", d, testBodyTimeout)
			}
		})
	}
	t.Run("login chunked, complete object, terminating chunk withheld", func(t *testing.T) {
		for name, obj := range map[string]string{"empty": `{}`, "right credentials": `{"username":"root","password":"integration-password-1"}`} {
			body := fmt.Sprintf("%x\r\n%s\r\n", len(obj), obj) // no "0\r\n\r\n"
			req := "POST /auth/login HTTP/1.1\r\nHost: hub.test\r\nContent-Type: application/json\r\nTransfer-Encoding: chunked\r\n\r\n" + body
			if d, ok := h1Stall(t, addr, true, req); !ok {
				t.Fatalf("%s: the Hub held a chunked body without its last chunk for %v (deadline %v)", name, d, testBodyTimeout)
			}
		}
	})
	t.Run("login chunked", func(t *testing.T) {
		req := "POST /auth/login HTTP/1.1\r\nHost: hub.test\r\nContent-Type: application/json\r\nTransfer-Encoding: chunked\r\n\r\n" +
			"10\r\n{\"username\":\"roo\r\n"
		if d, ok := h1Stall(t, addr, true, req); !ok {
			t.Fatalf("the Hub held a stalled chunked body for %v (deadline %v)", d, testBodyTimeout)
		}
	})
}

func TestStalledBodyIsCutOnEveryRouteOverPlainHTTP1(t *testing.T) {
	h := bodyHubPlain(t)
	addr := h.srv.Addr()
	for _, c := range stallCases() {
		t.Run(c.name, func(t *testing.T) {
			if d, ok := h1Stall(t, addr, false, c.request); !ok {
				t.Fatalf("the Hub held a stalled body for %v (deadline %v)", d, testBodyTimeout)
			}
		})
	}
}

// HTTP/2: the stalled stream ends (a response or RST_STREAM) within the
// deadline, and the connection and its other streams go on.
func TestStalledBodyIsCutOnEveryRouteOverTLSHTTP2(t *testing.T) {
	h := bodyHubTLS(t)
	addr := h.srv.Addr()
	for _, c := range stallCases() {
		t.Run(c.name, func(t *testing.T) {
			cl := stallh2.Dial(t, addr)
			cl.PostPartial(1, c.path, c.hdr, c.length, []byte(c.part))
			start := time.Now()
			if ended, _ := cl.WaitStreamEnd(1, cutWithin); !ended {
				t.Fatalf("the Hub held a stalled HTTP/2 body for %v (deadline %v)", time.Since(start), testBodyTimeout)
			}
		})
	}
}

// What is healthy keeps working: a slow client that finishes its body in time,
// a keep-alive connection that idles after a body, an event stream and a
// taken-over (WebSocket) connection that idle for much longer than the body
// deadline. No blanket deadline is put on the connection.
func TestHealthyClientsAreNotCutByTheBodyDeadline(t *testing.T) {
	h := bodyHubTLS(t)
	addr := h.srv.Addr()

	t.Run("a slow body that finishes in time", func(t *testing.T) {
		c := dialH1(t, addr, true)
		part1 := `{"username":"root","password":"wr`
		part2 := `ong"}`
		_, _ = fmt.Fprintf(c, "POST /auth/login HTTP/1.1\r\nHost: hub.test\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s", len(part1)+len(part2), part1)
		time.Sleep(testBodyTimeout / 4)
		_, _ = io.WriteString(c, part2)
		_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
		resp, err := http.ReadResponse(bufio.NewReader(c), nil)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized { // the password is wrong: the body was parsed
			t.Fatalf("status = %d, want 401 for a wrong password", resp.StatusCode)
		}
	})

	t.Run("a keep-alive connection idles after a body", func(t *testing.T) {
		c := dialH1(t, addr, true)
		br := bufio.NewReader(c)
		body := strings.Repeat("x", 100)
		_, _ = fmt.Fprintf(c, "POST /slurp HTTP/1.1\r\nHost: hub.test\r\nContent-Length: %d\r\n\r\n%s", len(body), body)
		_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
		resp, err := http.ReadResponse(br, nil)
		if err != nil || resp.StatusCode != http.StatusNoContent || resp.Header.Get("X-Read") != "100" {
			t.Fatalf("slurp = %v, %v", resp, err)
		}
		time.Sleep(3 * testBodyTimeout) // the lifted deadline must not close the idle connection
		_, _ = io.WriteString(c, "GET /healthz HTTP/1.1\r\nHost: hub.test\r\n\r\n")
		_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
		if resp, err := http.ReadResponse(br, nil); err != nil || resp.StatusCode != 200 {
			t.Fatalf("the idle keep-alive connection was cut: %v, %v", resp, err)
		}
	})

	t.Run("an event stream idles", func(t *testing.T) {
		c := dialH1(t, addr, true)
		_, _ = io.WriteString(c, "GET /idle HTTP/1.1\r\nHost: hub.test\r\n\r\n")
		br := bufio.NewReader(c)
		_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
		if resp, err := http.ReadResponse(br, nil); err != nil || resp.StatusCode != 200 {
			t.Fatalf("open: %v, %v", resp, err)
		}
		_ = c.SetReadDeadline(time.Now().Add(5 * testBodyTimeout))
		if n, err := br.Discard(1); n == 1 && err == nil {
			_, _ = br.Discard(br.Buffered()) // the ": open" comment
		}
		_ = c.SetReadDeadline(time.Now().Add(4 * testBodyTimeout))
		if _, err := br.ReadByte(); err == nil || !isTimeout(err) {
			t.Fatalf("the event stream ended after %v of idling: %v", 4*testBodyTimeout, err)
		}
	})

	t.Run("a taken-over connection idles", func(t *testing.T) {
		c := dialH1(t, addr, true)
		_, _ = io.WriteString(c, "GET /hijack HTTP/1.1\r\nHost: hub.test\r\nConnection: Upgrade\r\nUpgrade: test\r\n\r\n")
		br := bufio.NewReader(c)
		_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
		if resp, err := http.ReadResponse(br, nil); err != nil || resp.StatusCode != 101 {
			t.Fatalf("upgrade: %v, %v", resp, err)
		}
		_ = c.SetReadDeadline(time.Now().Add(4 * testBodyTimeout))
		if _, err := br.ReadByte(); err == nil || !isTimeout(err) {
			t.Fatalf("the taken-over connection ended after %v of idling: %v", 4*testBodyTimeout, err)
		}
	})

	t.Run("HTTP/2: an idle stream lives while a stalled one is cut", func(t *testing.T) {
		cl := stallh2.Dial(t, addr)
		cl.Get(1, "/idle", nil)
		cl.ReadHeaders(1, 5*time.Second)
		cl.PostPartial(3, "/auth/login", map[string]string{"content-type": "application/json"}, 512, []byte(`{"username":"root`))
		// The stalled stream ends; stream 1 must not.
		start := time.Now()
		ended := false
		for time.Since(start) < cutWithin && !ended {
			e, _ := cl.WaitStreamEnd(3, 200*time.Millisecond)
			ended = e
		}
		if !ended {
			t.Fatal("the stalled HTTP/2 body was not cut")
		}
		if e, _ := cl.WaitStreamEnd(1, 3*testBodyTimeout); e {
			t.Fatal("the idle stream was ended together with the stalled one")
		}
	})
}

func isTimeout(err error) bool {
	ne, ok := err.(net.Error)
	return ok && ne.Timeout()
}

// The Worker's WebSocket is the longest-lived channel of the Hub. On the
// production composition with a short body deadline, a healthy Worker stays
// connected and keeps its session alive while it idles for several body
// deadlines, and a stalled body on another connection is cut at the same time.
func TestWorkerWebSocketIdlesBeyondTheBodyDeadline(t *testing.T) {
	reg := workersRegistry(t)
	var (
		svc *workers.Service
		st  *store.Store
	)
	if err := reg.Register("grab", 60, func(_ context.Context, env *Env) error {
		var err error
		if svc, err = Require[*workers.Service](env); err != nil {
			return err
		}
		st, err = Require[*store.Store](env)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	data := hubstore.Dir(t)
	h := startHub(t, &Config{DataDir: data}, reg, WithBodyTimeout(testBodyTimeout))

	wk, _ := identity.GenerateKey()
	row := store.WorkerRow{ID: store.NewID(), Pub: wk.Public(), Name: "pi", OwnerID: "owner-1", Kind: store.WorkerKindPrivate, CreatedAt: time.Now().UnixMilli()}
	if err := st.InsertWorker(context.Background(), nil, row); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, err := transport.Dial(ctx, "https://"+h.srv.Addr())
	if err != nil {
		t.Fatal(err)
	}
	res, err := transport.WorkerHandshake(conn, transport.WorkerConfig{
		Key: wk, Name: "pi", Ops: []string{"send_text"}, Mode: transport.ModeConnect,
		Pin: identity.Pin{Pub: svc.Identity().Public()}, Timeout: 10 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := proto.ReadFrame(res.Channel, proto.StreamFrameLimit); err != nil {
		t.Fatal(err)
	}
	sess, err := transport.NewSession(res.Channel, transport.RoleWorker, transport.SessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	waitUntilTrue(t, 5*time.Second, "the Worker to go online", func() bool { return svc.Registry().IsOnline(row.ID) })

	// A stalled body elsewhere is cut meanwhile.
	if d, ok := h1Stall(t, h.srv.Addr(), true, "POST /auth/login HTTP/1.1\r\nHost: hub.test\r\nContent-Length: 300\r\n\r\n{\"user"); !ok {
		t.Fatalf("a stalled body was held for %v", d)
	}
	time.Sleep(5 * testBodyTimeout) // far beyond the body deadline
	select {
	case <-sess.Done():
		t.Fatalf("the Worker session ended while idle: %v", sess.Err())
	default:
	}
	if !svc.Registry().IsOnline(row.ID) {
		t.Fatal("the Worker went offline while idle")
	}
	if _, err := sess.Ping(); err != nil {
		t.Fatalf("the Worker session does not answer a ping after idling: %v", err)
	}
}
