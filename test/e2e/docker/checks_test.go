//go:build dockere2e

package dockere2e

import (
	"bytes"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/testutil/tmuxtest"
)

// Story 9.6: the checks of the container end-to-end job. They run against a
// Hub that docker compose started from the image ko built for this commit (or,
// for the local rehearsal of the same checks, against a Hub process; the
// checks cannot tell, they only use REST and the Worker), and drive the native
// minion binary as a foreground Worker on a private tmux socket.
//
// run.sh sets the environment (nothing here has a default for a path):
//
//	E2E_VARIANT                 "direct" or "proxy" (a label for messages)
//	E2E_HUB_URL                 https URL of the Hub (or of the proxy in front of it), host localhost
//	E2E_CA_FILE                 PEM certificate the Hub (or the proxy) presents: the only trust root of the REST client
//	E2E_MINION_BIN              the native minion binary
//	E2E_WORK                    a private (0700) directory for the Worker state, the log and the key file
//	E2E_ADMIN_USER              the seeded admin (default "admin")
//	E2E_ADMIN_PASSWORD_FILE     the file the Hub's admin secret came from
//	E2E_RESTART_ARGV            JSON array: the exact command that restarts the Hub
//	E2E_DEFAULT_TMUX_SOCKET     path of the default tmux socket of this user
//	E2E_DEFAULT_TMUX_STATE      "absent" or "present:<mtime>" as run.sh saw it before anything started

func env(t *testing.T, k string) string {
	t.Helper()
	v := os.Getenv(k)
	if v == "" {
		t.Fatalf("%s is not set; run test/e2e/docker/run.sh", k)
	}
	return v
}

// hubClient is a REST client of the Hub whose only trust root is the Hub's own
// certificate, with the host name check left on.
type hubClient struct {
	t    *testing.T
	base string
	hc   *http.Client
	csrf string
}

func newTLSClient(t *testing.T, caFile, serverName string) *http.Client {
	t.Helper()
	pemBytes, err := os.ReadFile(caFile)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pemBytes) {
		t.Fatalf("%s holds no certificate", caFile)
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Client{
		Timeout: 30 * time.Second,
		Jar:     jar,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{RootCAs: pool, ServerName: serverName, MinVersion: tls.VersionTLS12},
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

func (c *hubClient) do(method, path string, body any) (int, []byte, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.base+path, rd)
	if err != nil {
		return 0, nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.csrf != "" {
		req.Header.Set("X-Minion-CSRF", c.csrf)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	return resp.StatusCode, b, err
}

// call runs an op and decodes the result into out. The error never carries the
// response body (an invitation result holds a secret).
func (c *hubClient) call(op string, args, out any) error {
	status, body, err := c.do(http.MethodPost, ops.RESTPrefix+op, args)
	if err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	if status != http.StatusOK {
		var e struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
			Code string `json:"code"`
		}
		_ = json.Unmarshal(body, &e)
		code := e.Error.Code
		if code == "" {
			code = e.Code
		}
		return fmt.Errorf("%s: HTTP %d, code %q", op, status, code)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(body, out)
}

func (c *hubClient) login(user, password string) (mustChange bool) {
	c.t.Helper()
	c.csrf = ""
	status, body, err := c.do(http.MethodPost, "/auth/login", map[string]string{"username": user, "password": password})
	if err != nil || status != http.StatusOK {
		c.t.Fatalf("login as %s: HTTP %d, %v", user, status, err)
	}
	var resp struct {
		User struct {
			MustChangePassword bool `json:"must_change_password"`
		} `json:"user"`
		CSRFToken string `json:"csrf_token"`
	}
	if err := json.Unmarshal(body, &resp); err != nil || resp.CSRFToken == "" {
		c.t.Fatalf("login answer is not a session: %v", err)
	}
	c.csrf = resp.CSRFToken
	return resp.User.MustChangePassword
}

func readSecret(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimRight(string(b), "\r\n")
}

func waitFor(t *testing.T, what string, within time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for %s", within, what)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// worker is the native minion binary in foreground mode.
type worker struct {
	t    *testing.T
	cmd  *exec.Cmd
	log  string
	done chan error
	once sync.Once
}

func startWorker(t *testing.T, bin, hubURL, work, keyFile, tmuxSock string) *worker {
	t.Helper()
	// An own, short runtime directory: the Worker's control socket lives in it,
	// and two Workers (or this one and a developer's) must never share one.
	rt, err := os.MkdirTemp("", "e2erun")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(rt) })
	home := filepath.Join(work, "home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(work, "worker.log")
	lf, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lf.Close() })
	cmd := exec.Command(bin, "worker", "up", "--foreground",
		"--server", hubURL, "--invite-key", keyFile, "--share-existing", "all",
		"--state-dir", filepath.Join(work, "state"), "--tmux-socket", tmuxSock, "--name", "docker-e2e")
	// A clean environment: no TMUX (the Worker must not adopt a surrounding
	// tmux), no inherited MINION_*, no proxy variables, its own runtime dir.
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "XDG_RUNTIME_DIR=" + rt,
		"XDG_STATE_HOME=" + filepath.Join(home, "state"), "XDG_CONFIG_HOME=" + filepath.Join(home, "config")}
	cmd.Stdout, cmd.Stderr = lf, lf
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	w := &worker{t: t, cmd: cmd, log: logPath, done: make(chan error, 1)}
	go func() { w.done <- cmd.Wait() }()
	t.Cleanup(w.stop)
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("the last lines of the Worker log:\n%s", tail(w.logText()))
		}
	})
	return w
}

func (w *worker) stop() {
	w.once.Do(func() {
		select {
		case err := <-w.done:
			w.t.Logf("the Worker had already exited: %v", err)
			return
		default:
		}
		_ = w.cmd.Process.Signal(syscall.SIGINT)
		select {
		case err := <-w.done:
			w.t.Logf("the Worker stopped: %v", err)
		case <-time.After(15 * time.Second):
			_ = w.cmd.Process.Kill()
			err := <-w.done
			w.t.Logf("the Worker was killed: %v", err)
		}
	})
}

func (w *worker) logText() string {
	b, _ := os.ReadFile(w.log)
	return string(b)
}

// exited reports whether the process has ended (without consuming its status).
func (w *worker) exited() (error, bool) {
	select {
	case err := <-w.done:
		w.done <- err
		return err, true
	default:
		return nil, false
	}
}

func uuid7() string { return ops.NewUUIDv7() }

func TestHubImageAndNativeWorker(t *testing.T) {
	variant := env(t, "E2E_VARIANT")
	hubURL := strings.TrimRight(env(t, "E2E_HUB_URL"), "/")
	caFile := env(t, "E2E_CA_FILE")
	bin := env(t, "E2E_MINION_BIN")
	work := env(t, "E2E_WORK")
	adminUser := os.Getenv("E2E_ADMIN_USER")
	if adminUser == "" {
		adminUser = "admin"
	}
	adminPassword := readSecret(t, env(t, "E2E_ADMIN_PASSWORD_FILE"))
	var restartArgv []string
	if err := json.Unmarshal([]byte(env(t, "E2E_RESTART_ARGV")), &restartArgv); err != nil || len(restartArgv) == 0 {
		t.Fatalf("E2E_RESTART_ARGV must be a JSON array: %v", err)
	}
	defaultSock := env(t, "E2E_DEFAULT_TMUX_SOCKET")
	defaultState := env(t, "E2E_DEFAULT_TMUX_STATE")
	u, err := url.Parse(hubURL)
	if err != nil || u.Scheme != "https" || u.Hostname() != "localhost" {
		t.Fatalf("E2E_HUB_URL must be https://localhost:<port>, got %q", hubURL)
	}
	t.Logf("variant %s, Hub %s", variant, hubURL)

	c := &hubClient{t: t, base: hubURL, hc: newTLSClient(t, caFile, "localhost")}

	t.Run("TLS is verified, not skipped", func(t *testing.T) {
		// Trusting the test certificate and the name localhost works ...
		status, _, err := c.do(http.MethodGet, "/healthz", nil)
		if err != nil || status != http.StatusOK {
			t.Fatalf("GET /healthz with the pinned certificate: HTTP %d, %v", status, err)
		}
		// ... a client with the system roots only must refuse the same server ...
		sys := &http.Client{Timeout: 15 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12}}}
		if resp, err := sys.Get(hubURL + "/healthz"); err == nil {
			_ = resp.Body.Close()
			t.Fatal("a client without the test certificate accepted the Hub's certificate")
		} else if !strings.Contains(err.Error(), "certificate") && !strings.Contains(err.Error(), "x509") {
			t.Fatalf("the system-roots client failed for another reason: %v", err)
		}
		// ... and a client that has the certificate but asks for another name
		// must refuse it too, so the host name check is on.
		wrong := newTLSClient(t, caFile, "not-the-hub.invalid")
		if resp, err := wrong.Get(hubURL + "/healthz"); err == nil {
			_ = resp.Body.Close()
			t.Fatal("the certificate was accepted for a wrong host name")
		} else if !strings.Contains(err.Error(), "not-the-hub.invalid") && !strings.Contains(err.Error(), "x509") {
			t.Fatalf("the wrong-name client failed for another reason: %v", err)
		}
	})

	t.Run("seeded admin signs in and the first password change goes through the real auth flow", func(t *testing.T) {
		if c.login(adminUser, adminPassword) {
			next := randomHex(t, 16)
			if err := c.call("password_change", map[string]string{"old_password": adminPassword, "new_password": next}, nil); err != nil {
				t.Fatal(err)
			}
			adminPassword = next
			if c.login(adminUser, adminPassword) {
				t.Fatal("the account still must change its password")
			}
		}
	})

	var inviteFile string
	t.Run("an invitation is created over REST", func(t *testing.T) {
		var res ops.InvitationCreateResult
		if err := c.call(ops.OpInvitationCreate, ops.InvitationCreateArgs{Mode: ops.InvitationOneTime, TTL: "1h", Name: "docker-e2e"}, &res); err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(res.Key, "mnk_") {
			t.Fatal("the invitation result carries no key")
		}
		inviteFile = filepath.Join(work, "invite.key")
		if err := os.WriteFile(inviteFile, []byte(res.Key+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	})
	if inviteFile == "" {
		t.FailNow()
	}

	// The private tmux server comes from tmuxtest (AD-25): a socket below the
	// test's temporary directory, never the default one, killed by its own
	// socket at the end of the test. The Worker is told to use that socket.
	tmuxSock := tmuxtest.New(t).Socket()

	w := startWorker(t, bin, hubURL, work, inviteFile, tmuxSock)
	workerID := ""
	// listed returns the Worker docker-e2e as the Hub lists it, if it is there.
	listed := func() (ops.WorkerInfo, bool) {
		var list ops.WorkersListResult
		if err := c.call(ops.OpWorkersList, struct{}{}, &list); err != nil {
			return ops.WorkerInfo{}, false
		}
		for _, wi := range list.Workers {
			if wi.Name == "docker-e2e" {
				return wi, true
			}
		}
		return ops.WorkerInfo{}, false
	}
	// online: connected and approved, so that the Hub routes operations to it.
	online := func() bool {
		wi, ok := listed()
		if ok && wi.Online && !wi.Pending {
			workerID = wi.ID
			return true
		}
		return false
	}
	t.Run("the native Worker enrolls with the literal key and waits for approval", func(t *testing.T) {
		var pending ops.WorkerInfo
		waitFor(t, "the Worker to be connected and pending at the Hub", 90*time.Second, func() bool {
			if err, gone := w.exited(); gone {
				t.Fatalf("the Worker process ended (%v):\n%s", err, tail(w.logText()))
			}
			wi, ok := listed()
			pending = wi
			return ok && wi.Online
		})
		if !pending.Pending || len(pending.Fingerprint) != 52 || pending.EnrollAddr == "" {
			t.Fatalf("a newly enrolled Worker must be pending with its fingerprint and address: %+v", pending)
		}
		if !strings.Contains(w.logText(), "enrolled as docker-e2e") {
			t.Fatalf("the Worker did not report its enrollment:\n%s", tail(w.logText()))
		}
		// F-02: until it is approved the Hub routes nothing to it.
		err := c.call(ops.OpSessionCreate, createBody{
			SessionCreateArgs: ops.SessionCreateArgs{WriteArgs: ops.WriteArgs{RequestID: uuid7()}, Name: "refused", Argv: []string{"cat"}},
			WorkerID:          pending.ID,
		}, nil)
		if err == nil || !strings.Contains(err.Error(), "worker_pending") {
			t.Fatalf("session_create on a pending Worker = %v, want worker_pending", err)
		}
		// The admin approves it (the owner or an admin could; automation would use
		// an admin API key with `minion hub worker approve <fingerprint>`).
		var res ops.WriteResult
		if err := c.call(ops.OpWorkerApprove, ops.WorkerApproveArgs{
			WriteArgs: ops.WriteArgs{RequestID: uuid7()}, WorkerID: pending.ID, Fingerprint: pending.Fingerprint,
		}, &res); err != nil || res.Outcome != ops.OutcomeDelivered {
			t.Fatalf("worker_approve = %+v, %v", res, err)
		}
	})
	t.Run("the approved Worker reconnects and comes online", func(t *testing.T) {
		waitFor(t, "the approved Worker to be online at the Hub", 90*time.Second, func() bool {
			if err, gone := w.exited(); gone {
				t.Fatalf("the Worker process ended (%v):\n%s", err, tail(w.logText()))
			}
			return online()
		})
		if n := strings.Count(w.logText(), "enrolled as docker-e2e"); n != 1 {
			t.Fatalf("the Worker enrolled %d times; an approval must not enroll again", n)
		}
	})

	target := func(session string) ops.Target { return ops.Target{WorkerID: workerID, SessionName: session} }
	snapshot := func(session string) string {
		var res ops.SessionSnapshotResult
		if err := c.call(ops.OpSessionSnapshot, ops.SessionSnapshotArgs{Target: target(session)}, &res); err != nil {
			t.Logf("snapshot: %v", err)
			return ""
		}
		return strings.Join(res.Screen.Lines, "\n")
	}
	var session string
	t.Run("a session is created, text is sent and the screen is read over REST", func(t *testing.T) {
		var created ops.WriteResult
		err := c.call(ops.OpSessionCreate, createBody{
			SessionCreateArgs: ops.SessionCreateArgs{WriteArgs: ops.WriteArgs{RequestID: uuid7()}, Name: "e2e-cat", Argv: []string{"cat"}},
			WorkerID:          workerID,
		}, &created)
		if err != nil {
			t.Fatal(err)
		}
		if created.Outcome != ops.OutcomeDelivered || created.Pane == nil || created.Pane.Session == "" {
			t.Fatalf("session_create = %+v", created)
		}
		session = created.Pane.Session
		sendMarker(t, c, workerID, session, "docker-e2e-before-restart")
		waitFor(t, "the first marker on the screen", 30*time.Second, func() bool {
			return strings.Contains(snapshot(session), "docker-e2e-before-restart")
		})
	})
	if session == "" {
		t.FailNow()
	}

	t.Run("the Hub restarts and the same Worker reconnects, with no new enrollment", func(t *testing.T) {
		cmd := exec.Command(restartArgv[0], restartArgv[1:]...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("restart the Hub (%s): %v: %s", restartArgv[0], err, out)
		}
		waitFor(t, "the restarted Hub's /healthz", 90*time.Second, func() bool {
			status, _, err := c.do(http.MethodGet, "/healthz", nil)
			return err == nil && status == http.StatusOK
		})
		// The session lives in the database and survives; sign in again if it
		// does not.
		if status, _, _ := c.do(http.MethodPost, ops.RESTPrefix+ops.OpWorkersList, struct{}{}); status == http.StatusUnauthorized {
			c.login(adminUser, adminPassword)
		}
		waitFor(t, "the Worker to be online at the restarted Hub", 120*time.Second, func() bool {
			if err, gone := w.exited(); gone {
				t.Fatalf("the Worker process ended (%v):\n%s", err, tail(w.logText()))
			}
			return online()
		})
		sendMarker(t, c, workerID, session, "docker-e2e-after-restart")
		waitFor(t, "both markers on the same pane", 30*time.Second, func() bool {
			s := snapshot(session)
			return strings.Contains(s, "docker-e2e-before-restart") && strings.Contains(s, "docker-e2e-after-restart")
		})
		if n := strings.Count(w.logText(), "enrolled as docker-e2e"); n != 1 {
			t.Fatalf("the Worker enrolled %d times; a restart of the Hub must not enroll again", n)
		}
	})

	t.Run("the default tmux socket was never created or touched", func(t *testing.T) {
		fi, err := os.Lstat(defaultSock)
		switch {
		case defaultState == "absent" && err == nil:
			t.Fatalf("the default tmux socket %s exists after the run", defaultSock)
		case defaultState == "absent" && !errors.Is(err, os.ErrNotExist):
			t.Fatalf("lstat %s: %v", defaultSock, err)
		case strings.HasPrefix(defaultState, "present:"):
			// Not a clean runner (a developer's machine): the socket was there
			// before, so only "unchanged" can be asserted.
			if err != nil {
				t.Fatalf("the default tmux socket that existed before is gone: %v", err)
			}
			if got := fmt.Sprintf("present:%d", fi.ModTime().UnixNano()); got != defaultState {
				t.Fatalf("the default tmux socket changed: %s, was %s", got, defaultState)
			}
		}
	})
}

// createBody is the REST body of session_create: its arguments have no target,
// so the Worker is named by the worker_id member.
type createBody struct {
	ops.SessionCreateArgs
	WorkerID string `json:"worker_id"`
}

func sendMarker(t *testing.T, c *hubClient, workerID, session, text string) {
	t.Helper()
	var res ops.WriteResult
	err := c.call(ops.OpSendText, ops.SendTextArgs{
		WriteArgs: ops.WriteArgs{RequestID: uuid7()},
		Target:    ops.Target{WorkerID: workerID, SessionName: session}, Text: text,
	}, &res)
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != ops.OutcomeDelivered {
		t.Fatalf("send_text outcome = %s, reason %q", res.Outcome, res.Reason)
	}
}

func randomHex(t *testing.T, n int) string {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}

// tail returns the last lines of a log for a failure message.
func tail(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > 30 {
		lines = lines[len(lines)-30:]
	}
	return strings.Join(lines, "\n")
}
