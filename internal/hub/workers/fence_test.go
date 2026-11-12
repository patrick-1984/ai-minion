package workers

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/testutil/hubcert"
	"github.com/patrick-1984/ai-minion/internal/transport"
)

func connectWelcomed(t *testing.T, h *testHub, key *identity.Key) (*transport.WorkerResult, *transport.Session) {
	t.Helper()
	res, err := h.dial(h.workerConfig(key, "send_text"))
	if err != nil {
		t.Fatal(err)
	}
	decodeWelcome(t, res)
	sess, err := transport.NewSession(res.Channel, transport.RoleWorker, transport.SessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	return res, sess
}

func TestHelloCarriesTheCertificateAndSigHubIsMadeByTheOperationalKey(t *testing.T) {
	h := newTestHub(t, nil)
	key := newKey(t)
	h.enrol(key, "o", "pi")
	res, _ := connectWelcomed(t, h, key)
	if !bytes.Equal(res.HubPub, h.hub.RootPub()) {
		t.Error("hub_pub is not the root")
	}
	if res.OpPub == nil || !bytes.Equal(res.OpPub, h.hub.Op.Public()) || bytes.Equal(res.OpPub, res.HubPub) {
		t.Error("the handshake was not authenticated by the operational key")
	}
	if res.Cert.Serial != h.hub.Cert.Serial || res.CertSHA256 != h.hub.Cert.SHA256() {
		t.Error("the certificate of the hello is not the Hub's")
	}
}

// A 2.0 Worker gets proto_incompatible; the Hub speaks only 3.x.
func TestAProtocol2WorkerIsRefused(t *testing.T) {
	h := newTestHub(t, nil)
	cfg := h.workerConfig(newKey(t))
	cfg.Protocols = proto.Range{Min: proto.Version{Major: 2}, Max: proto.Version{Major: 2}}
	if _, err := h.dial(cfg); proto.CodeOf(err) != proto.CodeProtoIncompatible {
		t.Fatalf("err = %v", err)
	}
}

// An activation closes all sessions; a handshake that took the old snapshot,
// stalled across the activation and then reaches the registry, fails at
// registration: no session survives with the old signer (design 9.6).
func TestActivationClosesSessionsAndAStalledHandshakeFailsAtRegistration(t *testing.T) {
	h := newTestHub(t, nil)
	k1, k2 := newKey(t), newKey(t)
	w1 := h.enrol(k1, "o", "one")
	w2 := h.enrol(k2, "o", "two")
	_, s1 := connectWelcomed(t, h, k1)
	eventually(t, "the first Worker to be online", func() bool { return h.svc.Registry().IsOnline(w1.ID) })

	h.svc.handler.beforeRegister = func() {
		// Step 6 (swap) and step 8 (close all) of a re-issue, while the second
		// handshake holds the old snapshot.
		h.id.Activate(h.hub.Next(t, h.hub.Cert.Serial+1))
	}
	_, s2 := connectWelcomed(t, h, k2)

	select {
	case <-s1.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the activation did not close the registered session")
	}
	select {
	case <-s2.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the stalled handshake kept a session with the superseded signer")
	}
	eventually(t, "nobody to be online", func() bool { return h.svc.Registry().Len() == 0 })
	if h.svc.Registry().IsOnline(w2.ID) {
		t.Error("the stalled connection was registered")
	}
	// The next handshake uses the new snapshot and registers.
	h.svc.handler.beforeRegister = nil
	connectWelcomed(t, h, k2)
	eventually(t, "the reconnect to register", func() bool { return h.svc.Registry().IsOnline(w2.ID) })
}

// A master-key rotation is not an exception: it swaps the full snapshot, raises
// the generation and closes all sessions, and a stalled handshake that took the
// old root-and-key set fails at registration.
func TestARotationClosesSessionsAndAStalledHandshakeFailsAtRegistration(t *testing.T) {
	h := newTestHub(t, nil)
	k1, k2 := newKey(t), newKey(t)
	w1 := h.enrol(k1, "o", "one")
	w2 := h.enrol(k2, "o", "two")
	_, s1 := connectWelcomed(t, h, k1)
	eventually(t, "online", func() bool { return h.svc.Registry().IsOnline(w1.ID) })
	h.svc.handler.beforeRegister = func() { h.id.Activate(hubcert.New(t)) } // another root
	_, s2 := connectWelcomed(t, h, k2)
	for name, s := range map[string]*transport.Session{"registered": s1, "stalled": s2} {
		select {
		case <-s.Done():
		case <-time.After(5 * time.Second):
			t.Fatalf("the %s session survived the rotation", name)
		}
	}
	eventually(t, "nobody online", func() bool { return h.svc.Registry().Len() == 0 })
	if h.svc.Registry().IsOnline(w2.ID) {
		t.Error("the stalled connection registered with the superseded root")
	}
}

func TestAnActivationBeforeTheRegistrationOfAnEarlierConnectionClosesIt(t *testing.T) {
	h := newTestHub(t, nil)
	k := newKey(t)
	w := h.enrol(k, "o", "one")
	_, s := connectWelcomed(t, h, k)
	eventually(t, "online", func() bool { return h.svc.Registry().IsOnline(w.ID) })
	h.id.Activate(h.hub.Next(t, h.hub.Cert.Serial+1))
	select {
	case <-s.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the session survived the activation")
	}
}

// The revoke answer at reconnect is signed by the operational key of THAT
// connection's snapshot: after a re-issue it is the new key, never the root or
// the previous key (design 12.4).
func TestTombstoneRevokeIsSignedByTheConnectionsOperationalKeyAcrossAReissue(t *testing.T) {
	h := newTestHub(t, nil)
	k := newKey(t)
	w := h.enrol(k, "o", "gone")
	if err := h.st.TombstoneWorker(t.Context(), w.ID, "admin", time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	oldOp := h.hub.Op.Public()
	h.id.Activate(h.hub.Next(t, h.hub.Cert.Serial+1))
	res, err := h.dial(h.workerConfig(k))
	if err != nil {
		t.Fatal(err)
	}
	m := readControl(t, res)
	if frameType(t, m) != proto.TypeRevoke {
		t.Fatalf("frame = %v", m)
	}
	var f RevokeFrame
	raw, _ := json.Marshal(m)
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	if err := VerifyRevoke(res.OpPub, res.TranscriptHash[:], f); err != nil {
		t.Fatalf("not signed by the connection's operational key: %v", err)
	}
	if bytes.Equal(res.OpPub, oldOp) {
		t.Fatal("the connection still presents the previous operational key")
	}
	for name, pub := range map[string][]byte{"root": res.HubPub, "previous operational key": oldOp, "worker key": k.Public()} {
		if VerifyRevoke(pub, res.TranscriptHash[:], f) == nil {
			t.Errorf("the revoke verifies against the %s", name)
		}
	}
	other := make([]byte, len(res.TranscriptHash))
	if VerifyRevoke(res.OpPub, other, f) == nil {
		t.Error("the revoke verifies on another connection's transcript")
	}
}

func TestTheLiveRevokeUsesTheKeyOfTheLiveConnection(t *testing.T) {
	h := newTestHub(t, nil)
	k := newKey(t)
	w := h.enrol(k, "o", "live")
	res, sess := connectWelcomed(t, h, k)
	eventually(t, "online", func() bool { return h.svc.Registry().IsOnline(w.ID) })
	f, ok, err := h.svc.Registry().SignRevoke(w.ID)
	if err != nil || !ok {
		t.Fatalf("SignRevoke = %v %v", ok, err)
	}
	if err := VerifyRevoke(res.OpPub, res.TranscriptHash[:], f); err != nil {
		t.Fatalf("the live revoke is not signed with the connection's key: %v", err)
	}
	if VerifyRevoke(res.HubPub, res.TranscriptHash[:], f) == nil {
		t.Error("the live revoke verifies against the root")
	}
	_ = sess
	// A registration without a snapshot has no key and cannot sign.
	h.svc.Registry().Add(Online{WorkerID: "w-nokey"}, newFakeSession())
	if _, ok, err := h.svc.Registry().SignRevoke("w-nokey"); !ok || err == nil {
		t.Errorf("SignRevoke without a key = %v %v", ok, err)
	}
}
