package workers

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/testutil/hubcert"
	"github.com/patrick-1984/ai-minion/internal/transport"
)

func link(t *testing.T, old *identity.Key, newPub []byte, seq uint64) identity.Transition {
	t.Helper()
	tr, err := identity.NewTransition(old, newPub, seq, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return tr
}

func TestChainUpToCutsTheLinksPastTheRootInUse(t *testing.T) {
	a, b, c := newKey(t), newKey(t), newKey(t)
	l1, l2 := link(t, a, b.Public(), 1), link(t, b, c.Public(), 2)
	chain := []identity.Transition{l1, l2}
	if got := chainUpTo(chain, c.Public()); len(got) != 2 {
		t.Errorf("up to the newest root: %d links", len(got))
	}
	if got := chainUpTo(chain, b.Public()); len(got) != 1 || got[0].Seq != 1 {
		t.Errorf("up to the middle root: %v", got)
	}
	if got := chainUpTo(chain, a.Public()); got != nil {
		t.Errorf("up to the first root: %v", got)
	}
	other := newKey(t)
	if got := chainUpTo(chain, other.Public()); len(got) != 2 {
		t.Errorf("a root the chain does not mention: %d links", len(got))
	}
}

func TestChainSourceReturnsTheStoredChain(t *testing.T) {
	h := newTestHub(t, nil)
	src := ChainSource(h.st)
	chain, err := src(context.Background())
	if err != nil || len(chain) != 0 {
		t.Fatalf("empty chain = %v, %v", chain, err)
	}
}

// A Worker pinned to the old root follows the chain to the new root of a
// rotated Hub: the hello carries the new root and a certificate under it, the
// chain is verified from the stored pin (design 11.2 steps b and c).
func TestAWorkerPinnedToTheOldRootFollowsTheChainToTheNewRoot(t *testing.T) {
	oldHub := hubcert.New(t)
	newHub := hubcert.New(t)
	tr := link(t, oldHub.Root, newHub.RootPub(), 1)
	h := newTestHub(t, func(o *ServiceOptions) {
		o.Chain = func(context.Context) ([]identity.Transition, error) { return []identity.Transition{tr}, nil }
	})
	h.id.Activate(newHub)
	key := newKey(t)
	h.enrol(key, "owner-1", "pi")

	cfg := h.workerConfig(key, "send_text")
	cfg.Pin = identity.Pin{Pub: oldHub.RootPub()}
	res, err := h.dial(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Channel.Close()
	if !res.PinChanged || res.Pin.Seq != 1 || !bytes.Equal(res.Pin.Pub, newHub.RootPub()) {
		t.Errorf("pin = %+v changed=%v", res.Pin, res.PinChanged)
	}
	if res.Ratchet.MaxSerial != newHub.Cert.Serial || !bytes.Equal(res.Ratchet.Root, newHub.RootPub()) {
		t.Errorf("ratchet = %+v: a new root starts a new scope", res.Ratchet)
	}
	decodeWelcome(t, res)
}

func TestAWorkerRejectsABadChainAndKeepsItsPin(t *testing.T) {
	oldHub := hubcert.New(t)
	newHub := hubcert.New(t)
	stranger := newKey(t)
	bad := link(t, stranger, newHub.RootPub(), 1) // signed by a key the Worker never pinned
	h := newTestHub(t, func(o *ServiceOptions) {
		o.Chain = func(context.Context) ([]identity.Transition, error) { return []identity.Transition{bad}, nil }
	})
	h.id.Activate(newHub)
	cfg := h.workerConfig(newKey(t))
	cfg.Pin = identity.Pin{Pub: oldHub.RootPub()}
	if _, err := h.dial(cfg); err == nil || !errors.Is(err, transport.ErrPinMismatch) {
		t.Fatalf("dial = %v, want a pin mismatch", err)
	}
}

func TestTheLargestChainFitsTheHandshakeFrame(t *testing.T) {
	keys := make([]*identity.Key, MaxChainLinks+1)
	for i := range keys {
		keys[i] = newKey(t)
	}
	chain := make([]identity.Transition, MaxChainLinks)
	for i := range chain {
		chain[i] = link(t, keys[i], keys[i+1].Public(), uint64(i+1))
	}
	// The frame is the hello with the chain and the certificate.
	hello := transport.HubHello{
		Type: proto.TypeHello, Version: proto.Current, HubPub: make([]byte, 32), HubCert: make([]byte, identity.HubCertSize),
		EphH: make([]byte, 32), Nonce: make([]byte, 32), Chain: chain,
	}
	var buf bytes.Buffer
	if _, err := buf.Write(mustJSON(t, hello)); err != nil {
		t.Fatal(err)
	}
	if buf.Len() > proto.HandshakeFrameLimit {
		t.Fatalf("a hello with %d links is %d bytes, over the %d byte handshake frame limit", MaxChainLinks, buf.Len(), proto.HandshakeFrameLimit)
	}
}
