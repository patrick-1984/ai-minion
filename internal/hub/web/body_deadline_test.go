package web

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/auth/session"
	"github.com/patrick-1984/ai-minion/internal/hub/authz"
)

type noOpCaller struct{}

func (noOpCaller) CallOp(*http.Request, string, any, any) error { return nil }

// An admin form is read under a body deadline: a client that stops sending the
// form does not hold the connection. The route is served here by a plain
// http.Server that has no read timeout and no body guard of its own.
func TestAdminFormStalledBodyIsCut(t *testing.T) {
	prev := adminFormBodyTimeout
	adminFormBodyTimeout = 200 * time.Millisecond
	t.Cleanup(func() { adminFormBodyTimeout = prev })
	SetAdminOps(noOpCaller{})
	t.Cleanup(func() { SetAdminOps(nil) })
	h := http.NewServeMux()
	h.Handle("POST /admin/x", adminRoute(func(w http.ResponseWriter, r *http.Request, _ OpCaller) {
		w.WriteHeader(http.StatusNoContent)
	}))
	admin := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r = r.WithContext(session.WithSession(r.Context(), &session.Session{Username: "a", Role: authz.RoleAdmin, CSRFToken: "c"}))
		h.ServeHTTP(w, r)
	})
	ts := httptest.NewUnstartedServer(admin)
	ts.Config.ReadHeaderTimeout = 10 * time.Second
	ts.Start()
	t.Cleanup(ts.Close)

	for name, req := range map[string]string{
		// ParseForm does not read the body of a content type it does not know:
		// the declared body is still unread when it returns.
		"a form that stops":             "POST /admin/x HTTP/1.1\r\nHost: x\r\nContent-Type: application/x-www-form-urlencoded\r\nContent-Length: 300\r\n\r\nusername=bob&ro",
		"a complete form, tail missing": "POST /admin/x HTTP/1.1\r\nHost: x\r\nContent-Type: application/x-www-form-urlencoded\r\nContent-Length: 300\r\n\r\nusername=bob",
		"JSON that ParseForm ignores":   "POST /admin/x HTTP/1.1\r\nHost: x\r\nContent-Type: application/json\r\nContent-Length: 300\r\n\r\n{\"a\":1}",
		"multipart that stops":          "POST /admin/x HTTP/1.1\r\nHost: x\r\nContent-Type: multipart/form-data; boundary=zz\r\nContent-Length: 300\r\n\r\n--zz\r\n",
		"chunked, last chunk withheld":  "POST /admin/x HTTP/1.1\r\nHost: x\r\nContent-Type: text/plain\r\nTransfer-Encoding: chunked\r\n\r\n3\r\nabc\r\n",
	} {
		t.Run(name, func(t *testing.T) {
			c, err := net.Dial("tcp", ts.Listener.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = c.Close() }()
			start := time.Now()
			_, _ = io.WriteString(c, req)
			_ = c.SetReadDeadline(start.Add(5 * time.Second))
			_, err = io.Copy(io.Discard, c)
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				t.Fatalf("the admin route held an unfinished body for %v", time.Since(start))
			}
		})
	}
}
