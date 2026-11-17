package workers

// W9 Hub part (design 4.6, addendum C5): the welcome states how the request
// reached the Hub.

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/transport"
)

// serveOn replaces the test server of h by srv (not started yet).
func (h *testHub) serveOn(srv *httptest.Server, tlsOn bool) {
	h.t.Helper()
	h.srv.Close()
	mux := http.NewServeMux()
	mux.Handle("GET "+transport.ConnectPath, h.svc.Handler())
	srv.Config.Handler = mux
	if tlsOn {
		srv.StartTLS()
	} else {
		srv.Start()
	}
	h.srv = srv
	h.t.Cleanup(srv.Close)
}

func welcomeOver(t *testing.T, h *testHub) Welcome {
	t.Helper()
	key := newKey(t)
	h.enrol(key, "owner-1", "w1")
	res, err := h.dial(h.workerConfig(key))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = res.Channel.Close() })
	return decodeWelcome(t, res)
}

func TestWelcomeStatesOwnTLSWithTheServedKey(t *testing.T) {
	h := newTestHub(t, nil)
	served := bytes.Repeat([]byte{0xab}, 32)
	srv := httptest.NewUnstartedServer(nil)
	srv.Config.ConnContext = func(ctx context.Context, _ net.Conn) context.Context {
		return WithServedTLSKey(ctx, func() []byte { return served })
	}
	h.serveOn(srv, true)
	w := welcomeOver(t, h)
	if w.HubTLS == nil || w.HubTLS.Mode != HubTLSOwn || !bytes.Equal(w.HubTLS.KeyHash, served) {
		t.Errorf("hub_tls = %+v", w.HubTLS)
	}
}

func TestWelcomeStatesOffBehindAPlainListener(t *testing.T) {
	h := newTestHub(t, nil)
	h.serveOn(httptest.NewUnstartedServer(nil), false)
	w := welcomeOver(t, h)
	if w.HubTLS == nil || w.HubTLS.Mode != HubTLSOff || len(w.HubTLS.KeyHash) != 0 {
		t.Errorf("hub_tls = %+v", w.HubTLS)
	}
}

// A Worker of v0.10 (its welcome struct has no hub_tls) still decodes a
// welcome that carries it, and the member is in the exact wire form.
func TestAV010WelcomeStructDecodesAWelcomeWithHubTLS(t *testing.T) {
	type v010Welcome struct {
		Type     string `json:"type"`
		WorkerID string `json:"worker_id"`
		Name     string `json:"name"`
		Owner    string `json:"owner"`
		OwnerID  string `json:"owner_id"`
		Kind     string `json:"kind"`
		Pending  bool   `json:"pending,omitempty"`
		Policy   Policy `json:"policy"`
		proto.Range
	}
	in := Welcome{Type: proto.TypeWelcome, WorkerID: "0190a1b2-c3d4-7e5f-8a6b-000000000001", Name: "w1", Owner: "alice", OwnerID: "u1",
		Kind: "private", HubTLS: &HubTLS{Mode: HubTLSOwn, KeyHash: bytes.Repeat([]byte{1}, 32)}, Range: proto.SupportedRange(proto.Version{})}
	body, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatal(err)
	}
	if got := string(m["hub_tls"]); got != `{"mode":"own","key_sha256":"AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE"}` {
		t.Errorf("hub_tls on the wire = %s", got)
	}
	var old v010Welcome
	if err := json.Unmarshal(body, &old); err != nil || old.WorkerID != in.WorkerID || old.Name != "w1" || old.OwnerID != "u1" {
		t.Errorf("v0.10 decode = %+v, %v", old, err)
	}
	off, _ := json.Marshal(Welcome{HubTLS: &HubTLS{Mode: HubTLSOff}})
	if !bytes.Contains(off, []byte(`"hub_tls":{"mode":"off"}`)) {
		t.Errorf("off on the wire = %s", off)
	}
}
