package workers

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/hubid"
	"github.com/patrick-1984/ai-minion/internal/hub/hubid/hubidtest"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/transport"
)

type idClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *idClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *idClock) Add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// hubidHub is a testHub whose identity is a real hubid.Service on the same
// store, with a fake clock.
func hubidHub(t *testing.T, provision bool) (*testHub, *hubid.Service, *idClock) {
	t.Helper()
	dir := t.TempDir()
	st := openStore(t, dir)
	clk := &idClock{t: time.Now().Truncate(time.Millisecond)}
	var svc *hubid.Service
	if provision {
		svc = hubidtest.Open(t, st, dir, hubidtest.WithNow(clk.Now))
	} else {
		var err error
		svc, err = hubid.Open(context.Background(), hubid.Options{DataDir: dir, Store: st, Sealer: hubidtest.NewSealer(), Now: clk.Now})
		if err != nil {
			t.Fatal(err)
		}
	}
	h := newTestHubOn(t, st, svc, nil)
	return h, svc, clk
}

func getConnect(t *testing.T, h *testHub) (*http.Response, proto.Code) {
	t.Helper()
	resp, err := h.srv.Client().Get(h.srv.URL + transport.ConnectPath)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var env struct {
		Error struct {
			Code proto.Code `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(body, &env)
	return resp, env.Error.Code
}

func TestAHubWithoutACertificateRefusesWorkersBeforeTheUpgrade(t *testing.T) {
	h, svc, _ := hubidHub(t, false)
	if svc.State() != hubid.StatePristine {
		t.Fatalf("state = %v", svc.State())
	}
	resp, code := getConnect(t, h)
	if resp.StatusCode != http.StatusServiceUnavailable || code != proto.CodeWorkerOffline {
		t.Fatalf("B0: status %d code %q", resp.StatusCode, code)
	}
	if resp.Header.Get("Retry-After") == "" {
		t.Error("no Retry-After")
	}
	// B2: a sealed root without a certificate: set the passphrase issues one in
	// the same step, so B2 is reached by a refusing Worker service only before.
	if _, err := svc.SetPassphrase(context.Background(), hubidtest.Actor, hubidtest.Passphrase, hubidtest.Passphrase, hubid.IssueOptions{}); err != nil {
		t.Fatal(err)
	}
	k := newKey(t)
	h.enrol(k, "o", "pi")
	cfg := h.workerConfig(k)
	root := svc.Generation()
	_ = root
	sn, err := svc.Current()
	if err != nil {
		t.Fatal(err)
	}
	cfg.Pin.Pub = sn.RootPub
	res, err := h.dial(cfg)
	if err != nil {
		t.Fatalf("after the passphrase is set the Hub serves: %v", err)
	}
	defer res.Channel.Close()
	decodeWelcome(t, res)
}

func TestAnExpiredCertificateAnswers503HubCertExpiredBeforeTheUpgrade(t *testing.T) {
	h, svc, clk := hubidHub(t, true)
	clk.Add(181 * 24 * time.Hour)
	if _, err := svc.Current(); err == nil {
		t.Fatal("the expired certificate is still served")
	}
	resp, code := getConnect(t, h)
	if resp.StatusCode != http.StatusServiceUnavailable || code != proto.CodeHubCertExpired {
		t.Fatalf("status %d code %q", resp.StatusCode, code)
	}
	if resp.Header.Get("Retry-After") == "" {
		t.Error("no Retry-After")
	}
}

func TestEveryActivationClosesSessionsAndTheGenerationRisesButNotForAPassphraseChange(t *testing.T) {
	h, svc, _ := hubidHub(t, true)
	k := newKey(t)
	w := h.enrol(k, "o", "pi")
	sn, _ := svc.Current()
	cfg := h.workerConfig(k, "send_text")
	cfg.Pin.Pub = sn.RootPub
	connect := func() *transport.Session {
		res, err := h.dial(cfg)
		if err != nil {
			t.Fatal(err)
		}
		decodeWelcome(t, res)
		s, err := transport.NewSession(res.Channel, transport.RoleWorker, transport.SessionOptions{})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = s.Close() })
		eventually(t, "online", func() bool { return h.svc.Registry().IsOnline(w.ID) })
		return s
	}
	s := connect()
	gen := svc.Generation()
	if _, err := svc.ChangePassphrase(context.Background(), hubidtest.Actor, hubidtest.Passphrase, "another long passphrase!!", "another long passphrase!!"); err != nil {
		t.Fatal(err)
	}
	if svc.Generation() != gen {
		t.Error("a passphrase change raised the generation")
	}
	select {
	case <-s.Done():
		t.Fatal("a passphrase change closed a session")
	case <-time.After(200 * time.Millisecond):
	}
	if _, err := svc.Reissue(context.Background(), hubidtest.Actor, "another long passphrase!!", hubid.IssueOptions{}); err != nil {
		t.Fatal(err)
	}
	if svc.Generation() != gen+1 {
		t.Errorf("generation = %d, want %d", svc.Generation(), gen+1)
	}
	select {
	case <-s.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the re-issue did not close the session")
	}
	// The Worker reconnects to the new certificate with a higher serial.
	s = connect()
	if _, err := svc.RotateMaster(context.Background(), hubidtest.Actor, "another long passphrase!!", "a third long passphrase!!", "a third long passphrase!!", hubid.IssueOptions{}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-s.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the rotation did not close the session")
	}
	if svc.Generation() != gen+2 {
		t.Errorf("generation = %d, want %d", svc.Generation(), gen+2)
	}
	// After a rotation the Worker follows the chain from its old pin.
	res, err := h.dial(cfg)
	if err != nil {
		t.Fatalf("the Worker did not follow the rotation: %v", err)
	}
	defer res.Channel.Close()
	if !res.PinChanged || res.Pin.Seq != 1 {
		t.Errorf("pin = %+v", res.Pin)
	}
}

func TestTheHubClosesEverySessionAtNotAfter(t *testing.T) {
	h, svc, clk := hubidHub(t, true)
	k := newKey(t)
	w := h.enrol(k, "o", "pi")
	sn, _ := svc.Current()
	cfg := h.workerConfig(k, "send_text")
	cfg.Pin.Pub = sn.RootPub
	res, err := h.dial(cfg)
	if err != nil {
		t.Fatal(err)
	}
	decodeWelcome(t, res)
	s, _ := transport.NewSession(res.Channel, transport.RoleWorker, transport.SessionOptions{})
	defer s.Close()
	eventually(t, "online", func() bool { return h.svc.Registry().IsOnline(w.ID) })
	clk.Add(time.Duration(sn.Cert.NotAfterMs-clk.Now().UnixMilli())*time.Millisecond - time.Millisecond)
	if _, err := svc.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-s.Done():
		t.Fatal("closed one millisecond before not_after")
	case <-time.After(200 * time.Millisecond):
	}
	clk.Add(time.Millisecond) // now == not_after
	if _, err := svc.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-s.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the Hub did not close the session at not_after")
	}
}
