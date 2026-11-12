package workers

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/audit"
	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/ratelimit"
	"github.com/patrick-1984/ai-minion/internal/transport"
)

// testHub is an in-process Hub: a real store, the workers Service and an
// httptest TLS server with its self-signed certificate (for a host name that
// is not the one the Worker dials; the Worker dial ignores certificates).
type testHub struct {
	t     *testing.T
	st    *store.Store
	svc   *Service
	srv   *httptest.Server
	audit *audit.Log
	logs  *syncBuf
}

func newTestHub(t *testing.T, mutate func(*ServiceOptions)) *testHub {
	t.Helper()
	dir := t.TempDir()
	st := openStore(t, dir)
	logs := &syncBuf{}
	h := &testHub{t: t, st: st, audit: audit.New(st), logs: logs}
	opts := ServiceOptions{DataDir: dir, Store: st, Auditor: h.audit, Logger: textLogger(logs)}
	if mutate != nil {
		mutate(&opts)
	}
	svc, err := NewService(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	h.svc = svc
	mux := http.NewServeMux()
	mux.Handle("GET "+transport.ConnectPath, svc.Handler())
	h.srv = httptest.NewTLSServer(mux)
	t.Cleanup(func() {
		svc.Close()
		h.srv.Close()
	})
	return h
}

// enrol inserts a Worker row for key, as enrollment (Story 4.5) will.
func (h *testHub) enrol(key *identity.Key, owner, name string) store.WorkerRow {
	h.t.Helper()
	w := store.WorkerRow{
		ID: store.NewID(), Pub: key.Public(), Name: name, OwnerID: owner,
		Kind: store.WorkerKindPrivate, CreatedAt: time.Now().UnixMilli(),
	}
	if err := h.st.InsertWorker(context.Background(), nil, w); err != nil {
		h.t.Fatal(err)
	}
	return w
}

func (h *testHub) pin() identity.Pin { return identity.Pin{Pub: h.svc.Identity().Public()} }

func newKey(t *testing.T) *identity.Key {
	t.Helper()
	k, err := identity.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func (h *testHub) workerConfig(key *identity.Key, ops ...string) transport.WorkerConfig {
	return transport.WorkerConfig{Key: key, Name: "test-worker", Ops: ops, Mode: transport.ModeConnect, Pin: h.pin(), Timeout: 10 * time.Second}
}

// dial runs the Worker side up to and including the AD-6 handshake.
func (h *testHub) dial(cfg transport.WorkerConfig) (*transport.WorkerResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := transport.Dial(ctx, h.srv.URL)
	if err != nil {
		return nil, err
	}
	return transport.WorkerHandshake(conn, cfg)
}

func readControl(t *testing.T, res *transport.WorkerResult) map[string]json.RawMessage {
	t.Helper()
	_ = res.Channel.SetReadDeadline(time.Now().Add(10 * time.Second))
	body, err := proto.ReadFrame(res.Channel, proto.StreamFrameLimit)
	if err != nil {
		t.Fatalf("reading the control frame: %v", err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func frameType(t *testing.T, m map[string]json.RawMessage) string {
	t.Helper()
	var s string
	if err := json.Unmarshal(m["type"], &s); err != nil {
		t.Fatal(err)
	}
	return s
}

func decodeWelcome(t *testing.T, res *transport.WorkerResult) Welcome {
	t.Helper()
	_ = res.Channel.SetReadDeadline(time.Now().Add(10 * time.Second))
	// The connection becomes a long-lived yamux session after welcome.
	defer func() { _ = res.Channel.SetReadDeadline(time.Time{}) }()
	body, err := proto.ReadFrame(res.Channel, proto.StreamFrameLimit)
	if err != nil {
		t.Fatalf("reading welcome: %v", err)
	}
	var w Welcome
	if err := json.Unmarshal(body, &w); err != nil || w.Type != proto.TypeWelcome {
		t.Fatalf("not a welcome: %s (%v)", body, err)
	}
	return w
}

// expectClosed checks that the Hub closes the connection after its answer: the
// read must end because the peer closed, not because our own deadline ran out.
// The WebSocket net.Conn reports an expired read deadline as a cancelled
// context, so the error class and the elapsed time are both checked.
func expectClosed(t *testing.T, res *transport.WorkerResult) {
	t.Helper()
	const limit = 10 * time.Second
	_ = res.Channel.SetReadDeadline(time.Now().Add(limit))
	start := time.Now()
	_, err := proto.ReadFrame(res.Channel, proto.StreamFrameLimit)
	if err == nil {
		t.Fatal("the Hub sent another frame instead of closing")
	}
	var ne net.Error
	timedOut := errors.Is(err, os.ErrDeadlineExceeded) || errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout()) ||
		time.Since(start) >= limit-time.Second
	if timedOut {
		t.Fatalf("the Hub did not close the connection (read ended by our own deadline after %s): %v", time.Since(start).Round(time.Millisecond), err)
	}
	_ = res.Channel.Close()
}

func TestKnownWorkerGetsWelcomeAndGoesOnline(t *testing.T) {
	h := newTestHub(t, nil)
	key := newKey(t)
	w := h.enrol(key, "owner-1", "pi")

	res, err := h.dial(h.workerConfig(key, "send_text", "sessions_list"))
	if err != nil {
		t.Fatal(err)
	}
	welcome := decodeWelcome(t, res)
	if welcome.WorkerID != w.ID || welcome.Name != "pi" || welcome.Owner != "owner-1" || welcome.Kind != "private" || welcome.Policy.PreviewOnly {
		t.Errorf("welcome = %+v", welcome)
	}
	want := proto.SupportedRange(proto.Current)
	if welcome.Min != want.Min || welcome.Max != want.Max {
		t.Errorf("welcome range = %s..%s, want the Hub's supported range %s..%s", welcome.Min, welcome.Max, want.Min, want.Max)
	}
	if res.PinChanged {
		t.Error("a connect with an up-to-date pin changed the pin")
	}

	sess, err := transport.NewSession(res.Channel, transport.RoleWorker, transport.SessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	eventually(t, "the Worker to be online", func() bool { return h.svc.Registry().IsOnline(w.ID) })
	on, _ := h.svc.Registry().Get(w.ID)
	if on.Version != proto.Current || on.OwnerID != "owner-1" || on.Name != "pi" || !on.Announces("send_text") || !on.Announces("sessions_list") || on.Announces("answer") {
		t.Errorf("online = %+v", on)
	}

	// OpenOp opens a real op stream that the Worker accepts.
	st, err := h.svc.Registry().OpenOp(context.Background(), w.ID, "send_text")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Reset()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	got, err := sess.Accept(ctx)
	if err != nil || got.Kind() != transport.KindOp {
		t.Fatalf("Worker accept: kind=%v err=%v", got, err)
	}
	if err := st.WriteFrame(map[string]string{"type": proto.TypeOpRequest}); err != nil {
		t.Fatal(err)
	}
	body, err := got.ReadFrame()
	if err != nil || !bytes.Contains(body, []byte(proto.TypeOpRequest)) {
		t.Fatalf("frame = %s err=%v", body, err)
	}
	if _, err := h.svc.Registry().OpenOp(context.Background(), w.ID, "answer"); proto.CodeOf(err) != proto.CodeUnsupportedOp {
		t.Errorf("an op the Worker did not announce: %v", err)
	}

	// Ending the connection takes the Worker offline.
	_ = sess.Close()
	eventually(t, "the Worker to go offline", func() bool { return !h.svc.Registry().IsOnline(w.ID) })
	if _, err := h.svc.Registry().OpenOp(context.Background(), w.ID, "send_text"); proto.CodeOf(err) != proto.CodeWorkerOffline {
		t.Errorf("offline OpenOp: %v", err)
	}
	if !strings.Contains(h.logs.String(), "worker online") || strings.Contains(h.logs.String(), "worker_pub") {
		t.Errorf("logs:\n%s", h.logs.String())
	}
}

func TestUnknownKeyIsAnsweredNotEnrolledAndClosed(t *testing.T) {
	h := newTestHub(t, nil)
	stranger := newKey(t)
	res, err := h.dial(h.workerConfig(stranger, "send_text"))
	if err != nil {
		t.Fatal(err)
	}
	m := readControl(t, res)
	if got := frameType(t, m); got != proto.TypeNotEnrolled {
		t.Fatalf("frame type = %q, want not_enrolled", got)
	}
	expectClosed(t, res)
	if h.svc.Registry().Len() != 0 {
		t.Error("an unknown Worker went online")
	}
	// Non-destructive: the Hub stored nothing about the stranger.
	if _, err := h.st.WorkerByPub(context.Background(), stranger.Public()); !errors.Is(err, store.ErrWorkerNotFound) {
		t.Errorf("the Hub kept a record of an unknown key: %v", err)
	}
	if _, err := h.st.TombstoneByPub(context.Background(), stranger.Public()); !errors.Is(err, store.ErrWorkerNotFound) {
		t.Errorf("tombstone for an unknown key: %v", err)
	}
}

func TestTombstonedKeyGetsASignedRevoke(t *testing.T) {
	h := newTestHub(t, nil)
	key := newKey(t)
	w := h.enrol(key, "owner-1", "pi")
	if err := h.st.TombstoneWorker(context.Background(), w.ID, "owner-1", time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}

	res, err := h.dial(h.workerConfig(key, "send_text"))
	if err != nil {
		t.Fatal(err)
	}
	m := readControl(t, res)
	if got := frameType(t, m); got != proto.TypeRevoke {
		t.Fatalf("frame type = %q, want revoke", got)
	}
	var f RevokeFrame
	raw, _ := json.Marshal(m)
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	// The signature verifies against the Hub key, purpose revoke, over the
	// transcript hash of this very handshake.
	if err := VerifyRevoke(res.HubPub, res.TranscriptHash[:], f); err != nil {
		t.Fatalf("revoke signature: %v", err)
	}
	if err := identity.Verify(res.HubPub, identity.PurposeRevoke, res.TranscriptHash[:], f.Sig); err != nil {
		t.Fatalf("revoke signature with the purpose spelled out: %v", err)
	}
	// ... and not over another transcript, for another purpose, or by another key.
	other := bytes.Clone(res.TranscriptHash[:])
	other[0] ^= 0xff
	if VerifyRevoke(res.HubPub, other, f) == nil {
		t.Error("the revoke verifies over a different transcript (replay)")
	}
	if identity.Verify(res.HubPub, identity.PurposeHandshakeHub, res.TranscriptHash[:], f.Sig) == nil {
		t.Error("the revoke signature verifies for another purpose")
	}
	if VerifyRevoke(ed25519.PublicKey(key.Public()), res.TranscriptHash[:], f) == nil {
		t.Error("the revoke verifies under another key")
	}
	expectClosed(t, res)
	if h.svc.Registry().Len() != 0 {
		t.Error("a removed Worker went online")
	}

	// The delivered revoke is audited, without content.
	page, err := h.audit.List(context.Background(), audit.Filter{Action: "worker.connect"})
	if err != nil || len(page.Events) != 1 {
		t.Fatalf("audit events = %+v err=%v", page, err)
	}
	if e := page.Events[0]; e.Actor != "worker:"+w.ID || e.Target != w.ID || e.Outcome != "revoked" {
		t.Errorf("audit event = %+v", e)
	}
}

func TestMajorVersionMismatchIsProtoIncompatible(t *testing.T) {
	h := newTestHub(t, nil)
	key := newKey(t)
	w := h.enrol(key, "owner-1", "pi")
	cfg := h.workerConfig(key, "send_text")
	cfg.Protocols = proto.Range{Min: proto.Version{Major: 3}, Max: proto.Version{Major: 3, Minor: 3}}
	_, err := h.dial(cfg)
	if proto.CodeOf(err) != proto.CodeProtoIncompatible {
		t.Fatalf("err = %v, want proto_incompatible", err)
	}
	if h.svc.Registry().Len() != 0 {
		t.Error("an incompatible Worker went online")
	}
	// No state change: the enrollment is untouched and the Worker can connect
	// again once it speaks the right version.
	if got, err := h.st.WorkerByPub(context.Background(), key.Public()); err != nil || got.ID != w.ID {
		t.Fatalf("worker row = %+v err=%v", got, err)
	}
	res, err := h.dial(h.workerConfig(key, "send_text"))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Channel.Close()
	if decodeWelcome(t, res).WorkerID != w.ID {
		t.Error("retry did not get its welcome")
	}
}

func TestVersionNegotiationAcceptsCurrentAndPreviousMinor(t *testing.T) {
	hubVersion := proto.Version{Major: 1, Minor: 2}
	h := newTestHub(t, func(o *ServiceOptions) { o.Current = hubVersion })
	cases := []struct {
		name    string
		worker  proto.Range
		want    proto.Version // zero: proto_incompatible
		wantErr bool
	}{
		{"current minor", proto.Range{Min: hubVersion, Max: hubVersion}, hubVersion, false},
		{"previous minor", proto.Range{Min: proto.Version{Major: 1, Minor: 1}, Max: proto.Version{Major: 1, Minor: 1}}, proto.Version{Major: 1, Minor: 1}, false},
		{"wide range picks the newest", proto.Range{Min: proto.Version{Major: 1}, Max: proto.Version{Major: 1, Minor: 9}}, hubVersion, false},
		{"two minors back", proto.Range{Min: proto.Version{Major: 1}, Max: proto.Version{Major: 1}}, proto.Version{}, true},
		{"newer than the Hub", proto.Range{Min: proto.Version{Major: 1, Minor: 3}, Max: proto.Version{Major: 1, Minor: 3}}, proto.Version{}, true},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			key := newKey(t)
			w := h.enrol(key, "owner-1", "w"+string(rune('a'+i)))
			cfg := h.workerConfig(key, "send_text")
			cfg.Protocols = c.worker
			res, err := h.dial(cfg)
			if c.wantErr {
				if proto.CodeOf(err) != proto.CodeProtoIncompatible {
					t.Fatalf("err = %v, want proto_incompatible", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			welcome := decodeWelcome(t, res)
			if welcome.Min != (proto.Version{Major: 1, Minor: 1}) || welcome.Max != hubVersion {
				t.Errorf("welcome range = %s..%s, want 1.1..1.2", welcome.Min, welcome.Max)
			}
			sess, err := transport.NewSession(res.Channel, transport.RoleWorker, transport.SessionOptions{})
			if err != nil {
				t.Fatal(err)
			}
			defer sess.Close()
			eventually(t, "online", func() bool { return h.svc.Registry().IsOnline(w.ID) })
			if on, _ := h.svc.Registry().Get(w.ID); on.Version != c.want {
				t.Errorf("negotiated version = %s, want %s", on.Version, c.want)
			}
		})
	}
}

// recordingLimiter counts calls and denies after a number of them.
type recordingLimiter struct {
	mu    sync.Mutex
	calls []netip.Addr
	allow int
	wait  time.Duration
	pol   []ratelimit.Policy
}

func (l *recordingLimiter) Allow(p ratelimit.Policy, ip netip.Addr, _ string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls = append(l.calls, ip)
	l.pol = append(l.pol, p)
	if len(l.calls) > l.allow {
		return false, l.wait
	}
	return true, 0
}

func TestConnectFloodIsLimitedBeforeTheHandshake(t *testing.T) {
	lim := &recordingLimiter{allow: 2, wait: 90 * time.Second}
	h := newTestHub(t, func(o *ServiceOptions) {
		o.Limiter = lim
		o.ClientIP = func(*http.Request) netip.Addr { return netip.MustParseAddr("203.0.113.9") }
	})
	key := newKey(t)
	h.enrol(key, "owner-1", "pi")

	for i := 0; i < 2; i++ {
		res, err := h.dial(h.workerConfig(key, "send_text"))
		if err != nil {
			t.Fatalf("attempt %d: %v", i, err)
		}
		_ = res.Channel.Close()
	}
	// The third attempt is refused with HTTP 429 before any WebSocket upgrade,
	// so no handshake, key agreement or database read happens for it.
	client := h.srv.Client()
	req, _ := http.NewRequest(http.MethodGet, h.srv.URL+transport.ConnectPath, nil)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Sec-WebSocket-Version", "13")
	req.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	req.Header.Set("Sec-WebSocket-Protocol", transport.Subprotocol)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429; body %s", resp.StatusCode, body)
	}
	if resp.Header.Get("Retry-After") != "90" {
		t.Errorf("Retry-After = %q", resp.Header.Get("Retry-After"))
	}
	var env struct {
		Error struct{ Code string } `json:"error"`
	}
	if err := json.Unmarshal(body, &env); err != nil || env.Error.Code != "rate_limited" {
		t.Errorf("body = %s (%v)", body, err)
	}
	if _, err := h.dial(h.workerConfig(key, "send_text")); err == nil {
		t.Error("a Worker dial got through a limited IP")
	}
	lim.mu.Lock()
	defer lim.mu.Unlock()
	for i := range lim.calls {
		if lim.calls[i].String() != "203.0.113.9" || lim.pol[i] != ratelimit.PolicyConnect {
			t.Errorf("limiter call %d = %v %v", i, lim.calls[i], lim.pol[i])
		}
	}
}

func TestRealLimiterAppliesTheConnectPolicyPerIP(t *testing.T) {
	limiter, err := ratelimit.New(ratelimit.Config{Policies: map[ratelimit.Policy]ratelimit.Spec{
		ratelimit.PolicyConnect: {IP: ratelimit.Bucket{Burst: 3, Interval: time.Hour}, Target: ratelimit.Bucket{Burst: 3, Interval: time.Hour}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	h := newTestHub(t, func(o *ServiceOptions) { o.Limiter = limiter })
	var ok, refused int
	for i := 0; i < 6; i++ {
		res, err := h.dial(h.workerConfig(newKey(t), "send_text"))
		if err != nil {
			refused++
			continue
		}
		ok++
		_ = res.Channel.Close()
	}
	if ok != 3 || refused != 3 {
		t.Errorf("ok=%d refused=%d, want 3 and 3", ok, refused)
	}
}

func TestUnservedModesAreRefused(t *testing.T) {
	h := newTestHub(t, nil)
	for _, mode := range []transport.Mode{transport.ModeEnrollInvite, transport.ModePairCode} {
		t.Run(string(mode), func(t *testing.T) {
			cfg := transport.WorkerConfig{Key: newKey(t), Name: "w", Mode: mode, Timeout: 10 * time.Second}
			switch mode {
			case transport.ModeEnrollInvite:
				cfg.HubFingerprint = h.svc.Identity().Fingerprint()
			case transport.ModePairCode:
				cfg.PairingCode = identity.NewPairingCode()
			}
			_, err := h.dial(cfg)
			if proto.CodeOf(err) != proto.CodeInvalidArgument {
				t.Fatalf("err = %v, want invalid_argument", err)
			}
		})
	}
}

// echoMode is a ModeHandler that reads one control frame and admits the
// Worker, as Story 4.5 will for an invitation.
type echoMode struct {
	row  store.WorkerRow
	seen chan json.RawMessage
}

func (e *echoMode) Handle(ctx context.Context, c *Conn) error {
	body, err := c.ReadFrame()
	if err != nil {
		return err
	}
	e.seen <- body
	return c.Welcome(ctx, e.row)
}

func TestModeHandlerPlugsIntoTheEndpoint(t *testing.T) {
	h := newTestHub(t, nil)
	key := newKey(t)
	row := h.enrol(key, "owner-1", "pi")
	mode := &echoMode{row: row, seen: make(chan json.RawMessage, 1)}
	if err := h.svc.Handler().Handle(transport.ModeEnrollInvite, mode); err != nil {
		t.Fatal(err)
	}
	if err := h.svc.Handler().Handle(transport.ModeEnrollInvite, mode); err == nil {
		t.Error("a second handler for the same mode was accepted")
	}
	if err := h.svc.Handler().Handle(transport.ModeConnect, mode); err == nil {
		t.Error("the connect mode was replaced")
	}

	cfg := transport.WorkerConfig{Key: key, Name: "pi", Mode: transport.ModeEnrollInvite, HubFingerprint: h.svc.Identity().Fingerprint(),
		Ops: []string{"send_text"}, Timeout: 10 * time.Second}
	res, err := h.dial(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := proto.WriteFrame(res.Channel, proto.StreamFrameLimit, map[string]string{"type": proto.TypeInviteSecret}); err != nil {
		t.Fatal(err)
	}
	if welcome := decodeWelcome(t, res); welcome.WorkerID != row.ID {
		t.Errorf("welcome = %+v", welcome)
	}
	select {
	case body := <-mode.seen:
		if !bytes.Contains(body, []byte(proto.TypeInviteSecret)) {
			t.Errorf("handler saw %s", body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the mode handler did not run")
	}
	sess, err := transport.NewSession(res.Channel, transport.RoleWorker, transport.SessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	eventually(t, "online", func() bool { return h.svc.Registry().IsOnline(row.ID) })
}

func TestReconnectReplacesTheStaleConnection(t *testing.T) {
	h := newTestHub(t, nil)
	key := newKey(t)
	w := h.enrol(key, "owner-1", "pi")

	connect := func() *transport.Session {
		res, err := h.dial(h.workerConfig(key, "send_text"))
		if err != nil {
			t.Fatal(err)
		}
		decodeWelcome(t, res)
		s, err := transport.NewSession(res.Channel, transport.RoleWorker, transport.SessionOptions{})
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	first := connect()
	eventually(t, "first online", func() bool { return h.svc.Registry().IsOnline(w.ID) })
	second := connect()
	defer second.Close()
	// The Hub closed the first connection when the Worker came back.
	select {
	case <-first.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("the first connection was not closed")
	}
	time.Sleep(50 * time.Millisecond)
	if !h.svc.Registry().IsOnline(w.ID) || h.svc.Registry().Len() != 1 {
		t.Fatal("the reconnected Worker is not online")
	}
}

func TestWelcomeCarriesPolicyOwnerNameAndStableOwnerID(t *testing.T) {
	h := newTestHub(t, func(o *ServiceOptions) {
		o.Policy = func(_ context.Context, w store.WorkerRow) (Policy, error) {
			return Policy{PreviewOnly: w.Name == "restricted"}, nil
		}
	})
	h.svc.SetOwnerNames(func(_ context.Context, id string) string { return "user-" + id })
	for name, want := range map[string]bool{"restricted": true, "free": false} {
		key := newKey(t)
		h.enrol(key, "42", name)
		res, err := h.dial(h.workerConfig(key))
		if err != nil {
			t.Fatal(err)
		}
		welcome := decodeWelcome(t, res)
		// owner_id is the stable id (what pipe_accept_from=owner-workers
		// compares); owner stays the display name.
		if welcome.Policy.PreviewOnly != want || welcome.Owner != "user-42" || welcome.OwnerID != "42" {
			t.Errorf("%s: welcome = %+v", name, welcome)
		}
		_ = res.Channel.Close()
	}
}

func TestPolicyFailureDoesNotAdmitTheWorker(t *testing.T) {
	h := newTestHub(t, func(o *ServiceOptions) {
		o.Policy = func(context.Context, store.WorkerRow) (Policy, error) {
			return Policy{}, errors.New("db down: secret detail")
		}
	})
	key := newKey(t)
	w := h.enrol(key, "owner-1", "pi")
	res, err := h.dial(h.workerConfig(key))
	if err != nil {
		t.Fatal(err)
	}
	m := readControl(t, res)
	if frameType(t, m) != proto.TypeError || !bytes.Contains(m["code"], []byte("internal")) {
		t.Fatalf("frame = %v", m)
	}
	if bytes.Contains(m["message"], []byte("secret")) {
		t.Error("an internal error detail reached the Worker")
	}
	if h.svc.Registry().IsOnline(w.ID) {
		t.Error("the Worker went online despite the failure")
	}
}

func TestServiceCloseDisconnectsWorkersAndRefusesNewOnes(t *testing.T) {
	h := newTestHub(t, nil)
	key := newKey(t)
	w := h.enrol(key, "owner-1", "pi")
	res, err := h.dial(h.workerConfig(key, "send_text"))
	if err != nil {
		t.Fatal(err)
	}
	decodeWelcome(t, res)
	sess, err := transport.NewSession(res.Channel, transport.RoleWorker, transport.SessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "online", func() bool { return h.svc.Registry().IsOnline(w.ID) })

	h.svc.Close()
	select {
	case <-sess.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("Close left the Worker connected")
	}
	eventually(t, "offline", func() bool { return !h.svc.Registry().IsOnline(w.ID) })
	resp, err := h.srv.Client().Get(h.srv.URL + transport.ConnectPath)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status after Close = %d, want 503", resp.StatusCode)
	}
}

func TestWorkerDialWithoutUpgradeIsAnswered(t *testing.T) {
	h := newTestHub(t, nil)
	resp, err := h.srv.Client().Get(h.srv.URL + transport.ConnectPath)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 400 || resp.StatusCode >= 500 {
		t.Errorf("a plain GET got status %d, want a 4xx", resp.StatusCode)
	}
}

func TestOwnerLookupReadsTheDatabaseEveryTime(t *testing.T) {
	h := newTestHub(t, nil)
	w := h.enrol(newKey(t), "owner-1", "pi")
	l := h.svc.OwnerLookup()
	owner, err := l.WorkerOwner(context.Background(), w.ID)
	if err != nil || owner != "owner-1" {
		t.Fatalf("owner = %q err=%v", owner, err)
	}
	if _, err := l.WorkerOwner(context.Background(), "unknown"); !errors.Is(err, authz.ErrNotFound) {
		t.Errorf("unknown Worker: %v", err)
	}
	// A removed Worker is not found any more: no stale positive answer.
	if err := h.st.TombstoneWorker(context.Background(), w.ID, "admin", time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if _, err := l.WorkerOwner(context.Background(), w.ID); !errors.Is(err, authz.ErrNotFound) {
		t.Errorf("removed Worker: %v", err)
	}
}

func TestChainIsSentInConnectMode(t *testing.T) {
	// The chain source (Story 4.9) is read per connection and handed to the
	// handshake; a Worker pinned to an older Hub key follows it.
	oldKey := newKey(t)
	var h *testHub
	var chain []identity.Transition
	h = newTestHub(t, func(o *ServiceOptions) {
		o.Chain = func(context.Context) ([]identity.Transition, error) { return chain, nil }
	})
	tr, err := identity.NewTransition(oldKey, h.svc.Identity().Public(), 1, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	chain = []identity.Transition{tr}
	key := newKey(t)
	h.enrol(key, "owner-1", "pi")
	cfg := h.workerConfig(key)
	cfg.Pin = identity.Pin{Pub: oldKey.Public()}
	res, err := h.dial(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Channel.Close()
	if !res.PinChanged || res.Pin.Seq != 1 || !bytes.Equal(res.Pin.Pub, h.svc.Identity().Public()) {
		t.Errorf("pin = %+v changed=%v", res.Pin, res.PinChanged)
	}
}

func TestConnectHandlerNeedsItsDependencies(t *testing.T) {
	if _, err := NewHandler(Config{}); err == nil {
		t.Error("an empty config was accepted")
	}
	if _, err := NewService(context.Background(), ServiceOptions{}); err == nil {
		t.Error("a service without a store was accepted")
	}
}

// TestCloseEndsAConnectionThatIsStillInTheHandshake: a Worker that upgraded
// but has not sent its hello is cut off by Service.Close, and Close returns
// only after its handler is gone (nothing touches the store afterwards).
func TestCloseEndsAConnectionThatIsStillInTheHandshake(t *testing.T) {
	h := newTestHub(t, func(o *ServiceOptions) { o.HandshakeTimeout = time.Minute })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := transport.Dial(ctx, h.srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	// The handler is registered once the upgrade has completed.
	eventually(t, "the handler to start", func() bool {
		h.svc.handler.connMu.Lock()
		defer h.svc.handler.connMu.Unlock()
		return len(h.svc.handler.live) == 1
	})

	began := time.Now()
	h.svc.Close()
	if d := time.Since(began); d >= shutdownWait {
		t.Fatalf("Service.Close ran into its wait limit (%s): the handshaking handler was not ended", d)
	}
	wctx, wcancel := context.WithTimeout(context.Background(), time.Second)
	defer wcancel()
	if err := h.svc.handler.Wait(wctx); err != nil {
		t.Fatalf("a handler is still running after Close: %v", err)
	}
	h.svc.handler.connMu.Lock()
	left := len(h.svc.handler.live)
	h.svc.handler.connMu.Unlock()
	if left != 0 {
		t.Errorf("%d connections still tracked after Close", left)
	}
	// The client side sees the connection end well before the minute-long
	// handshake timeout.
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	readStart := time.Now()
	if _, err := conn.Read(make([]byte, 1)); err == nil {
		t.Error("read succeeded on a connection the Hub closed")
	} else if time.Since(readStart) >= 4*time.Second {
		t.Errorf("the connection was not closed by the Hub (our deadline ended the read): %v", err)
	}
}

func TestAConnectAfterCloseIsRefused(t *testing.T) {
	h := newTestHub(t, nil)
	h.svc.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if conn, err := transport.Dial(ctx, h.srv.URL); err == nil {
		_ = conn.Close()
		t.Error("the Hub accepted a connection after Close")
	}
}
