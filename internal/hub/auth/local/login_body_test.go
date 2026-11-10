package local

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Security regression (finding "Unauthenticated login bodies have no read
// deadline"): the login handler is safe on its own, behind any server. With a
// plain http.Server that has the Hub's header and idle timeouts and no read
// timeout, a client that sends headers and part of a body and stops used to
// hold the connection and the handler until it closed the socket.
func loginServer(t *testing.T, bodyTimeout time.Duration) *httptest.Server {
	t.Helper()
	f := newFixture(t)
	f.handler.BodyTimeout = bodyTimeout
	ts := httptest.NewUnstartedServer(f.mux)
	ts.Config.ReadHeaderTimeout = 10 * time.Second
	ts.Config.IdleTimeout = 2 * time.Minute
	ts.Start()
	t.Cleanup(ts.Close)
	return ts
}

func TestLoginStalledBodyIsCutByItsOwnDeadline(t *testing.T) {
	const timeout = 300 * time.Millisecond
	ts := loginServer(t, timeout)
	for name, req := range map[string]string{
		"json":    "POST /auth/login HTTP/1.1\r\nHost: x\r\nContent-Type: application/json\r\nContent-Length: 400\r\n\r\n{\"username\":\"alice\",\"pass",
		"form":    "POST /auth/login HTTP/1.1\r\nHost: x\r\nContent-Type: application/x-www-form-urlencoded\r\nContent-Length: 400\r\n\r\nusername=alice&pass",
		"nothing": "POST /auth/login HTTP/1.1\r\nHost: x\r\nContent-Type: application/json\r\nContent-Length: 400\r\n\r\n",
		// One complete JSON object does not make the body complete.
		"complete object, tail withheld": "POST /auth/login HTTP/1.1\r\nHost: x\r\nContent-Type: application/json\r\nContent-Length: 400\r\n\r\n{\"username\":\"alice\",\"password\":\"x\"}",
		"chunked, last chunk withheld":   "POST /auth/login HTTP/1.1\r\nHost: x\r\nContent-Type: application/json\r\nTransfer-Encoding: chunked\r\n\r\n2\r\n{}\r\n",
		"chunked":                        "POST /auth/login HTTP/1.1\r\nHost: x\r\nContent-Type: application/json\r\nTransfer-Encoding: chunked\r\n\r\n8\r\n{\"userna\r\n",
	} {
		t.Run(name, func(t *testing.T) {
			c, err := net.Dial("tcp", ts.Listener.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = c.Close() }()
			start := time.Now()
			_, _ = io.WriteString(c, req)
			_ = c.SetReadDeadline(start.Add(timeout + 3*time.Second))
			_, err = io.Copy(io.Discard, c)
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				t.Fatalf("the handler held a stalled login body for %v (deadline %v)", time.Since(start), timeout)
			}
			if d := time.Since(start); d < timeout/2 {
				t.Fatalf("the connection ended after %v, before the deadline %v", d, timeout)
			}
		})
	}
}

// A client that sends the whole body in time is served, and the deadline is
// lifted afterwards: the connection can idle for longer than the deadline.
func TestLoginBodyDeadlineIsLiftedOnceTheBodyIsRead(t *testing.T) {
	const timeout = 300 * time.Millisecond
	ts := loginServer(t, timeout)
	c, err := net.Dial("tcp", ts.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	br := bufio.NewReader(c)
	body := fmt.Sprintf(`{"username":"alice","password":%q}`, "not the password")
	send := func() int {
		t.Helper()
		_, _ = fmt.Fprintf(c, "POST /auth/login HTTP/1.1\r\nHost: x\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n", len(body))
		time.Sleep(timeout / 4) // slow, but in time
		_, _ = io.WriteString(c, body)
		_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
		resp, err := http.ReadResponse(br, nil)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	if code := send(); code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (a wrong password, so the body was parsed)", code)
	}
	time.Sleep(3 * timeout) // the lifted deadline must not close the idle connection
	if code := send(); code != http.StatusUnauthorized {
		t.Fatalf("second login on the same connection = %d", code)
	}
}

// Anything but white space after the login object is refused, and a login is
// never answered from a body that was not read to its end.
func TestLoginRefusesDataAfterTheObject(t *testing.T) {
	f := newFixture(t)
	good := `{"username":"alice","password":"` + alicePassword + `"}`
	if rec := f.post("/auth/login", good+" \n", "application/json", nil); rec.Code != http.StatusOK {
		t.Fatalf("object and white space = %d %s", rec.Code, rec.Body)
	}
	if rec := f.post("/auth/login", good+`{"x":1}`, "application/json", nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("object and a second value = %d, want 400", rec.Code)
	}
	if rec := f.post("/auth/login", good+"x", "application/json", nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("object and a stray byte = %d, want 400", rec.Code)
	}
	// Total size is strict: the cap makes a longer body an error, not an end.
	if rec := f.post("/auth/login", good+strings.Repeat(" ", maxLoginBody), "application/json", nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("object and %d bytes of white space = %d, want 400", maxLoginBody, rec.Code)
	}
}
