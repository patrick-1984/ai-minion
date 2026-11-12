//go:build unix

package adminsock

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/hubid"
)

type call struct {
	method string
	actor  hubid.Actor
	pass   string
	new    string
	opts   hubid.IssueOptions
}

type fakeSvc struct {
	mu    sync.Mutex
	calls []call
	err   error
}

func (f *fakeSvc) record(c call) (hubid.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, c)
	return hubid.Result{Kind: c.method, Serial: 7, RootFingerprint: "root-fp", OpFingerprint: "op-fp",
		NotBefore: time.Unix(1, 0), NotAfter: time.Unix(2, 0), Generation: 2}, f.err
}

func (f *fakeSvc) SetPassphrase(_ context.Context, a hubid.Actor, p, _ string, o hubid.IssueOptions) (hubid.Result, error) {
	return f.record(call{"passphrase_set", a, p, "", o})
}

func (f *fakeSvc) Reissue(_ context.Context, a hubid.Actor, p string, o hubid.IssueOptions) (hubid.Result, error) {
	return f.record(call{"reissue", a, p, "", o})
}

func (f *fakeSvc) RotateMaster(_ context.Context, a hubid.Actor, p, n, _ string, o hubid.IssueOptions) (hubid.Result, error) {
	return f.record(call{"rotate", a, p, n, o})
}

func (f *fakeSvc) ChangePassphrase(_ context.Context, a hubid.Actor, p, n, _ string) (hubid.Result, error) {
	return f.record(call{"passphrase_change", a, p, n, hubid.IssueOptions{}})
}

func (f *fakeSvc) Status(context.Context) (hubid.Status, error) {
	return hubid.Status{State: hubid.StateEstablished, Serving: true, Serial: 7}, f.err
}

func (f *fakeSvc) last(t *testing.T) call {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		t.Fatal("the service was not called")
	}
	return f.calls[len(f.calls)-1]
}

func (f *fakeSvc) count() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.calls) }

func shortSock(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "as")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return Path(dir)
}

func serve(t *testing.T, svc *fakeSvc, tune func(*Server)) string {
	t.Helper()
	path := shortSock(t)
	ln, err := Listen(path)
	if err != nil {
		t.Fatal(err)
	}
	srv := NewServer(svc, nil)
	if tune != nil {
		tune(srv)
	}
	done := make(chan struct{})
	go func() { defer close(done); _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close(); <-done })
	return path
}

func request(t *testing.T, sock, method, path, contentType, body string) (int, string) {
	t.Helper()
	c := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
		DisableKeepAlives: true,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", sock)
		},
	}}
	req, _ := http.NewRequest(method, "http://hub"+path, strings.NewReader(body))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestTheActorComesFromThePeerCredentialsNotFromTheClient(t *testing.T) {
	svc := &fakeSvc{}
	sock := serve(t, svc, nil)
	// A client cannot name its own actor: unknown members are refused, headers are not read.
	code, body := request(t, sock, "POST", PathReissue, "application/json", `{"passphrase":"p","actor":"local:0"}`)
	if code != 400 || svc.count() != 0 {
		t.Fatalf("unknown member = %d %s (calls %d)", code, body, svc.count())
	}
	code, body = request(t, sock, "POST", PathReissue, "application/json", `{"passphrase":"p-long-enough","validity_days":30}`)
	if code != 200 {
		t.Fatalf("reissue = %d %s", code, body)
	}
	c := svc.last(t)
	want := hubid.Actor{ID: "local:" + itoa(os.Getuid()), Channel: hubid.ChannelCLI}
	if c.actor != want || c.pass != "p-long-enough" || c.opts.ValidityDays != 30 {
		t.Errorf("call = %+v, want actor %+v", c, want)
	}
	if ActorForUID(1000) != "local:1000" {
		t.Errorf("ActorForUID = %s", ActorForUID(1000))
	}
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

func TestEveryCommandReachesItsOperationWithTheCLIChannel(t *testing.T) {
	svc := &fakeSvc{}
	sock := serve(t, svc, nil)
	for path, body := range map[string]string{
		PathSetPassphrase:    `{"passphrase":"aaaaaaaaaaaa","confirm":"aaaaaaaaaaaa"}`,
		PathReissue:          `{"passphrase":"aaaaaaaaaaaa","serial_floor":5,"allow_far_floor":true}`,
		PathRotateMaster:     `{"passphrase":"aaaaaaaaaaaa","new_passphrase":"bbbbbbbbbbbb","confirm":"bbbbbbbbbbbb","word":"ROTATE"}`,
		PathChangePassphrase: `{"passphrase":"aaaaaaaaaaaa","new_passphrase":"bbbbbbbbbbbb","confirm":"bbbbbbbbbbbb"}`,
	} {
		code, out := request(t, sock, "POST", path, "application/json", body)
		if code != 200 {
			t.Fatalf("%s = %d %s", path, code, out)
		}
		if got := svc.last(t); got.actor.Channel != hubid.ChannelCLI {
			t.Errorf("%s: channel = %q", path, got.actor.Channel)
		}
		var res struct{ Kind string }
		if json.Unmarshal([]byte(out), &res) != nil || res.Kind == "" {
			t.Errorf("%s: result %s", path, out)
		}
	}
	code, out := request(t, sock, "GET", PathStatus, "", "")
	if code != 200 || !strings.Contains(out, `"state":"B3"`) {
		t.Errorf("status = %d %s", code, out)
	}
}

func TestOnlyTheHubsUserAndRootAreServed(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root is always served")
	}
	svc := &fakeSvc{}
	sock := serve(t, svc, func(s *Server) { s.allowed = uint32(os.Getuid()) + 1 }) //nolint:gosec // test uid
	for _, c := range []struct{ method, path, body string }{
		{"GET", PathStatus, ""}, {"POST", PathReissue, `{"passphrase":"aaaaaaaaaaaa"}`},
	} {
		code, out := request(t, sock, c.method, c.path, "application/json", c.body)
		if code != 403 || !strings.Contains(out, `"forbidden"`) {
			t.Errorf("%s %s by another user = %d %s", c.method, c.path, code, out)
		}
	}
	if svc.count() != 0 {
		t.Error("the service was called for a refused peer")
	}
}

func TestMalformedRequestsAreRefusedBeforeTheService(t *testing.T) {
	svc := &fakeSvc{}
	sock := serve(t, svc, nil)
	for name, c := range map[string]struct{ ct, body string }{
		"form content type": {"application/x-www-form-urlencoded", `passphrase=aaaaaaaaaaaa`},
		"no content type":   {"", `{"passphrase":"aaaaaaaaaaaa"}`},
		"trailing data":     {"application/json", `{"passphrase":"aaaaaaaaaaaa"}{"x":1}`},
		"not json":          {"application/json", `passphrase`},
		"empty passphrase":  {"application/json", `{"passphrase":""}`},
		"wrong type":        {"application/json", `{"passphrase":12}`},
		"too long":          {"application/json", `{"passphrase":"` + strings.Repeat("a", 20000) + `"}`},
	} {
		code, out := request(t, sock, "POST", PathReissue, c.ct, c.body)
		if code/100 != 4 {
			t.Errorf("%s = %d %s", name, code, out)
		}
	}
	if code, _ := request(t, sock, "GET", PathReissue, "", ""); code != 405 {
		t.Errorf("GET on a write = %d", code)
	}
	if code, _ := request(t, sock, "POST", "/v1/identity/nope", "application/json", "{}"); code != 404 {
		t.Errorf("unknown path = %d", code)
	}
	if svc.count() != 0 {
		t.Errorf("the service was called %d times for malformed requests", svc.count())
	}
}

func TestServiceErrorsAreMappedToTheEnvelope(t *testing.T) {
	cases := []struct {
		err  error
		code int
		want string
	}{
		{&hubid.Error{Code: hubid.CodePassphraseInvalid, Msg: "internal detail"}, 403, "passphrase_invalid"},
		{&hubid.Error{Code: hubid.CodeRateLimited, RetryAt: time.Now().Add(time.Hour)}, 429, "rate_limited"},
		{&hubid.Error{Code: hubid.CodeConflict, Msg: "a sealed root exists; use re-issue certificate"}, 409, "conflict"},
		{errors.New("secret detail: /var/lib/hub/root.key.age"), 500, "internal"},
	}
	for _, c := range cases {
		svc := &fakeSvc{err: c.err}
		sock := serve(t, svc, nil)
		code, out := request(t, sock, "POST", PathReissue, "application/json", `{"passphrase":"aaaaaaaaaaaa"}`)
		if code != c.code || !strings.Contains(out, `"`+c.want+`"`) || strings.Contains(out, "internal detail") || strings.Contains(out, "/var/lib") {
			t.Errorf("%v = %d %s", c.err, code, out)
		}
	}
}

func TestTheSocketIsOwnerOnlyAndStaleSocketsAreReplaced(t *testing.T) {
	path := shortSock(t)
	ln, err := Listen(path)
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Lstat(path)
	if err != nil || fi.Mode().Perm() != 0o600 || fi.Mode()&os.ModeSocket == 0 {
		t.Fatalf("socket = %v, %v", fi, err)
	}
	// A live socket is never replaced.
	if _, err := Listen(path); !errors.Is(err, ErrInUse) {
		t.Errorf("second listener: %v", err)
	}
	// A stale one (nobody answers) is replaced. Keep the file: closing a Go
	// unix listener removes it, so unlink-on-close is turned off first.
	ul := ln.(*net.UnixListener)
	ul.SetUnlinkOnClose(false)
	_ = ln.Close()
	if _, err := os.Lstat(path); err != nil {
		t.Fatal("the socket file is gone")
	}
	ln2, err := Listen(path)
	if err != nil {
		t.Fatalf("replace a stale socket: %v", err)
	}
	_ = ln2.Close()
	// A path that is not a socket is never touched.
	plain := filepath.Join(filepath.Dir(path), "other")
	if err := os.WriteFile(plain, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Listen(plain); err == nil {
		t.Error("a regular file was replaced by a socket")
	}
	if b, _ := os.ReadFile(plain); !bytes.Equal(b, []byte("x")) {
		t.Error("the regular file changed")
	}
	// A path that does not fit a Unix socket is reported, and skippable.
	long := "/tmp/" + strings.Repeat("d", 120) + "/admin.sock"
	if _, err := Listen(long); !Skippable(err) {
		t.Errorf("long path: %v", err)
	}
}
