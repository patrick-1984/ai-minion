package agent

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/transport"
	"github.com/patrick-1984/ai-minion/internal/worker/store"
)

// ------------------------------------------------------------ fakes

// stepLog records the order of the persistence and network steps.
type stepLog struct {
	mu    sync.Mutex
	steps []string
}

func (l *stepLog) add(s string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.steps = append(l.steps, s)
}

func (l *stepLog) get() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.steps...)
}

var errCrash = errors.New("injected crash")

// fakeState is an InviteState over memory that records its steps and can fail
// at a chosen one (crash injection).
type fakeState struct {
	log      *stepLog
	key      *identity.Key
	enr      *Enrollment
	failKey  bool
	failLoad bool
	failSave bool
}

func (s *fakeState) LoadOrCreateKey() (*identity.Key, error) {
	s.log.add("key")
	if s.failKey {
		return nil, errCrash
	}
	if s.key == nil {
		k, err := identity.GenerateKey()
		if err != nil {
			return nil, err
		}
		s.key = k
	}
	return s.key, nil
}

func (s *fakeState) Load(context.Context) (Enrollment, bool, error) {
	s.log.add("load")
	if s.failLoad {
		return Enrollment{}, false, errCrash
	}
	if s.enr == nil {
		return Enrollment{}, false, nil
	}
	return *s.enr, true, nil
}

func (s *fakeState) Commit(_ context.Context, e Enrollment) error {
	s.log.add("commit")
	if s.failSave {
		return errCrash
	}
	s.enr = &e
	return nil
}

// fakeDialer is an InviteDialer that records the call and answers from a script.
type fakeDialer struct {
	log     *stepLog
	results []error // one per call; nil means success; the last one repeats
	calls   int
	last    InviteParams
	pin     identity.Pin
}

func (d *fakeDialer) Enroll(_ context.Context, p InviteParams) (*InviteOutcome, error) {
	d.log.add("dial")
	d.calls++
	d.last = p
	var err error
	if len(d.results) > 0 {
		i := d.calls - 1
		if i >= len(d.results) {
			i = len(d.results) - 1
		}
		err = d.results[i]
	}
	if err != nil {
		return nil, err
	}
	pin := d.pin
	if pin.Pub == nil {
		pin = identity.Pin{Pub: make([]byte, 32)}
	}
	return &InviteOutcome{Pin: pin, Welcome: HubWelcome{
		Type: proto.TypeWelcome, WorkerID: "w-1", Name: "pi", Owner: "alice", Kind: "private",
	}}, nil
}

func newInviteKey(t *testing.T) (identity.InviteKey, *identity.Key) {
	t.Helper()
	hub, err := identity.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	k, err := identity.NewInviteKey(hub.Fingerprint())
	if err != nil {
		t.Fatal(err)
	}
	return k, hub
}

func noSleep(context.Context, time.Duration) error { return nil }

func newFakeEnroller(t *testing.T, st InviteState, d InviteDialer, logs *logBuf) *InviteEnroller {
	t.Helper()
	e, err := NewInviteEnroller(InviteEnrollerConfig{
		State: st, Dialer: d, HubURL: "https://hub.example:8443", Name: "pi", Sleep: noSleep,
		Logger: slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// ------------------------------------------------------------ persistence order

func TestEnrollPersistsTheKeyPairThenConnectsThenCommits(t *testing.T) {
	invite, _ := newInviteKey(t)
	log := &stepLog{}
	st := &fakeState{log: log}
	e := newFakeEnroller(t, st, &fakeDialer{log: log}, &logBuf{})

	res, err := e.Enroll(context.Background(), invite)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"load", "key", "dial", "commit"}; !reflect.DeepEqual(log.get(), want) {
		t.Fatalf("steps = %v, want %v", log.get(), want)
	}
	if res.AlreadyEnrolled || res.Enrollment.Name != "pi" || res.Enrollment.Owner != "alice" || res.Enrollment.WorkerID != "w-1" {
		t.Fatalf("result = %+v", res)
	}
	if st.enr == nil || st.enr.Pin.Seq != 0 || len(st.enr.Pin.Pub) != 32 {
		t.Fatalf("stored = %+v", st.enr)
	}
}

func TestEnrollCrashInjectionAbortsBetweenStepsWithoutLaterSteps(t *testing.T) {
	cases := []struct {
		name  string
		setup func(*fakeState, *fakeDialer)
		want  []string
		keep  bool // the key pair must exist afterwards
	}{
		{"before the key pair is saved", func(s *fakeState, _ *fakeDialer) { s.failKey = true }, []string{"load", "key"}, false},
		{"between key pair and connect", func(_ *fakeState, d *fakeDialer) { d.results = []error{errCrash} }, []string{"load", "key", "dial"}, true},
		{"between connect and commit", func(s *fakeState, _ *fakeDialer) { s.failSave = true }, []string{"load", "key", "dial", "commit"}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			invite, _ := newInviteKey(t)
			log := &stepLog{}
			st, d := &fakeState{log: log}, &fakeDialer{log: log}
			c.setup(st, d)
			logs := &logBuf{}
			e := newFakeEnroller(t, st, d, logs)
			e.cfg.Attempts = 1

			res, err := e.Enroll(context.Background(), invite)
			if !errors.Is(err, errCrash) {
				t.Fatalf("err = %v", err)
			}
			if !reflect.DeepEqual(log.get(), c.want) {
				t.Fatalf("steps = %v, want %v", log.get(), c.want)
			}
			if st.enr != nil || res.Enrollment.WorkerID != "" {
				t.Fatalf("success without a committed enrollment: %+v", st.enr)
			}
			if (st.key != nil) != c.keep {
				t.Fatalf("key pair saved = %v, want %v", st.key != nil, c.keep)
			}
		})
	}
}

func TestEnrollRetryAfterACrashKeepsTheSameKeyPair(t *testing.T) {
	invite, _ := newInviteKey(t)
	log := &stepLog{}
	st := &fakeState{log: log, failSave: true}
	d := &fakeDialer{log: log}
	e := newFakeEnroller(t, st, d, &logBuf{})
	e.cfg.Attempts = 1
	if _, err := e.Enroll(context.Background(), invite); !errors.Is(err, errCrash) {
		t.Fatal(err)
	}
	first := d.last.Key.Public()

	st.failSave = false
	if _, err := e.Enroll(context.Background(), invite); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(d.last.Key.Public(), first) {
		t.Fatal("the retry presented another key pair")
	}
}

func TestEnrollAlreadyEnrolledIgnoresTheKeyWithAnInfoLine(t *testing.T) {
	invite, _ := newInviteKey(t)
	log := &stepLog{}
	st := &fakeState{log: log, enr: &Enrollment{Name: "old", Owner: "bob", WorkerID: "w-0"}}
	d := &fakeDialer{log: log}
	logs := &logBuf{}
	e := newFakeEnroller(t, st, d, logs)

	res, err := e.Enroll(context.Background(), invite)
	if err != nil {
		t.Fatal(err)
	}
	if !res.AlreadyEnrolled || res.Enrollment.Name != "old" {
		t.Fatalf("result = %+v", res)
	}
	if want := []string{"load"}; !reflect.DeepEqual(log.get(), want) {
		t.Fatalf("an enrolled Worker touched the key or the Hub: %v", log.get())
	}
	out := logs.String()
	if !strings.Contains(out, "level=INFO") || !strings.Contains(out, "already enrolled") {
		t.Fatalf("no info line: %s", out)
	}
	if strings.Contains(out, invite.String()) {
		t.Fatal("the log carries the key")
	}
}

func TestEnrollNeedsAHubURL(t *testing.T) {
	invite, _ := newInviteKey(t)
	log := &stepLog{}
	e, err := NewInviteEnroller(InviteEnrollerConfig{State: &fakeState{log: log}, Dialer: &fakeDialer{log: log}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Enroll(context.Background(), invite); err == nil || !strings.Contains(err.Error(), "--server") {
		t.Fatalf("err = %v", err)
	}
	if want := []string{"load"}; !reflect.DeepEqual(log.get(), want) {
		t.Fatalf("steps = %v", log.get())
	}
}

func TestEnrollRetriesTransientFailuresButNeverARefusal(t *testing.T) {
	invite, _ := newInviteKey(t)
	t.Run("transient then success", func(t *testing.T) {
		log := &stepLog{}
		d := &fakeDialer{log: log, results: []error{errors.New("connection refused"), errors.New("reset"), nil}}
		var waits []time.Duration
		e := newFakeEnroller(t, &fakeState{log: log}, d, &logBuf{})
		e.cfg.RetryWait = time.Second
		e.cfg.Sleep = func(_ context.Context, w time.Duration) error { waits = append(waits, w); return nil }
		if _, err := e.Enroll(context.Background(), invite); err != nil {
			t.Fatal(err)
		}
		if d.calls != 3 || !reflect.DeepEqual(waits, []time.Duration{time.Second, 2 * time.Second}) {
			t.Fatalf("calls %d waits %v", d.calls, waits)
		}
	})
	t.Run("gives up after the attempts", func(t *testing.T) {
		log := &stepLog{}
		d := &fakeDialer{log: log, results: []error{errors.New("down")}}
		e := newFakeEnroller(t, &fakeState{log: log}, d, &logBuf{})
		if _, err := e.Enroll(context.Background(), invite); err == nil || d.calls != DefaultInviteAttempts {
			t.Fatalf("calls %d err %v", d.calls, err)
		}
	})
	for name, perm := range map[string]error{
		"hub refusal":      proto.NewError(proto.CodeInviteExpired, "the invitation key has expired"),
		"fingerprint":      &FingerprintMismatchError{Want: "a", Got: "b"},
		"signature":        fmt.Errorf("handshake: %w", transport.ErrBadSignature),
		"protocol":         fmt.Errorf("handshake: %w", transport.ErrProtocol),
		"hub key rejected": transport.ErrPinMismatch,
	} {
		t.Run(name+" is not retried", func(t *testing.T) {
			log := &stepLog{}
			d := &fakeDialer{log: log, results: []error{perm}}
			e := newFakeEnroller(t, &fakeState{log: log}, d, &logBuf{})
			if _, err := e.Enroll(context.Background(), invite); err == nil || d.calls != 1 {
				t.Fatalf("calls %d err %v", d.calls, err)
			}
		})
	}
	t.Run("a canceled context stops the retry", func(t *testing.T) {
		log := &stepLog{}
		d := &fakeDialer{log: log, results: []error{errors.New("down")}}
		e := newFakeEnroller(t, &fakeState{log: log}, d, &logBuf{})
		ctx, cancel := context.WithCancel(context.Background())
		e.cfg.Sleep = func(ctx context.Context, _ time.Duration) error { cancel(); return ctx.Err() }
		if _, err := e.Enroll(ctx, invite); !errors.Is(err, context.Canceled) || d.calls != 1 {
			t.Fatalf("calls %d err %v", d.calls, err)
		}
	})
}

func TestEnrollRejectsAnIncompleteWelcome(t *testing.T) {
	invite, _ := newInviteKey(t)
	log := &stepLog{}
	d := incompleteDialer{}
	e := newFakeEnroller(t, &fakeState{log: log}, d, &logBuf{})
	if _, err := e.Enroll(context.Background(), invite); err == nil {
		t.Fatal("an incomplete welcome was accepted")
	}
	if got := log.get(); got[len(got)-1] == "commit" {
		t.Fatalf("committed an incomplete welcome: %v", got)
	}
}

type incompleteDialer struct{}

func (incompleteDialer) Enroll(context.Context, InviteParams) (*InviteOutcome, error) {
	return &InviteOutcome{Pin: identity.Pin{Pub: make([]byte, 32)}}, nil
}

// ------------------------------------------------------------ in-process Hub

// inviteHub is a Hub made of the shared transport package, as the Hub's
// enroll_invite handler uses it: WebSocket accept, AD-6 handshake, the
// invite.secret frame inside the channel, the answer, then the connection
// ends. It serves TLS with a self-signed certificate for another host name.
// The real Hub handler runs against the same Worker code in test/e2e.
type inviteHub struct {
	t    *testing.T
	key  *identity.Key
	srv  *httptest.Server
	cert tls.Certificate

	mu       sync.Mutex
	secret   string            // the expected secret (wire form)
	known    map[string]string // worker pub -> worker id
	received []string          // secrets that arrived inside the channel
	hellos   []transport.WorkerHello
	// loseAnswer commits the enrollment but closes the channel instead of
	// sending the welcome, once.
	loseAnswer bool
	// refuse answers with this error frame instead of enrolling.
	refuse *proto.Error
	// oneTime makes the invitation single-use: once a Worker key is enrolled,
	// every other key that presents the secret is refused.
	oneTime bool
}

func newInviteHub(t *testing.T) (*inviteHub, identity.InviteKey) {
	t.Helper()
	key, err := identity.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	invite, err := identity.NewInviteKey(key.Fingerprint())
	if err != nil {
		t.Fatal(err)
	}
	h := &inviteHub{t: t, key: key, cert: wrongNameCert(t), known: map[string]string{}, secret: inviteSecretPart(invite)}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+transport.ConnectPath, h.serve)
	h.srv = httptest.NewUnstartedServer(mux)
	_ = h.srv.Listener.Close()
	h.srv.Listener = l
	h.srv.TLS = &tls.Config{Certificates: []tls.Certificate{h.cert}, MinVersion: tls.VersionTLS13}
	h.srv.StartTLS()
	t.Cleanup(func() { h.srv.CloseClientConnections(); h.srv.Close() })
	return h, invite
}

func (h *inviteHub) url() string { return h.srv.URL }

func (h *inviteHub) secretsReceived() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.received...)
}

func (h *inviteHub) serve(w http.ResponseWriter, r *http.Request) {
	conn, err := transport.Accept(w, r)
	if err != nil {
		return
	}
	res, err := transport.HubHandshake(conn, transport.HubConfig{Key: h.key, Timeout: 10 * time.Second})
	if err != nil {
		return
	}
	defer func() { _ = res.Channel.Close() }()
	h.mu.Lock()
	h.hellos = append(h.hellos, res.Worker)
	h.mu.Unlock()
	body, err := proto.ReadFrame(res.Channel, proto.StreamFrameLimit)
	if err != nil {
		return
	}
	var f struct {
		Type   string `json:"type"`
		Secret string `json:"secret"`
	}
	if json.Unmarshal(body, &f) != nil || f.Type != proto.TypeInviteSecret {
		_ = proto.WriteFrame(res.Channel, proto.StreamFrameLimit, proto.NewError(proto.CodeInvalidArgument, "expected invite.secret").Frame())
		return
	}
	h.mu.Lock()
	h.received = append(h.received, f.Secret)
	refuse, lose := h.refuse, h.loseAnswer
	id, known := h.known[string(res.WorkerPub)]
	switch {
	case refuse != nil:
	case known:
	case h.oneTime && len(h.known) > 0:
		refuse = proto.NewError(proto.CodeInviteInvalid, "the invitation key was already used")
	case f.Secret != h.secret:
		refuse = proto.NewError(proto.CodeInviteInvalid, "the invitation key is not valid")
	default:
		id = fmt.Sprintf("w-%d", len(h.known)+1)
		h.known[string(res.WorkerPub)] = id
	}
	if lose && refuse == nil {
		h.loseAnswer = false
	} else {
		lose = false
	}
	h.mu.Unlock()
	switch {
	case refuse != nil:
		_ = proto.WriteFrame(res.Channel, proto.StreamFrameLimit, refuse.Frame())
	case lose:
		// Committed, but the Worker never hears of it.
	default:
		_ = proto.WriteFrame(res.Channel, proto.StreamFrameLimit, map[string]any{
			"type": proto.TypeWelcome, "worker_id": id, "name": res.Worker.Name, "owner": "alice", "kind": "private",
			"policy": map[string]bool{"preview_only": false}, "proto_min": "1.0", "proto_max": "1.0",
		})
		// Hold the connection until the Worker closes it.
		_, _ = res.Channel.Read(make([]byte, 1))
	}
}

func inviteSettings(t *testing.T, res *config.Resolver, out *bytes.Buffer, logs *logBuf) Settings {
	t.Helper()
	return Settings{
		WorkerCore: config.WorkerCore{StateDir: t.TempDir(), Name: "pi"},
		Resolver:   res,
		Logger:     slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Out:        out,
	}
}

func inviteResolver(flags config.MapSource, env map[string]string, file config.FileSource) *config.Resolver {
	return &config.Resolver{
		Role: config.ScopeWorker, Flags: flags, File: file,
		LookupEnv: func(k string) (string, bool) { v, ok := env[k]; return v, ok },
	}
}

// runHook runs the production hook with the production dialer against hub.
func runHook(t *testing.T, s Settings, stdin string, stderr *bytes.Buffer) error {
	t.Helper()
	d := defaultInviteHookDeps()
	d.stdin, d.stderr = strings.NewReader(stdin), stderr
	d.sleep, d.wait = noSleep, time.Millisecond
	return d.run(context.Background(), s)
}

func openWorkerState(t *testing.T, dir string) *Enrollments {
	t.Helper()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return NewEnrollments(st, dir)
}

// ------------------------------------------------------------ hook, real dialer

func TestHookEnrollsWithTheLiteralKeyAgainstASelfSignedWrongNameHubWithNoTLSFlags(t *testing.T) {
	hub, invite := newInviteHub(t)
	var out, stderr bytes.Buffer
	logs := &logBuf{}
	res := inviteResolver(config.MapSource{config.KeyServer: hub.url(), config.KeyInviteKey: invite.String()}, nil, nil)
	s := inviteSettings(t, res, &out, logs)

	if err := runHook(t, s, "", &stderr); err != nil {
		t.Fatal(err)
	}

	// Exactly one warning line on stderr, and the report only after success.
	if got := stderr.String(); got != config.InviteKeyArgvWarning+"\n" {
		t.Fatalf("stderr = %q", got)
	}
	if got := out.String(); got != "enrolled as pi, owner alice\n" {
		t.Fatalf("out = %q", got)
	}
	// The secret arrived inside the channel, and the pin and enrollment are in worker.db.
	if got := hub.secretsReceived(); len(got) != 1 || got[0] != hub.secret {
		t.Fatalf("hub received %v", got)
	}
	enr, ok, err := openWorkerState(t, s.StateDir).Load(context.Background())
	if err != nil || !ok {
		t.Fatalf("enrollment: %v, %v", ok, err)
	}
	if !bytes.Equal(enr.Pin.Pub, hub.key.Public()) || enr.Pin.Seq != 0 || enr.WorkerID != "w-1" ||
		enr.Name != "pi" || enr.Owner != "alice" || enr.Kind != "private" {
		t.Fatalf("stored enrollment = %+v", enr)
	}
	// The Worker key pair exists and is the one the Hub enrolled.
	k, err := openWorkerState(t, s.StateDir).LoadKey()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := hub.known[string(k.Public())]; !ok {
		t.Fatal("the Hub enrolled another key than the Worker's saved key pair")
	}
	assertNoKey(t, invite, logs.String(), out.String(), stderr.String())
}

func TestHookEnrollsWithAFileAndWithStdinWithoutAWarning(t *testing.T) {
	for _, form := range []string{"file", "stdin", "env"} {
		t.Run(form, func(t *testing.T) {
			hub, invite := newInviteHub(t)
			var out, stderr bytes.Buffer
			logs := &logBuf{}
			path := filepath.Join(t.TempDir(), "invite")
			if err := os.WriteFile(path, []byte(invite.String()+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			flags := config.MapSource{config.KeyServer: hub.url()}
			env := map[string]string{}
			stdin := ""
			switch form {
			case "file":
				flags[config.KeyInviteKey] = path
			case "stdin":
				flags[config.KeyInviteKey] = "-"
				stdin = invite.String() + "\n"
			case "env":
				env["MINION_INVITE_KEY_FILE"] = path
			}
			s := inviteSettings(t, inviteResolver(flags, env, nil), &out, logs)
			if err := runHook(t, s, stdin, &stderr); err != nil {
				t.Fatal(err)
			}
			if stderr.Len() != 0 {
				t.Fatalf("warning for the %s form: %q", form, stderr.String())
			}
			if out.String() != "enrolled as pi, owner alice\n" {
				t.Fatalf("out = %q", out.String())
			}
			assertNoKey(t, invite, logs.String(), out.String(), stderr.String())
		})
	}
}

func TestHookWithAWrongHubIdentityAbortsBeforeSendingTheSecretAndNamesBothFingerprints(t *testing.T) {
	hub, _ := newInviteHub(t)
	other, err := identity.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	wrong, err := identity.NewInviteKey(other.Fingerprint())
	if err != nil {
		t.Fatal(err)
	}
	var out, stderr bytes.Buffer
	logs := &logBuf{}
	res := inviteResolver(config.MapSource{config.KeyServer: hub.url(), config.KeyInviteKey: wrong.String()}, nil, nil)
	s := inviteSettings(t, res, &out, logs)

	err = runHook(t, s, "", &stderr)
	var fm *FingerprintMismatchError
	if !errors.As(err, &fm) || !errors.Is(err, transport.ErrFingerprintMismatch) {
		t.Fatalf("err = %v", err)
	}
	msg := err.Error()
	if !strings.Contains(msg, string(other.Fingerprint())) || !strings.Contains(msg, string(hub.key.Fingerprint())) {
		t.Fatalf("the error does not name both fingerprints: %s", msg)
	}
	if got := hub.secretsReceived(); len(got) != 0 {
		t.Fatalf("the secret was sent to the wrong Hub: %v", got)
	}
	if out.Len() != 0 {
		t.Fatalf("success reported: %q", out.String())
	}
	if _, ok, _ := openWorkerState(t, s.StateDir).Load(context.Background()); ok {
		t.Fatal("an enrollment was stored")
	}
	assertNoKey(t, wrong, msg, logs.String(), out.String())
}

func TestHookRetryWithTheSameKeyAndKeyPairIsIdempotent(t *testing.T) {
	hub, invite := newInviteHub(t)
	hub.loseAnswer = true // the Hub commits, the Worker never hears of it
	var out, stderr bytes.Buffer
	logs := &logBuf{}
	res := inviteResolver(config.MapSource{config.KeyServer: hub.url(), config.KeyInviteKey: invite.String()}, nil, nil)
	s := inviteSettings(t, res, &out, logs)

	d := defaultInviteHookDeps()
	d.stdin, d.stderr = strings.NewReader(""), &stderr
	d.attempts = 1
	if err := d.run(context.Background(), s); err == nil {
		t.Fatal("the lost answer was not an error")
	}
	if _, ok, _ := openWorkerState(t, s.StateDir).Load(context.Background()); ok || out.Len() != 0 {
		t.Fatalf("enrolled without the Hub's answer (out %q)", out.String())
	}
	firstKey, err := openWorkerState(t, s.StateDir).LoadKey()
	if err != nil {
		t.Fatal("the key pair was not saved before connecting: ", err)
	}

	// The retry (the next start) enrolls and gets the same Worker.
	if err := runHook(t, s, "", &stderr); err != nil {
		t.Fatal(err)
	}
	enr, ok, err := openWorkerState(t, s.StateDir).Load(context.Background())
	if err != nil || !ok || enr.WorkerID != "w-1" {
		t.Fatalf("after the retry: %+v, %v, %v", enr, ok, err)
	}
	secondKey, _ := openWorkerState(t, s.StateDir).LoadKey()
	if !bytes.Equal(firstKey.Public(), secondKey.Public()) {
		t.Fatal("the retry used another key pair")
	}
	if len(hub.known) != 1 {
		t.Fatalf("the Hub enrolled %d Workers", len(hub.known))
	}
	if out.String() != "enrolled as pi, owner alice\n" {
		t.Fatalf("out = %q", out.String())
	}
}

func TestHookSurfacesTheHubRefusalAndStoresNothing(t *testing.T) {
	hub, invite := newInviteHub(t)
	hub.refuse = proto.NewError(proto.CodeInviteExhausted, "the invitation key has no uses left")
	var out, stderr bytes.Buffer
	logs := &logBuf{}
	res := inviteResolver(config.MapSource{config.KeyServer: hub.url(), config.KeyInviteKey: invite.String()}, nil, nil)
	s := inviteSettings(t, res, &out, logs)

	err := runHook(t, s, "", &stderr)
	var pe *proto.Error
	if !errors.As(err, &pe) || pe.Code != proto.CodeInviteExhausted {
		t.Fatalf("err = %v", err)
	}
	if got := hub.secretsReceived(); len(got) != 1 {
		t.Fatalf("a refusal was retried: %d attempts", len(got))
	}
	if _, ok, _ := openWorkerState(t, s.StateDir).Load(context.Background()); ok || out.Len() != 0 {
		t.Fatal("stored or reported after a refusal")
	}
	assertNoKey(t, invite, err.Error(), logs.String(), out.String())
}

// ------------------------------------------------------------ hook, other cases

func TestHookIgnoresAKeyWhenAlreadyEnrolled(t *testing.T) {
	hub, invite := newInviteHub(t)
	var out, stderr bytes.Buffer
	logs := &logBuf{}
	res := inviteResolver(config.MapSource{config.KeyServer: hub.url(), config.KeyInviteKey: invite.String()}, nil, nil)
	s := inviteSettings(t, res, &out, logs)
	if err := openWorkerState(t, s.StateDir).Commit(context.Background(), Enrollment{
		Pin: identity.Pin{Pub: hub.key.Public()}, WorkerID: "w-old", Name: "old", Owner: "bob", Kind: "private",
	}); err != nil {
		t.Fatal(err)
	}

	if err := runHook(t, s, "", &stderr); err != nil {
		t.Fatal(err)
	}
	if got := hub.secretsReceived(); len(got) != 0 {
		t.Fatalf("an enrolled Worker contacted the Hub: %v", got)
	}
	if out.Len() != 0 {
		t.Fatalf("out = %q", out.String())
	}
	if !strings.Contains(logs.String(), "already enrolled") || !strings.Contains(logs.String(), "level=INFO") {
		t.Fatalf("no info line: %s", logs.String())
	}
	enr, _, _ := openWorkerState(t, s.StateDir).Load(context.Background())
	if enr.WorkerID != "w-old" {
		t.Fatalf("enrollment replaced: %+v", enr)
	}
	assertNoKey(t, invite, logs.String(), out.String())
}

func TestHookWithoutAKeyDoesNothing(t *testing.T) {
	var out, stderr bytes.Buffer
	s := inviteSettings(t, inviteResolver(nil, nil, nil), &out, &logBuf{})
	if err := runHook(t, s, "", &stderr); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 || stderr.Len() != 0 {
		t.Fatalf("output without a key: %q %q", out.String(), stderr.String())
	}
	if _, err := os.Stat(filepath.Join(s.StateDir, WorkerKeyFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a key pair was created without enrollment: %v", err)
	}
	// And a nil resolver (unit tests of other modules).
	d := defaultInviteHookDeps()
	if err := d.run(context.Background(), Settings{}); err != nil {
		t.Fatal(err)
	}
}

func TestHookRejectsAMalformedKeyAndAKeyWithoutServerWithoutEchoingIt(t *testing.T) {
	var out, stderr bytes.Buffer
	logs := &logBuf{}
	bad := "mnk_notakey.notasecret"
	s := inviteSettings(t, inviteResolver(config.MapSource{config.KeyServer: "https://hub.example", config.KeyInviteKey: bad}, nil, nil), &out, logs)
	err := runHook(t, s, "", &stderr)
	if err == nil || strings.Contains(err.Error(), "notasecret") {
		t.Fatalf("err = %v", err)
	}

	invite, _ := newInviteKey(t)
	s = inviteSettings(t, inviteResolver(config.MapSource{config.KeyInviteKey: invite.String()}, nil, nil), &out, logs)
	err = runHook(t, s, "", &stderr)
	if err == nil || !strings.Contains(err.Error(), "--server") {
		t.Fatalf("no server: %v", err)
	}
	assertNoKey(t, invite, err.Error(), logs.String(), out.String())
}

func TestHookRemovesAnInviteKeyStoredInTheConfigFileAfterEnrollment(t *testing.T) {
	hub, invite := newInviteHub(t)
	var out, stderr bytes.Buffer
	logs := &logBuf{}
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	cf, err := config.LoadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	cf.Set(config.KeyInviteKey, invite.String())
	cf.Set(config.KeyServer, hub.url())
	cf.Set(config.KeyName, "pi")
	if err := cf.Save(); err != nil {
		t.Fatal(err)
	}
	s := inviteSettings(t, inviteResolver(nil, nil, cf), &out, logs)

	if err := runHook(t, s, "", &stderr); err != nil {
		t.Fatal(err)
	}
	if stderr.Len() != 0 {
		t.Fatalf("a config file key warned: %q", stderr.String())
	}
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "mnk_") || strings.Contains(string(data), config.KeyInviteKey) {
		t.Fatalf("the config file keeps the invitation key: %s", data)
	}
	if !strings.Contains(string(data), hub.url()) {
		t.Fatalf("other settings were lost: %s", data)
	}
}

func TestHookKeepsAConfigFileKeyWhenEnrollmentFails(t *testing.T) {
	hub, invite := newInviteHub(t)
	hub.refuse = proto.NewError(proto.CodeInviteInvalid, "the invitation key is not valid")
	var out, stderr bytes.Buffer
	cf, err := config.LoadFile(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	cf.Set(config.KeyInviteKey, invite.String())
	cf.Set(config.KeyServer, hub.url())
	s := inviteSettings(t, inviteResolver(nil, nil, cf), &out, &logBuf{})
	if err := runHook(t, s, "", &stderr); err == nil {
		t.Fatal("no error")
	}
	if _, has, _ := cf.Lookup(config.KeyInviteKey); !has {
		t.Fatal("the key was removed although enrollment failed")
	}
}

func TestInviteHookIsRegisteredAsAPreStartHook(t *testing.T) {
	found := false
	for _, n := range DefaultRegistry.PreStartNames() {
		found = found || n == HookEnrollInvite
	}
	if !found {
		t.Fatalf("pre-start hooks = %v", DefaultRegistry.PreStartNames())
	}
}

func TestFingerprintMismatchErrorText(t *testing.T) {
	e := &FingerprintMismatchError{Want: "wantfpr", Got: "gotfpr"}
	if !strings.Contains(e.Error(), "wantfpr") || !strings.Contains(e.Error(), "gotfpr") || !strings.Contains(e.Error(), "not sent") {
		t.Fatalf("text = %s", e.Error())
	}
	if !strings.Contains((&FingerprintMismatchError{Want: "w"}).Error(), "(unknown)") {
		t.Fatal("unknown fingerprint not shown")
	}
}

// assertNoKey fails when any text carries the invitation key or its secret.
func assertNoKey(t *testing.T, k identity.InviteKey, texts ...string) {
	t.Helper()
	full, secret := k.String(), inviteSecretPart(k)
	for _, s := range texts {
		if strings.Contains(s, full) || strings.Contains(s, secret) {
			t.Fatalf("the invitation key leaked: %q", s)
		}
	}
}

// ------------------------------------------------------------ enrolled Worker, key not touched

// failingReader fails the test when anything reads it.
type failingReader struct{ t *testing.T }

func (r failingReader) Read([]byte) (int, error) {
	r.t.Helper()
	r.t.Error("an enrolled Worker read stdin")
	return 0, errors.New("must not be read")
}

func enrolledSettings(t *testing.T, hub *inviteHub, res *config.Resolver, logs *logBuf) (Settings, *bytes.Buffer) {
	t.Helper()
	var out bytes.Buffer
	s := inviteSettings(t, res, &out, logs)
	if err := openWorkerState(t, s.StateDir).Commit(context.Background(), Enrollment{
		Pin: identity.Pin{Pub: hub.key.Public()}, WorkerID: "w-old", Name: "old", Owner: "bob", Kind: "private",
	}); err != nil {
		t.Fatal(err)
	}
	return s, &out
}

func assertIgnoredWithInfoLine(t *testing.T, err error, hub *inviteHub, logs *logBuf, out *bytes.Buffer) {
	t.Helper()
	if err != nil {
		t.Fatalf("an enrolled Worker failed to start: %v", err)
	}
	if got := hub.secretsReceived(); len(got) != 0 {
		t.Fatalf("an enrolled Worker contacted the Hub: %v", got)
	}
	if out.Len() != 0 {
		t.Fatalf("out = %q", out.String())
	}
	if !strings.Contains(logs.String(), "already enrolled") || !strings.Contains(logs.String(), "level=INFO") {
		t.Fatalf("no info line: %s", logs.String())
	}
}

func TestHookEnrolledWorkerIgnoresAMissingKeyFile(t *testing.T) {
	hub, _ := newInviteHub(t)
	logs := &logBuf{}
	missing := filepath.Join(t.TempDir(), "invite.key") // deleted after enrollment (Docker form)
	res := inviteResolver(nil, map[string]string{"MINION_INVITE_KEY_FILE": missing}, nil)
	s, out := enrolledSettings(t, hub, res, logs)
	var stderr bytes.Buffer
	err := runHook(t, s, "", &stderr)
	assertIgnoredWithInfoLine(t, err, hub, logs, out)
}

func TestHookEnrolledWorkerIgnoresAKeyFileFlagThatIsGone(t *testing.T) {
	hub, _ := newInviteHub(t)
	logs := &logBuf{}
	res := inviteResolver(config.MapSource{config.KeyInviteKey: filepath.Join(t.TempDir(), "gone")}, nil, nil)
	s, out := enrolledSettings(t, hub, res, logs)
	var stderr bytes.Buffer
	assertIgnoredWithInfoLine(t, runHook(t, s, "", &stderr), hub, logs, out)
}

func TestHookEnrolledWorkerNeverReadsStdin(t *testing.T) {
	hub, _ := newInviteHub(t)
	logs := &logBuf{}
	res := inviteResolver(config.MapSource{config.KeyInviteKey: "-"}, nil, nil)
	s, out := enrolledSettings(t, hub, res, logs)
	d := defaultInviteHookDeps()
	d.stdin, d.stderr = failingReader{t}, io.Discard
	err := d.run(context.Background(), s)
	assertIgnoredWithInfoLine(t, err, hub, logs, out)
}

func TestHookEnrolledWorkerIgnoresAMalformedKeyAndNeedsNoServer(t *testing.T) {
	hub, _ := newInviteHub(t)
	logs := &logBuf{}
	res := inviteResolver(config.MapSource{config.KeyInviteKey: "mnk_garbage.garbage"}, nil, nil) // no --server
	s, out := enrolledSettings(t, hub, res, logs)
	var stderr bytes.Buffer
	assertIgnoredWithInfoLine(t, runHook(t, s, "", &stderr), hub, logs, out)
	if strings.TrimSpace(stderr.String()) != config.InviteKeyArgvWarning {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestHookEnrolledWorkerRemovesAnInviteKeyFromTheConfigFile(t *testing.T) {
	hub, invite := newInviteHub(t)
	logs := &logBuf{}
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	cf, err := config.LoadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	cf.Set(config.KeyInviteKey, invite.String())
	cf.Set(config.KeyServer, hub.url())
	if err := cf.Save(); err != nil {
		t.Fatal(err)
	}
	s, out := enrolledSettings(t, hub, inviteResolver(nil, nil, cf), logs)
	var stderr bytes.Buffer
	assertIgnoredWithInfoLine(t, runHook(t, s, "", &stderr), hub, logs, out)
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "mnk_") || strings.Contains(string(data), config.KeyInviteKey) {
		t.Fatalf("the config file keeps the invitation key: %s", data)
	}
	if !strings.Contains(string(data), hub.url()) {
		t.Fatalf("other settings were lost: %s", data)
	}
}

func TestForgetStoredInviteKeyKeepsASettingChangedMeanwhile(t *testing.T) {
	invite, _ := newInviteKey(t)
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	stale, err := config.LoadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	stale.Set(config.KeyInviteKey, invite.String())
	if err := stale.Save(); err != nil {
		t.Fatal(err)
	}
	stale, err = config.LoadFile(cfgPath) // the Worker's snapshot
	if err != nil {
		t.Fatal(err)
	}
	// `minion worker config set name later` completes while the Worker waits.
	other, err := config.LoadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	other.Set(config.KeyName, "later")
	if err := other.Save(); err != nil {
		t.Fatal(err)
	}

	forgetStoredInviteKey(Settings{Resolver: inviteResolver(nil, nil, stale)})

	fresh, err := config.LoadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if v, ok, _ := fresh.Lookup(config.KeyName); !ok || v != "later" {
		t.Fatalf("the concurrent change was lost: %q %v", v, ok)
	}
	if _, ok, _ := fresh.Lookup(config.KeyInviteKey); ok {
		t.Fatal("invite_key is still stored")
	}
}

func TestHookStdinWaitEndsWithTheContext(t *testing.T) {
	pr, pw := io.Pipe()
	defer func() { _ = pw.Close() }()
	var out bytes.Buffer
	s := inviteSettings(t, inviteResolver(config.MapSource{config.KeyInviteKey: "-", config.KeyServer: "https://hub.example"}, nil, nil), &out, &logBuf{})
	d := defaultInviteHookDeps()
	d.stdin, d.stderr = pr, io.Discard
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.run(ctx, s) }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the hook stayed blocked on stdin after the cancel")
	}
}

// ------------------------------------------------------------ dial errors

func TestProductionDialerReportsTheRealCauseOfADialFailure(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close() // nothing listens here now
	invite, _ := newInviteKey(t)
	_, err = NewInviteDialer().Enroll(context.Background(), InviteParams{HubURL: "https://" + addr, Name: "pi", Invite: invite})
	if err == nil {
		t.Fatal("no error")
	}
	if strings.Contains(err.Error(), "no answer within") {
		t.Fatalf("the cause was replaced by a timeout text: %v", err)
	}
	if !strings.Contains(err.Error(), "refused") && !strings.Contains(err.Error(), "dial") {
		t.Fatalf("err = %v", err)
	}
}

func TestProductionDialerReportsATimeoutAsNoAnswer(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0") // accepts in the kernel, never answers
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	invite, _ := newInviteKey(t)
	d := inviteDialer{dialTimeout: 100 * time.Millisecond, handshakeTimeout: time.Second}
	_, err = d.Enroll(context.Background(), InviteParams{HubURL: "http://" + l.Addr().String(), Name: "pi", Invite: invite})
	if err == nil || !strings.Contains(err.Error(), "no answer within 100ms") {
		t.Fatalf("err = %v", err)
	}
}

func TestAnInvalidHubURLFailsAtOnceWithoutRetry(t *testing.T) {
	invite, _ := newInviteKey(t)
	log := &stepLog{}
	d := &fakeDialer{log: log, results: []error{errors.New("down")}}
	e := newFakeEnroller(t, &fakeState{log: log}, d, &logBuf{})
	e.cfg.HubURL = "ftp://hub.example"
	_, err := e.Enroll(context.Background(), invite)
	if err == nil || !strings.Contains(err.Error(), "https, http, wss or ws") {
		t.Fatalf("err = %v", err)
	}
	if d.calls != 0 || !reflect.DeepEqual(log.get(), []string{"load"}) {
		t.Fatalf("dial attempts %d, steps %v: the URL must be checked before any attempt", d.calls, log.get())
	}

	// The production dialer treats it as permanent too: one attempt, no retry.
	counting := &countingDialer{inner: NewInviteDialer()}
	e = newFakeEnroller(t, &fakeState{log: &stepLog{}}, counting, &logBuf{})
	e.cfg.HubURL = "https://hub.example"
	e.cfg.Dialer = counting
	counting.url = "ftp://hub.example"
	if _, err := e.Enroll(context.Background(), invite); err == nil || !strings.Contains(err.Error(), "https, http, wss or ws") {
		t.Fatalf("err = %v", err)
	}
	if counting.calls != 1 {
		t.Fatalf("attempts = %d, want exactly 1", counting.calls)
	}
}

// countingDialer counts attempts and swaps in a different URL, to reach the
// production dialer with a URL the enroller would already have refused.
type countingDialer struct {
	inner InviteDialer
	url   string
	calls int
}

func (c *countingDialer) Enroll(ctx context.Context, p InviteParams) (*InviteOutcome, error) {
	c.calls++
	p.HubURL = c.url
	return c.inner.Enroll(ctx, p)
}

func TestHookStaysSilentWhenWorkerUpAlreadyWarnedAboutTheLiteralKey(t *testing.T) {
	hub, _ := newInviteHub(t)
	logs := &logBuf{}
	res := inviteResolver(config.MapSource{config.KeyInviteKey: "mnk_garbage.garbage"}, nil, nil)
	s, out := enrolledSettings(t, hub, res, logs)
	s.InviteArgvWarned = true
	var stderr bytes.Buffer
	assertIgnoredWithInfoLine(t, runHook(t, s, "", &stderr), hub, logs, out)
	if stderr.Len() != 0 {
		t.Fatalf("the hook warned again: %q", stderr.String())
	}
}
