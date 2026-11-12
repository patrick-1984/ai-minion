package transport

import (
	"bytes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/yamux"
	"golang.org/x/crypto/chacha20poly1305"

	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/pake"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// ---------------------------------------------------------------- helpers

func mustKey(t *testing.T) *identity.Key {
	t.Helper()
	k, err := identity.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// recConn records what is written through it.
type recConn struct {
	net.Conn
	mu  sync.Mutex
	buf bytes.Buffer
}

func (r *recConn) Write(p []byte) (int, error) {
	r.mu.Lock()
	r.buf.Write(p)
	r.mu.Unlock()
	return r.Conn.Write(p)
}

func (r *recConn) written() []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	return bytes.Clone(r.buf.Bytes())
}

// frameTypes returns the "type" of every frame in raw wire bytes.
func frameTypes(t *testing.T, wire []byte) []string {
	t.Helper()
	var types []string
	r := bytes.NewReader(wire)
	for r.Len() > 0 {
		body, err := proto.ReadFrame(r, proto.HandshakeFrameLimit)
		if err != nil {
			t.Fatalf("wire is not handshake frames: %v", err)
		}
		ty, _ := proto.FrameType(body)
		types = append(types, ty)
	}
	return types
}

type outcome struct {
	worker    *WorkerResult
	workerErr error
	hub       *HubResult
	hubErr    error
	wWire     []byte // what the Worker wrote
	hWire     []byte // what the Hub wrote
}

// runPair runs both handshakes over net.Pipe, with an optional relay between
// them that may rewrite frames.
func runPair(t *testing.T, wc WorkerConfig, hc HubConfig, mutate func(fromWorker bool, idx int, body []byte) []byte) outcome {
	t.Helper()
	var wEnd, hEnd, wPeer, hPeer net.Conn
	if mutate == nil {
		wEnd, hEnd = net.Pipe() // direct: the channel runs on it after the handshake
	} else {
		wEnd, wPeer = net.Pipe()
		hEnd, hPeer = net.Pipe()
	}
	wrec := &recConn{Conn: wEnd}
	hrec := &recConn{Conn: hEnd}
	if wc.Timeout == 0 {
		wc.Timeout = 5 * time.Second
	}
	if hc.Timeout == 0 {
		hc.Timeout = 5 * time.Second
	}

	// With a mutator a relay forwards handshake frames between the two ends
	// and rewrites them. It only understands handshake frames, so mutated
	// runs are expected to fail before the channel starts.
	var relay sync.WaitGroup
	forward := func(from, to net.Conn, fromWorker bool) {
		defer relay.Done()
		defer to.Close()
		for idx := 0; ; idx++ {
			body, err := proto.ReadFrame(from, proto.HandshakeFrameLimit)
			if err != nil {
				return
			}
			body = mutate(fromWorker, idx, body)
			if _, err := sendFrameRaw(to, body); err != nil {
				return
			}
		}
	}
	if mutate != nil {
		relay.Add(2)
		go forward(wPeer, hPeer, true)
		go forward(hPeer, wPeer, false)
	}

	var o outcome
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); o.worker, o.workerErr = WorkerHandshake(wrec, wc) }()
	go func() { defer wg.Done(); o.hub, o.hubErr = HubHandshake(hrec, hc) }()
	wg.Wait()
	if mutate != nil {
		_ = wPeer.Close()
		_ = hPeer.Close()
		relay.Wait()
	}
	o.wWire, o.hWire = wrec.written(), hrec.written()
	return o
}

// sendFrameRaw writes body as one frame without re-marshalling it.
func sendFrameRaw(w io.Writer, body []byte) (int, error) {
	buf := append([]byte{byte(len(body) >> 24), byte(len(body) >> 16), byte(len(body) >> 8), byte(len(body))}, body...)
	return w.Write(buf)
}

func (o outcome) mustSucceed(t *testing.T) {
	t.Helper()
	if o.workerErr != nil || o.hubErr != nil {
		t.Fatalf("worker error: %v; hub error: %v", o.workerErr, o.hubErr)
	}
}

// roundTrip checks that the two channels work in both directions.
func (o outcome) roundTrip(t *testing.T) {
	t.Helper()
	defer o.worker.Channel.Close()
	defer o.hub.Channel.Close()
	for _, d := range []struct {
		from, to *Channel
		msg      string
	}{
		{o.worker.Channel, o.hub.Channel, "worker to hub"},
		{o.hub.Channel, o.worker.Channel, "hub to worker"},
	} {
		errc := make(chan error, 1)
		go func() { _, err := d.from.Write([]byte(d.msg)); errc <- err }()
		got := make([]byte, len(d.msg))
		if _, err := io.ReadFull(d.to, got); err != nil || string(got) != d.msg {
			t.Fatalf("%s: got %q, %v", d.msg, got, err)
		}
		if err := <-errc; err != nil {
			t.Fatal(err)
		}
	}
}

func connectConfigs(t *testing.T) (WorkerConfig, HubConfig, *identity.Key, *identity.Key) {
	t.Helper()
	wk, hub := mustKey(t), newHub(t)
	hk := hub.root
	return WorkerConfig{
			Key: wk, Name: "lab-pi", Ops: []string{"snapshot"}, Mode: ModeConnect,
			Pin: identity.Pin{Pub: hk.Public()},
		},
		hub.cfg(), wk, hk
}

// ---------------------------------------------------------------- vectors

func TestTranscriptHashVectors(t *testing.T) {
	var v vectorFile
	loadVectors(t, &v)
	if len(v.Transcript) == 0 {
		t.Fatal("no transcript vectors")
	}
	for _, tv := range v.Transcript {
		got := TranscriptHash([]byte(tv.WorkerHello), []byte(tv.HubHello))
		if hex.EncodeToString(got[:]) != tv.Hash {
			t.Errorf("%s: hash %x, want %s", tv.Name, got, tv.Hash)
		}
	}
	// The length prefix separates the two hellos.
	a := TranscriptHash([]byte("ab"), []byte("c"))
	b := TranscriptHash([]byte("a"), []byte("bc"))
	if a == b {
		t.Error("hello boundary is not part of the hash")
	}
}

func TestKeyDerivationVectors(t *testing.T) {
	var v vectorFile
	loadVectors(t, &v)
	if len(v.KDF) == 0 {
		t.Fatal("no kdf vectors")
	}
	dec := func(s string) []byte {
		b, err := hex.DecodeString(s)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	for _, kv := range v.KDF {
		w, err := ecdh.X25519().NewPrivateKey(dec(kv.EphWPriv))
		if err != nil {
			t.Fatal(err)
		}
		h, err := ecdh.X25519().NewPrivateKey(dec(kv.EphHPriv))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(w.PublicKey().Bytes(), dec(kv.EphWPub)) || !bytes.Equal(h.PublicKey().Bytes(), dec(kv.EphHPub)) {
			t.Fatalf("%s: ephemeral public keys differ from the vector", kv.Name)
		}
		dhW, err := w.ECDH(h.PublicKey())
		if err != nil {
			t.Fatal(err)
		}
		dhH, _ := h.ECDH(w.PublicKey())
		if !bytes.Equal(dhW, dec(kv.DH)) || !bytes.Equal(dhH, dec(kv.DH)) {
			t.Fatalf("%s: X25519 secret differs from the vector", kv.Name)
		}
		keys, err := deriveKeys(dhW, dec(kv.ISK), dec(kv.TranscriptHash))
		if err != nil {
			t.Fatal(err)
		}
		if hex.EncodeToString(keys.workerToHub[:]) != kv.WorkerToHub || hex.EncodeToString(keys.hubToWorker[:]) != kv.HubToWorker {
			t.Errorf("%s: keys %x / %x, want %s / %s", kv.Name, keys.workerToHub, keys.hubToWorker, kv.WorkerToHub, kv.HubToWorker)
		}
	}
	// The CPace ISK and the transcript each change both keys.
	base, _ := deriveKeys([]byte("dh"), nil, []byte("hash"))
	for name, k := range map[string]func() (channelKeys, error){
		"isk":  func() (channelKeys, error) { return deriveKeys([]byte("dh"), []byte("isk"), []byte("hash")) },
		"hash": func() (channelKeys, error) { return deriveKeys([]byte("dh"), nil, []byte("hash2")) },
		"dh":   func() (channelKeys, error) { return deriveKeys([]byte("dh2"), nil, []byte("hash")) },
	} {
		got, err := k()
		if err != nil {
			t.Fatal(err)
		}
		if got.workerToHub == base.workerToHub || got.hubToWorker == base.hubToWorker {
			t.Errorf("changing %s leaves a key unchanged", name)
		}
	}
	if base.workerToHub == base.hubToWorker {
		t.Error("the two directions share a key")
	}
	if _, err := deriveKeys(nil, nil, []byte("h")); err == nil {
		t.Error("empty DH secret accepted")
	}
}

// ---------------------------------------------------------------- connect

func TestConnectHandshake(t *testing.T) {
	wc, hc, wk, hk := connectConfigs(t)
	o := runPair(t, wc, hc, nil)
	o.mustSucceed(t)

	if o.worker.TranscriptHash != o.hub.TranscriptHash {
		t.Error("the two sides computed different transcript hashes")
	}
	if !bytes.Equal(o.worker.HubPub, hk.Public()) || !bytes.Equal(o.hub.WorkerPub, wk.Public()) {
		t.Error("peer keys not reported")
	}
	if o.worker.Version != proto.Current || o.hub.Version != proto.Current {
		t.Errorf("versions %s / %s", o.worker.Version, o.hub.Version)
	}
	if o.worker.PinChanged || o.worker.Pin.Seq != 0 || !bytes.Equal(o.worker.Pin.Pub, hk.Public()) {
		t.Errorf("unchanged pin reported as %+v changed=%v", o.worker.Pin, o.worker.PinChanged)
	}
	if h := o.hub.Worker; h.Name != "lab-pi" || h.Mode != ModeConnect || len(h.Ops) != 1 || h.Ops[0] != "snapshot" || h.PairingID != "" {
		t.Errorf("hub saw hello %+v", h)
	}
	// Frame order: hello, then sig (Worker); hello, then sig (Hub).
	if got := frameTypes(t, o.wWire); strings.Join(got, ",") != "hello,sig" {
		t.Errorf("worker frames %v", got)
	}
	if got := frameTypes(t, o.hWire); strings.Join(got, ",") != "hello,sig" {
		t.Errorf("hub frames %v", got)
	}
	o.roundTrip(t)
}

func TestHelloShapeAndTranscriptBytes(t *testing.T) {
	wc, hc, _, hk := connectConfigs(t)
	o := runPair(t, wc, hc, nil)
	o.mustSucceed(t)
	defer o.worker.Channel.Close()
	defer o.hub.Channel.Close()

	wBody, err := proto.ReadFrame(bytes.NewReader(o.wWire), proto.HandshakeFrameLimit)
	if err != nil {
		t.Fatal(err)
	}
	hBody, err := proto.ReadFrame(bytes.NewReader(o.hWire), proto.HandshakeFrameLimit)
	if err != nil {
		t.Fatal(err)
	}
	if got := TranscriptHash(wBody, hBody); got != o.worker.TranscriptHash {
		t.Error("transcript hash is not over the raw hello bytes as sent")
	}
	var wm, hm map[string]json.RawMessage
	if err := json.Unmarshal(wBody, &wm); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(hBody, &hm); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"type", "proto_min", "proto_max", "ops", "worker_pub", "eph_w", "nonce", "mode", "name"} {
		if _, ok := wm[f]; !ok {
			t.Errorf("worker hello lacks %q", f)
		}
	}
	if _, ok := wm["pairing_id"]; ok {
		t.Error("pairing_id present outside pair_code mode")
	}
	for _, f := range []string{"type", "version", "hub_pub", "eph_h", "nonce"} {
		if _, ok := hm[f]; !ok {
			t.Errorf("hub hello lacks %q", f)
		}
	}
	if _, ok := hm["chain"]; ok {
		t.Error("empty chain must be omitted")
	}
	var hub HubHello
	_ = json.Unmarshal(hBody, &hub)
	if !bytes.Equal(hub.HubPub, hk.Public()) || len(hub.Nonce) != NonceSize || len(hub.EphH) != 32 {
		t.Errorf("hub hello fields: %+v", hub)
	}
	// The Hub signature is in its own frame and is not part of the transcript.
	if bytes.Contains(hBody, []byte(`"sig"`)) {
		t.Error("signature inside the hub hello")
	}
}

func TestConnectWrongPinSendsNoWorkerSignature(t *testing.T) {
	wc, hc, _, _ := connectConfigs(t)
	wc.Pin = identity.Pin{Pub: mustKey(t).Public()} // pins some other key
	o := runPair(t, wc, hc, nil)
	if !errors.Is(o.workerErr, ErrPinMismatch) {
		t.Fatalf("worker error %v, want ErrPinMismatch", o.workerErr)
	}
	if o.hubErr == nil || o.worker != nil || o.hub != nil {
		t.Fatalf("hub must fail too: %v", o.hubErr)
	}
	if got := frameTypes(t, o.wWire); strings.Join(got, ",") != "hello" {
		t.Errorf("worker sent %v, want only its hello", got)
	}
	if ErrorCode(o.workerErr) != proto.CodeUnauthenticated {
		t.Errorf("code %s", ErrorCode(o.workerErr))
	}
}

// rotate builds n master-key rotations: the roots A, B, C... and the signed
// transitions between them, plus a certified Hub for every root.
func rotate(t *testing.T, n int) (keys []*identity.Key, chain []identity.Transition, hubs []*tHub) {
	t.Helper()
	first := newHub(t)
	keys, hubs = []*identity.Key{first.root}, []*tHub{first}
	for i := 1; i <= n; i++ {
		next := newHub(t)
		tr, err := identity.NewTransition(keys[i-1], next.root.Public(), uint64(i), time.Now())
		if err != nil {
			t.Fatal(err)
		}
		keys = append(keys, next.root)
		hubs = append(hubs, next)
		chain = append(chain, tr)
	}
	return keys, chain, hubs
}

func TestConnectAppliesTransitionChain(t *testing.T) {
	keys, chain, hubs := rotate(t, 3) // A -> B -> C -> D
	cases := []struct {
		name    string
		pin     identity.Pin
		wantSeq uint64
		changed bool
	}{
		{"pin at the first key, position unknown", identity.Pin{Pub: keys[0].Public()}, 3, true},
		{"pin at the first key, seq 0 explicit", identity.Pin{Pub: keys[0].Public(), Seq: 0}, 3, true},
		{"pin in the middle skips older links", identity.Pin{Pub: keys[2].Public(), Seq: 2}, 3, true},
		{"pin already current", identity.Pin{Pub: keys[3].Public(), Seq: 3}, 3, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			wc := WorkerConfig{Key: mustKey(t), Name: "w", Mode: ModeConnect, Pin: c.pin}
			hc := hubs[3].cfg()
			hc.Chain = chain
			o := runPair(t, wc, hc, nil)
			o.mustSucceed(t)
			if !bytes.Equal(o.worker.Pin.Pub, keys[3].Public()) || o.worker.Pin.Seq != c.wantSeq || o.worker.PinChanged != c.changed {
				t.Errorf("pin %+v changed=%v, want seq %d changed=%v", o.worker.Pin, o.worker.PinChanged, c.wantSeq, c.changed)
			}
			o.roundTrip(t)
		})
	}
}

func TestConnectRejectsBadChains(t *testing.T) {
	keys, chain, hubs := rotate(t, 2) // A -> B -> C
	forged := bytes.Clone(chain[1].SigByOld)
	forged[0] ^= 1
	badSig := []identity.Transition{chain[0], {Seq: 2, OldPub: chain[1].OldPub, NewPub: chain[1].NewPub, CreatedAt: chain[1].CreatedAt, SigByOld: forged}}
	other := newHub(t)
	cases := []struct {
		name string
		hub  *tHub
		pin  identity.Pin
		ch   []identity.Transition
		want error
	}{
		{"link with a bad signature", hubs[2], identity.Pin{Pub: keys[0].Public()}, badSig, ErrChainInvalid},
		{"gap: first link missing", hubs[2], identity.Pin{Pub: keys[0].Public()}, chain[1:], ErrPinMismatch},
		{"chain ends at another key", other, identity.Pin{Pub: keys[0].Public()}, chain, ErrPinMismatch},
		{"stale seq (rollback)", hubs[1], identity.Pin{Pub: keys[0].Public(), Seq: 5}, chain[:1], ErrChainInvalid},
		{"no chain, different key", hubs[1], identity.Pin{Pub: keys[0].Public()}, nil, ErrPinMismatch},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			wc := WorkerConfig{Key: mustKey(t), Name: "w", Mode: ModeConnect, Pin: c.pin}
			hc := c.hub.cfg()
			hc.Chain = c.ch
			o := runPair(t, wc, hc, nil)
			if !errors.Is(o.workerErr, c.want) {
				t.Fatalf("worker error %v, want %v", o.workerErr, c.want)
			}
			if got := frameTypes(t, o.wWire); strings.Join(got, ",") != "hello" {
				t.Errorf("worker sent %v after a rejected chain", got)
			}
		})
	}
}

// ----------------------------------------------------------- enroll_invite

func enrollConfigs(t *testing.T) (WorkerConfig, HubConfig, *identity.Key) {
	t.Helper()
	hub := newHub(t)
	hk := hub.root
	return WorkerConfig{Key: mustKey(t), Name: "new-worker", Mode: ModeEnrollInvite, HubFingerprint: hk.Fingerprint()},
		hub.cfg(), hk
}

func TestEnrollInviteHandshake(t *testing.T) {
	wc, hc, hk := enrollConfigs(t)
	o := runPair(t, wc, hc, nil)
	o.mustSucceed(t)
	if !o.worker.PinChanged || o.worker.Pin.Seq != 0 || !bytes.Equal(o.worker.Pin.Pub, hk.Public()) {
		t.Errorf("new pin %+v changed=%v", o.worker.Pin, o.worker.PinChanged)
	}
	if o.hub.Worker.Mode != ModeEnrollInvite || o.hub.Worker.Name != "new-worker" {
		t.Errorf("hub saw %+v", o.hub.Worker)
	}
	if got := frameTypes(t, o.wWire); strings.Join(got, ",") != "hello,sig" {
		t.Errorf("worker frames %v", got)
	}
	o.roundTrip(t)
}

func TestEnrollInviteWrongFingerprintSendsNoWorkerSignature(t *testing.T) {
	wc, hc, _ := enrollConfigs(t)
	wc.HubFingerprint = mustKey(t).Fingerprint()
	o := runPair(t, wc, hc, nil)
	if !errors.Is(o.workerErr, ErrFingerprintMismatch) {
		t.Fatalf("worker error %v", o.workerErr)
	}
	if got := frameTypes(t, o.wWire); strings.Join(got, ",") != "hello" {
		t.Errorf("worker sent %v, want only its hello", got)
	}
}

// The relay injects a transition chain into the Hub hello, whatever the mode,
// to test that the Worker refuses it outside connect mode.
func TestChainInHelloRejectedOutsideConnectMode(t *testing.T) {
	keys, chain, hubs := rotate(t, 1)
	for _, mode := range []Mode{ModeEnrollInvite, ModePairCode} {
		t.Run(string(mode), func(t *testing.T) {
			wc := WorkerConfig{Key: mustKey(t), Name: "w", Mode: mode, HubFingerprint: keys[1].Fingerprint()}
			if mode == ModePairCode {
				wc.PairingCode = identity.NewPairingCode()
			}
			// Inject the chain into the Hub hello on its way to the Worker.
			inject := func(fromWorker bool, idx int, body []byte) []byte {
				if fromWorker || idx != 0 {
					return body
				}
				var m map[string]json.RawMessage
				if err := json.Unmarshal(body, &m); err != nil {
					t.Fatal(err)
				}
				m["chain"], _ = json.Marshal(chain)
				out, _ := json.Marshal(m)
				return out
			}
			hc := hubs[1].cfg()
			hc.PairingPassword = func(string) ([]byte, error) { return wc.PairingCode.Password(), nil }
			o := runPair(t, wc, hc, inject)
			if !errors.Is(o.workerErr, ErrUnexpectedChain) {
				t.Fatalf("worker error %v, want ErrUnexpectedChain", o.workerErr)
			}
			if got := frameTypes(t, o.wWire); strings.Join(got, ",") != "hello" {
				t.Errorf("worker sent %v", got)
			}
		})
	}
}

func TestHubSendsChainOnlyInConnectMode(t *testing.T) {
	keys, chain, hubs := rotate(t, 1)
	wc, _, _ := enrollConfigs(t)
	hc := hubs[1].cfg()
	hc.Chain = chain
	wc.HubFingerprint = keys[1].Fingerprint()
	o := runPair(t, wc, hc, nil)
	o.mustSucceed(t)
	defer o.worker.Channel.Close()
	defer o.hub.Channel.Close()
	body, _ := proto.ReadFrame(bytes.NewReader(o.hWire), proto.HandshakeFrameLimit)
	if bytes.Contains(body, []byte("chain")) {
		t.Errorf("chain sent in enroll_invite mode: %s", body)
	}
}

func TestInvitationSecretOnlyTravelsInsideTheChannel(t *testing.T) {
	wc, hc, hk := enrollConfigs(t)
	invite, err := identity.NewInviteKey(hk.Fingerprint())
	if err != nil {
		t.Fatal(err)
	}
	secret := []byte(invite.Secret)

	a, b := net.Pipe()
	wrec, hrec := &recConn{Conn: a}, &recConn{Conn: b}
	wc.Timeout, hc.Timeout = 5*time.Second, 5*time.Second
	var o outcome
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); o.worker, o.workerErr = WorkerHandshake(wrec, wc) }()
	go func() { defer wg.Done(); o.hub, o.hubErr = HubHandshake(hrec, hc) }()
	wg.Wait()
	o.mustSucceed(t)
	defer o.worker.Channel.Close()
	defer o.hub.Channel.Close()

	// Nothing written so far (the handshake) holds the secret: the handshake
	// has no field for it.
	if bytes.Contains(wrec.written(), secret) || bytes.Contains(hrec.written(), secret) {
		t.Fatal("invitation secret in the handshake frames")
	}
	// Inside the channel it arrives intact but never in clear on the wire.
	errc := make(chan error, 1)
	go func() { _, err := o.worker.Channel.Write(secret); errc <- err }()
	got := make([]byte, len(secret))
	if _, err := io.ReadFull(o.hub.Channel, got); err != nil || !bytes.Equal(got, secret) {
		t.Fatalf("secret did not arrive: %v", err)
	}
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(wrec.written(), secret) || bytes.Contains(hrec.written(), secret) {
		t.Fatal("invitation secret visible on the wire after the channel was established")
	}
}

// -------------------------------------------------------------- pair_code

func pairConfigs(t *testing.T) (WorkerConfig, HubConfig, identity.PairingCode) {
	t.Helper()
	code := identity.NewPairingCode()
	wc := WorkerConfig{Key: mustKey(t), Name: "paired-pi", Mode: ModePairCode, PairingCode: code}
	hc := newHub(t).cfg()
	hc.PairingPassword = func(id string) ([]byte, error) {
		if id != code.PairingID() {
			return nil, proto.NewError(proto.CodePairingCodeInvalid, "unknown pairing id")
		}
		return code.Password(), nil
	}
	return wc, hc, code
}

func TestPairCodeHandshake(t *testing.T) {
	wc, hc, code := pairConfigs(t)
	var seenID string
	hc.Admit = func(h WorkerHello) error { seenID = h.PairingID; return nil }
	o := runPair(t, wc, hc, nil)
	o.mustSucceed(t)
	if seenID != code.PairingID() || o.hub.Worker.PairingID != code.PairingID() || o.hub.Worker.Mode != ModePairCode {
		t.Errorf("pairing id %q / %+v", seenID, o.hub.Worker)
	}
	if !bytes.Equal(o.worker.Pin.Pub, hc.Cert.RootPub) || !o.worker.PinChanged {
		t.Error("pin not taken from the paired Hub")
	}
	// Frame order: hellos, pake.msg W, pake.msg H, pake.confirm W, pake.confirm H, sigs.
	if got := strings.Join(frameTypes(t, o.wWire), ","); got != "hello,pake.msg,pake.confirm,sig" {
		t.Errorf("worker frames %s", got)
	}
	if got := strings.Join(frameTypes(t, o.hWire), ","); got != "hello,pake.msg,pake.confirm,sig" {
		t.Errorf("hub frames %s", got)
	}
	o.roundTrip(t)
}

func TestPairCodeConfirmationIsSignedOverHashAndTag(t *testing.T) {
	wc, hc, _ := pairConfigs(t)
	o := runPair(t, wc, hc, nil)
	o.mustSucceed(t)
	defer o.worker.Channel.Close()
	defer o.hub.Channel.Close()
	for _, side := range []struct {
		wire []byte
		pub  ed25519.PublicKey
	}{{o.wWire, wc.Key.Public()}, {o.hWire, hc.Cert.OpPub}} {
		r := bytes.NewReader(side.wire)
		for r.Len() > 0 {
			body, _ := proto.ReadFrame(r, proto.HandshakeFrameLimit)
			if ty, _ := proto.FrameType(body); ty != proto.TypePakeConfirm {
				continue
			}
			var f pakeConfirmFrame
			if err := json.Unmarshal(body, &f); err != nil {
				t.Fatal(err)
			}
			if err := identity.Verify(side.pub, identity.PurposePAKEConfirm, append(o.worker.TranscriptHash[:], f.Tag...), f.Sig); err != nil {
				t.Errorf("pake-confirm is not a signature over transcript_hash || tag: %v", err)
			}
			if identity.Verify(side.pub, identity.PurposeHandshakeHub, append(o.worker.TranscriptHash[:], f.Tag...), f.Sig) == nil {
				t.Error("signature valid under another purpose")
			}
		}
	}
}

func TestPairCodeWrongCodeDerivesNoKeys(t *testing.T) {
	wc, hc, _ := pairConfigs(t)
	wrong := identity.NewPairingCode()
	// The Hub finds the right id but holds a different password.
	hc.PairingPassword = func(string) ([]byte, error) { return wrong.Password(), nil }
	o := runPair(t, wc, hc, nil)
	if o.worker != nil || o.hub != nil {
		t.Fatal("a channel was created despite the wrong code")
	}
	if !errors.Is(o.hubErr, ErrPairingConfirm) {
		t.Errorf("hub error %v, want ErrPairingConfirm", o.hubErr)
	}
	if proto.CodeOf(o.workerErr) != proto.CodePairingCodeInvalid {
		t.Errorf("worker error %v, want pairing_code_invalid", o.workerErr)
	}
	// Neither side revealed a signature or a confirmation after the failure.
	if got := strings.Join(frameTypes(t, o.wWire), ","); got != "hello,pake.msg,pake.confirm" {
		t.Errorf("worker frames %s", got)
	}
	if got := strings.Join(frameTypes(t, o.hWire), ","); got != "hello,pake.msg,error" {
		t.Errorf("hub frames %s", got)
	}
}

func TestPairCodeUnknownPairingIDAndMissingLookup(t *testing.T) {
	wc, hc, _ := pairConfigs(t)
	hc.PairingPassword = func(string) ([]byte, error) { return nil, proto.NewError(proto.CodePairingIDCollision, "x") }
	o := runPair(t, wc, hc, nil)
	if proto.CodeOf(o.workerErr) != proto.CodePairingIDCollision || proto.CodeOf(o.hubErr) != proto.CodePairingIDCollision {
		t.Errorf("worker %v hub %v", o.workerErr, o.hubErr)
	}
	hc.PairingPassword = nil
	o = runPair(t, wc, hc, nil)
	if proto.CodeOf(o.workerErr) != proto.CodePairingCodeInvalid || o.hubErr == nil {
		t.Errorf("worker %v hub %v", o.workerErr, o.hubErr)
	}
	if got := strings.Join(frameTypes(t, o.hWire), ","); got != "error" {
		t.Errorf("hub frames %s", got)
	}
}

func TestPairCodeImpostorHubWithoutCode(t *testing.T) {
	// A Hub that does not know the code (here: it guesses) never gets the
	// Worker to sign anything.
	wc, hc, _ := pairConfigs(t)
	guess := identity.NewPairingCode()
	hc.PairingPassword = func(string) ([]byte, error) { return guess.Password(), nil }
	o := runPair(t, wc, hc, nil)
	if o.worker != nil {
		t.Fatal("worker accepted an impostor")
	}
	for _, ty := range frameTypes(t, o.wWire) {
		if ty == proto.TypeSig {
			t.Error("worker signed for an impostor")
		}
	}
}

// ---------------------------------------------------- tampering and errors

func flipField(t *testing.T, body []byte, field string) []byte {
	t.Helper()
	key := []byte(`"` + field + `":"`)
	i := bytes.Index(body, key)
	if i < 0 {
		t.Fatalf("no field %q in %s", field, body)
	}
	i += len(key) + 3
	out := bytes.Clone(body)
	if out[i] == 'A' {
		out[i] = 'B'
	} else {
		out[i] = 'A'
	}
	return out
}

func TestTamperedHelloByteIsDetected(t *testing.T) {
	for _, tc := range []struct {
		name       string
		fromWorker bool
		field      string
	}{
		{"worker hello nonce", true, "nonce"},
		{"worker hello ephemeral key", true, "eph_w"},
		{"hub hello nonce", false, "nonce"},
		{"hub hello ephemeral key", false, "eph_h"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wc, hc, _, _ := connectConfigs(t)
			o := runPair(t, wc, hc, func(fromWorker bool, idx int, body []byte) []byte {
				if fromWorker == tc.fromWorker && idx == 0 {
					return flipField(t, body, tc.field)
				}
				return body
			})
			if o.worker != nil || o.hub != nil {
				t.Fatal("a handshake over tampered hellos completed")
			}
			if !errors.Is(o.workerErr, ErrBadSignature) {
				t.Errorf("worker error %v, want ErrBadSignature", o.workerErr)
			}
			for _, ty := range frameTypes(t, o.wWire) {
				if ty == proto.TypeSig {
					t.Error("worker signed over a tampered transcript")
				}
			}
		})
	}
}

func TestTamperedHelloNameIsDetected(t *testing.T) {
	wc, hc, _, _ := connectConfigs(t)
	o := runPair(t, wc, hc, func(fromWorker bool, idx int, body []byte) []byte {
		if fromWorker && idx == 0 {
			return bytes.Replace(body, []byte("lab-pi"), []byte("lab-p1"), 1)
		}
		return body
	})
	if o.worker != nil || o.hub != nil || !errors.Is(o.workerErr, ErrBadSignature) {
		t.Fatalf("worker %v hub %v", o.workerErr, o.hubErr)
	}
}

func TestTamperedSignaturesAreRejected(t *testing.T) {
	// Frame 1 of the Hub is sig_hub, frame 1 of the Worker is sig_worker.
	for _, fromWorker := range []bool{false, true} {
		wc, hc, _, _ := connectConfigs(t)
		o := runPair(t, wc, hc, func(fw bool, idx int, body []byte) []byte {
			if fw == fromWorker && idx == 1 {
				return flipField(t, body, "sig")
			}
			return body
		})
		if o.hub != nil && o.worker != nil {
			t.Fatalf("fromWorker=%v: handshake completed with a bad signature", fromWorker)
		}
		if fromWorker && !errors.Is(o.hubErr, ErrBadSignature) {
			t.Errorf("hub error %v", o.hubErr)
		}
		if !fromWorker && !errors.Is(o.workerErr, ErrBadSignature) {
			t.Errorf("worker error %v", o.workerErr)
		}
	}
}

func TestSignatureOfAnotherPurposeIsRejected(t *testing.T) {
	// A hub sig_hub replayed as a sig_worker (same key, other purpose) fails.
	hk := mustKey(t)
	hash := TranscriptHash([]byte("w"), []byte("h"))
	sig, _ := hk.Sign(identity.PurposeHandshakeHub, hash[:])
	if identity.Verify(hk.Public(), identity.PurposeHandshakeWorker, hash[:], sig) == nil {
		t.Fatal("purposes are not separated")
	}
}

func TestVersionNegotiation(t *testing.T) {
	t.Run("hub one minor ahead still serves a 3.0 worker", func(t *testing.T) {
		wc, hc, _, _ := connectConfigs(t)
		hc.Current = proto.Version{Major: 3, Minor: 1}
		o := runPair(t, wc, hc, nil)
		o.mustSucceed(t)
		defer o.worker.Channel.Close()
		defer o.hub.Channel.Close()
		if o.worker.Version != (proto.Version{Major: 3}) || o.hub.Version != o.worker.Version {
			t.Errorf("version %s / %s", o.worker.Version, o.hub.Version)
		}
	})
	t.Run("worker range wins the highest common version", func(t *testing.T) {
		wc, hc, _, _ := connectConfigs(t)
		wc.Protocols = proto.Range{Min: proto.Version{Major: 3}, Max: proto.Version{Major: 3, Minor: 5}}
		hc.Current = proto.Version{Major: 3, Minor: 2}
		o := runPair(t, wc, hc, nil)
		o.mustSucceed(t)
		defer o.worker.Channel.Close()
		defer o.hub.Channel.Close()
		if o.worker.Version != (proto.Version{Major: 3, Minor: 2}) {
			t.Errorf("version %s", o.worker.Version)
		}
	})
	t.Run("major mismatch", func(t *testing.T) {
		wc, hc, _, _ := connectConfigs(t)
		hc.Current = proto.Version{Major: 4}
		o := runPair(t, wc, hc, nil)
		if proto.CodeOf(o.workerErr) != proto.CodeProtoIncompatible || proto.CodeOf(o.hubErr) != proto.CodeProtoIncompatible {
			t.Errorf("worker %v hub %v", o.workerErr, o.hubErr)
		}
		if o.worker != nil || o.hub != nil {
			t.Error("channel despite incompatible versions")
		}
	})
	t.Run("same major, disjoint minor range", func(t *testing.T) {
		wc, hc, _, _ := connectConfigs(t)
		wc.Protocols = proto.Range{Min: proto.Version{Major: 3, Minor: 5}, Max: proto.Version{Major: 3, Minor: 6}}
		o := runPair(t, wc, hc, nil)
		if proto.CodeOf(o.workerErr) != proto.CodeProtoIncompatible {
			t.Errorf("worker %v", o.workerErr)
		}
	})
	t.Run("a 1.x Hub is refused (protocol 3.0 only, no fallback)", func(t *testing.T) {
		wc, hc, _, _ := connectConfigs(t)
		hc.Current = proto.Version{Major: 1, Minor: 0}
		o := runPair(t, wc, hc, nil)
		if proto.CodeOf(o.workerErr) != proto.CodeProtoIncompatible || proto.CodeOf(o.hubErr) != proto.CodeProtoIncompatible {
			t.Errorf("worker %v hub %v", o.workerErr, o.hubErr)
		}
		if o.worker != nil || o.hub != nil {
			t.Error("channel despite a 1.x Hub")
		}
	})
}

func TestAdmitHookCanRefuse(t *testing.T) {
	wc, hc, _, _ := connectConfigs(t)
	var seen WorkerHello
	hc.Admit = func(h WorkerHello) error {
		seen = h
		return proto.NewError(proto.CodeRateLimited, "slow down")
	}
	o := runPair(t, wc, hc, nil)
	if proto.CodeOf(o.workerErr) != proto.CodeRateLimited || proto.CodeOf(o.hubErr) != proto.CodeRateLimited {
		t.Errorf("worker %v hub %v", o.workerErr, o.hubErr)
	}
	if seen.Name != "lab-pi" || !bytes.Equal(seen.WorkerPub, wc.Key.Public()) {
		t.Errorf("hook saw %+v", seen)
	}
	if got := strings.Join(frameTypes(t, o.hWire), ","); got != "error" {
		t.Errorf("hub sent %s before the Admit verdict", got)
	}

	// A non-proto error is reported to the Worker as a bare internal error.
	hc.Admit = func(WorkerHello) error { return errors.New("database password is hunter2") }
	o = runPair(t, wc, hc, nil)
	if proto.CodeOf(o.workerErr) != proto.CodeInternal || strings.Contains(o.workerErr.Error(), "hunter2") {
		t.Errorf("worker error leaks or has wrong code: %v", o.workerErr)
	}
}

func TestNotEnrolledFrameIsSurfaced(t *testing.T) {
	// not_enrolled before authentication is only information (AD-6 answers it
	// inside the channel); the Worker reports it and keeps its state.
	a, b := net.Pipe()
	go func() {
		_, _ = proto.ReadFrame(b, proto.HandshakeFrameLimit)
		_ = proto.WriteFrame(b, proto.HandshakeFrameLimit, map[string]string{"type": proto.TypeNotEnrolled})
	}()
	wc, _, _, _ := connectConfigs(t)
	_, err := WorkerHandshake(a, wc)
	if proto.CodeOf(err) != proto.CodeNotEnrolled {
		t.Fatalf("got %v", err)
	}
}

func TestMalformedPeerFrames(t *testing.T) {
	wc, _, _, _ := connectConfigs(t)
	cases := map[string]func(net.Conn){
		"wrong frame type": func(c net.Conn) {
			_ = proto.WriteFrame(c, 1<<16, map[string]string{"type": "pake.msg"})
		},
		"short key": func(c net.Conn) {
			_ = proto.WriteFrame(c, 1<<16, map[string]any{"type": "hello", "version": "1.0", "hub_pub": "AAAA", "eph_h": "AAAA", "nonce": "AAAA"})
		},
		"oversized frame": func(c net.Conn) {
			_, _ = c.Write([]byte{0, 1, 0, 1})
		},
		"not json": func(c net.Conn) {
			_, _ = c.Write([]byte{0, 0, 0, 3, 'a', 'b', 'c'})
		},
		"version outside the worker range": func(c net.Conn) {
			hk := mustKey(t)
			hello := map[string]any{"type": "hello", "version": "9.9", "hub_pub": identity.B64(hk.Public()), "eph_h": identity.B64(make([]byte, 32)), "nonce": identity.B64(make([]byte, 32))}
			_ = proto.WriteFrame(c, 1<<16, hello)
		},
	}
	for name, reply := range cases {
		t.Run(name, func(t *testing.T) {
			a, b := net.Pipe()
			go func() {
				_, _ = proto.ReadFrame(b, proto.HandshakeFrameLimit)
				reply(b)
				_, _ = io.Copy(io.Discard, b)
			}()
			cfg := wc
			cfg.Timeout = 2 * time.Second
			if _, err := WorkerHandshake(a, cfg); err == nil {
				t.Fatal("handshake succeeded")
			}
			_ = b.Close()
		})
	}
}

func TestHubRejectsBadHellos(t *testing.T) {
	wc, _, _, _ := connectConfigs(t)
	good := func() map[string]any {
		return map[string]any{
			"type": "hello", "proto_min": "1.0", "proto_max": "1.0", "ops": []string{},
			"worker_pub": identity.B64(wc.Key.Public()), "eph_w": identity.B64(make([]byte, 32)),
			"nonce": identity.B64(make([]byte, 32)), "mode": "connect", "name": "x",
		}
	}
	mods := map[string]func(map[string]any){
		"unknown mode":          func(m map[string]any) { m["mode"] = "teleport" },
		"short worker_pub":      func(m map[string]any) { m["worker_pub"] = "AAAA" },
		"short nonce":           func(m map[string]any) { m["nonce"] = "AAAA" },
		"pairing id in connect": func(m map[string]any) { m["pairing_id"] = "ABCD" },
		"pair_code without id":  func(m map[string]any) { m["mode"] = "pair_code" },
		"overlong name":         func(m map[string]any) { m["name"] = strings.Repeat("n", 300) },
		"empty range":           func(m map[string]any) { m["proto_min"] = "1.3"; m["proto_max"] = "1.2" },
	}
	for name, mod := range mods {
		t.Run(name, func(t *testing.T) {
			a, b := net.Pipe()
			m := good()
			mod(m)
			go func() {
				_ = proto.WriteFrame(b, 1<<16, m)
				_, _ = io.Copy(io.Discard, b)
			}()
			_, err := HubHandshake(a, HubConfig{Key: mustKey(t), Timeout: 2 * time.Second})
			if err == nil {
				t.Fatal("hub accepted the hello")
			}
			_ = b.Close()
		})
	}
}

func TestHandshakeTimeoutClosesConn(t *testing.T) {
	a, b := net.Pipe()
	go func() { _, _ = io.Copy(io.Discard, b) }() // swallows the hello, never answers
	wc, _, _, _ := connectConfigs(t)
	wc.Timeout = 50 * time.Millisecond
	start := time.Now()
	_, err := WorkerHandshake(a, wc)
	if err == nil || time.Since(start) > 3*time.Second {
		t.Fatalf("got %v after %s", err, time.Since(start))
	}
	var ne net.Error
	if !errors.As(err, &ne) || !ne.Timeout() {
		t.Errorf("want a timeout, got %v", err)
	}
	if _, err := a.Write([]byte("x")); err == nil {
		t.Error("connection still open after a failed handshake")
	}
}

func TestHandshakeClearsDeadline(t *testing.T) {
	wc, hc, _, _ := connectConfigs(t)
	wc.Timeout, hc.Timeout = 200*time.Millisecond, 200*time.Millisecond
	o := runPair(t, wc, hc, nil)
	o.mustSucceed(t)
	defer o.worker.Channel.Close()
	defer o.hub.Channel.Close()
	time.Sleep(400 * time.Millisecond) // past the handshake deadline
	o.roundTrip(t)
}

func TestConfigValidation(t *testing.T) {
	k := mustKey(t)
	for name, cfg := range map[string]WorkerConfig{
		"no key":              {Mode: ModeConnect, Pin: identity.Pin{Pub: k.Public()}},
		"connect without pin": {Key: k, Mode: ModeConnect},
		"enroll without fp":   {Key: k, Mode: ModeEnrollInvite},
		"pair without code":   {Key: k, Mode: ModePairCode},
		"unknown mode":        {Key: k, Mode: "x"},
		"empty range":         {Key: k, Mode: ModeConnect, Pin: identity.Pin{Pub: k.Public()}, Protocols: proto.Range{Min: proto.Version{Major: 1, Minor: 2}, Max: proto.Version{Major: 1}}},
		"long name":           {Key: k, Mode: ModeConnect, Pin: identity.Pin{Pub: k.Public()}, Name: strings.Repeat("n", 256)},
	} {
		a, b := net.Pipe()
		_, err := WorkerHandshake(a, cfg)
		if !errors.Is(err, ErrConfig) && !errors.Is(err, proto.ErrInvalidArgument) {
			t.Errorf("%s: got %v", name, err)
		}
		_ = b.Close()
	}
	a, b := net.Pipe()
	if _, err := HubHandshake(a, HubConfig{}); !errors.Is(err, ErrConfig) {
		t.Errorf("hub without key: %v", err)
	}
	_ = b.Close()
}

func TestErrorCodeMapping(t *testing.T) {
	for err, want := range map[error]proto.Code{
		ErrPinMismatch:                        proto.CodeUnauthenticated,
		ErrFingerprintMismatch:                proto.CodeUnauthenticated,
		ErrChainInvalid:                       proto.CodeUnauthenticated,
		ErrBadSignature:                       proto.CodeUnauthenticated,
		ErrPairingConfirm:                     proto.CodePairingCodeInvalid,
		ErrProtocol:                           proto.CodeInvalidArgument,
		ErrUnexpectedChain:                    proto.CodeInvalidArgument,
		ErrConfig:                             proto.CodeInvalidArgument,
		io.EOF:                                proto.CodeInternal,
		proto.NewError(proto.CodeRevoked, ""): proto.CodeRevoked,
	} {
		if got := ErrorCode(err); got != want {
			t.Errorf("%v: %s, want %s", err, got, want)
		}
	}
	if ErrorCode(nil) != "" {
		t.Error("nil error has a code")
	}
}

// ------------------------------------------- review round: extra evidence

// frameOfType returns the first frame of the given type in raw wire bytes.
func frameOfType(t *testing.T, wire []byte, ty string) []byte {
	t.Helper()
	r := bytes.NewReader(wire)
	for r.Len() > 0 {
		body, err := proto.ReadFrame(r, proto.HandshakeFrameLimit)
		if err != nil {
			t.Fatalf("wire is not handshake frames: %v", err)
		}
		if got, _ := proto.FrameType(body); got == ty {
			return body
		}
	}
	t.Fatalf("no %q frame on the wire", ty)
	return nil
}

func TestHandshakeSignaturePurposesOnTheWire(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(t *testing.T) (outcome, ed25519.PublicKey, ed25519.PublicKey)
	}{
		{"connect", func(t *testing.T) (outcome, ed25519.PublicKey, ed25519.PublicKey) {
			wc, hc, _, _ := connectConfigs(t)
			return runPair(t, wc, hc, nil), wc.Key.Public(), hc.Key.Public()
		}},
		{"pair_code", func(t *testing.T) (outcome, ed25519.PublicKey, ed25519.PublicKey) {
			wc, hc, _ := pairConfigs(t)
			return runPair(t, wc, hc, nil), wc.Key.Public(), hc.Key.Public()
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o, workerPub, hubPub := tc.run(t)
			o.mustSucceed(t)
			defer o.worker.Channel.Close()
			defer o.hub.Channel.Close()
			hash := o.worker.TranscriptHash[:]
			for _, side := range []struct {
				name        string
				wire        []byte
				pub         ed25519.PublicKey
				want, other identity.Purpose
			}{
				{"sig_hub", o.hWire, hubPub, identity.PurposeHandshakeHub, identity.PurposeHandshakeWorker},
				{"sig_worker", o.wWire, workerPub, identity.PurposeHandshakeWorker, identity.PurposeHandshakeHub},
			} {
				var f sigFrame
				if err := json.Unmarshal(frameOfType(t, side.wire, proto.TypeSig), &f); err != nil {
					t.Fatal(err)
				}
				if err := identity.Verify(side.pub, side.want, hash, f.Sig); err != nil {
					t.Errorf("%s does not verify under purpose %s: %v", side.name, side.want, err)
				}
				if identity.Verify(side.pub, side.other, hash, f.Sig) == nil {
					t.Errorf("%s also verifies under purpose %s", side.name, side.other)
				}
			}
		})
	}
}

// TestPairCodeWorkerChecksHubConfirmation covers the Worker-side failure
// branches of expectConfirm: the Hub's pake.confirm is the only thing that
// authenticates the Hub in pair_code mode.
func TestPairCodeWorkerChecksHubConfirmation(t *testing.T) {
	for _, tc := range []struct {
		field string
		want  error
	}{
		{"tag", ErrPairingConfirm},
		{"sig", ErrBadSignature},
	} {
		t.Run("hub confirm "+tc.field, func(t *testing.T) {
			wc, hc, _ := pairConfigs(t)
			o := runPair(t, wc, hc, func(fromWorker bool, idx int, body []byte) []byte {
				if !fromWorker && idx == 2 {
					return flipField(t, body, tc.field)
				}
				return body
			})
			if o.worker != nil || o.hub != nil {
				t.Fatal("a channel was created over a corrupted Hub confirmation")
			}
			if !errors.Is(o.workerErr, tc.want) {
				t.Errorf("worker error %v, want %v", o.workerErr, tc.want)
			}
			if got := strings.Join(frameTypes(t, o.wWire), ","); got != "hello,pake.msg,pake.confirm" {
				t.Errorf("worker frames %s: it must not sign before the Hub is authenticated", got)
			}
		})
	}
}

func TestPairCodeHubChecksWorkerConfirmationSignature(t *testing.T) {
	wc, hc, _ := pairConfigs(t)
	o := runPair(t, wc, hc, func(fromWorker bool, idx int, body []byte) []byte {
		if fromWorker && idx == 2 {
			return flipField(t, body, "sig")
		}
		return body
	})
	if o.worker != nil || o.hub != nil {
		t.Fatal("a channel was created over a corrupted Worker confirmation")
	}
	if !errors.Is(o.hubErr, ErrBadSignature) || errors.Is(o.hubErr, ErrPairingConfirm) {
		t.Errorf("hub error %v, want ErrBadSignature and not ErrPairingConfirm", o.hubErr)
	}
	for _, ty := range frameTypes(t, o.hWire) {
		if ty == proto.TypePakeConfirm || ty == proto.TypeSig {
			t.Errorf("hub sent a %s frame to an unauthenticated Worker", ty)
		}
	}
}

// TestPairCodeSessionIDIsTranscriptHash pins AC4: the CPace session id is
// the transcript hash. A tampered hello gives the two sides different hashes
// and so different CPace session ids, and the CPace confirmation itself must
// fail. With a constant session id the confirmation would pass and the
// failure would only show later at the signatures.
func TestPairCodeSessionIDIsTranscriptHash(t *testing.T) {
	for _, tc := range []struct {
		name       string
		fromWorker bool
	}{
		{"worker hello nonce", true},
		{"hub hello nonce", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wc, hc, _ := pairConfigs(t)
			o := runPair(t, wc, hc, func(fromWorker bool, idx int, body []byte) []byte {
				if fromWorker == tc.fromWorker && idx == 0 {
					return flipField(t, body, "nonce")
				}
				return body
			})
			if o.worker != nil || o.hub != nil {
				t.Fatal("a handshake over tampered hellos completed")
			}
			if !errors.Is(o.hubErr, ErrPairingConfirm) {
				t.Errorf("hub error %v, want ErrPairingConfirm (CPace sid must be transcript_hash)", o.hubErr)
			}
			for _, ty := range frameTypes(t, o.wWire) {
				if ty == proto.TypeSig {
					t.Error("worker sent sig_worker over a tampered transcript")
				}
			}
		})
	}
}

// TestPairCodeChannelKeysIncludeCPaceISK drives the real Worker against a
// scripted Hub that derives the channel keys on its own (X25519 secret,
// CPace ISK and transcript hash through HKDF written out in the test) and
// checks the records the resulting Channel emits and accepts. Dropping the
// ISK from the key schedule, or changing the labels or the salt, breaks it.
func TestPairCodeChannelKeysIncludeCPaceISK(t *testing.T) {
	code := identity.NewPairingCode()
	hub := newHub(t)
	hubKey := hub.op
	wc := WorkerConfig{Key: mustKey(t), Name: "scripted", Mode: ModePairCode, PairingCode: code, Timeout: 5 * time.Second}

	wEnd, hEnd := net.Pipe()
	defer hEnd.Close()
	type res struct {
		r   *WorkerResult
		err error
	}
	done := make(chan res, 1)
	go func() { r, err := WorkerHandshake(wEnd, wc); done <- res{r, err} }()

	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	workerBytes, err := expectFrame(hEnd, proto.TypeHello)
	must(err)
	var wh WorkerHello
	must(decode(workerBytes, &wh))
	eph, err := ecdh.X25519().GenerateKey(rand.Reader)
	must(err)
	hh := HubHello{
		Type: proto.TypeHello, Version: proto.Current, HubPub: identity.B64(hub.cert.RootPub), HubCert: hub.cert.Bytes(),
		EphH: eph.PublicKey().Bytes(), Nonce: bytes.Repeat([]byte{7}, NonceSize),
	}
	hubBytes, err := sendFrame(hEnd, hh)
	must(err)
	hash := TranscriptHash(workerBytes, hubBytes)

	body, err := expectFrame(hEnd, proto.TypePakeMsg)
	must(err)
	var msgA pakeMsgFrame
	must(decode(body, &msgA))
	session, msgB, err := pake.Respond(pake.Params{Password: code.Password(), SessionID: hash[:]}, msgA.Msg)
	must(err)
	_, err = sendFrame(hEnd, pakeMsgFrame{Type: proto.TypePakeMsg, Msg: msgB})
	must(err)
	body, err = expectFrame(hEnd, proto.TypePakeConfirm)
	must(err)
	var wconf pakeConfirmFrame
	must(decode(body, &wconf))
	must(session.VerifyConfirmation(wconf.Tag))
	must(sendConfirm(hEnd, session, hubKey, hash[:]))
	sig, err := hubKey.Sign(identity.PurposeHandshakeHub, hash[:])
	must(err)
	_, err = sendFrame(hEnd, sigFrame{Type: proto.TypeSig, Sig: sig})
	must(err)
	_, err = expectFrame(hEnd, proto.TypeSig)
	must(err)

	r := <-done
	must(r.err)
	defer r.r.Channel.Close()

	// Independent key schedule: HKDF-SHA256(salt = transcript_hash,
	// IKM = X25519 secret || CPace ISK), one label per direction.
	wpub, err := ecdh.X25519().NewPublicKey(wh.EphW)
	must(err)
	dh, err := eph.ECDH(wpub)
	must(err)
	prk, err := hkdf.Extract(sha256.New, append(bytes.Clone(dh), session.Key()...), hash[:])
	must(err)
	aead := func(info string) cipher.AEAD {
		k, err := hkdf.Expand(sha256.New, prk, info, 32)
		must(err)
		a, err := chacha20poly1305.New(k)
		must(err)
		return a
	}
	w2h, h2w := aead("minion-hs-v1 key worker-to-hub"), aead("minion-hs-v1 key hub-to-worker")

	// Hub to Worker: a record the scripted Hub seals is read by the Channel.
	msg := []byte("from the scripted hub")
	hdr := []byte{0, 0, 0, byte(len(msg) + 16)}
	rec := h2w.Seal(hdr, make([]byte, 12), msg, hdr)
	go func() { _, _ = hEnd.Write(rec) }()
	got := make([]byte, len(msg))
	if _, err := io.ReadFull(r.r.Channel, got); err != nil || !bytes.Equal(got, msg) {
		t.Fatalf("channel did not accept a record sealed with the independently derived key: %q, %v", got, err)
	}

	// Worker to Hub: a record the Channel emits opens under the derived key.
	out := []byte("from the worker")
	go func() { _, _ = r.r.Channel.Write(out) }()
	var h [4]byte
	_, err = io.ReadFull(hEnd, h[:])
	must(err)
	ct := make([]byte, binary.BigEndian.Uint32(h[:]))
	_, err = io.ReadFull(hEnd, ct)
	must(err)
	plain, err := w2h.Open(nil, make([]byte, 12), ct, h[:])
	if err != nil || !bytes.Equal(plain, out) {
		t.Fatalf("record emitted by the channel does not open under the independently derived key: %q, %v", plain, err)
	}
}

// TestChannelRunsYamux runs a real yamux session over two Channels (AC5).
func TestChannelRunsYamux(t *testing.T) {
	wc, hc, _, _ := connectConfigs(t)
	o := runPair(t, wc, hc, nil)
	o.mustSucceed(t)
	var _ net.Conn = o.worker.Channel
	cfg := yamux.DefaultConfig()
	cfg.LogOutput = io.Discard
	client, err := yamux.Client(o.worker.Channel, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	server, err := yamux.Server(o.hub.Channel, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	go func() {
		s, err := server.Accept()
		if err != nil {
			return
		}
		defer s.Close()
		_, _ = io.Copy(s, s)
	}()
	s, err := client.Open()
	if err != nil {
		t.Fatal(err)
	}
	msg := bytes.Repeat([]byte("yamux over aead "), 4096) // spans several records
	go func() { _, _ = s.Write(msg) }()
	got := make([]byte, len(msg))
	if _, err := io.ReadFull(s, got); err != nil || !bytes.Equal(got, msg) {
		t.Fatalf("echo over yamux: %v", err)
	}
}

// TestWorkerFeaturesTravelInTheSignedHello checks that the features a Worker
// announces reach the Hub from the hello (spine AM-13), that an omitted list
// stays omitted (an older Worker) and that a relay cannot add a feature: the
// transcript hash covers the hello bytes, so the Worker's signature no longer
// verifies and no handshake completes.
func TestWorkerFeaturesTravelInTheSignedHello(t *testing.T) {
	wc, hc, _, _ := connectConfigs(t)
	wc.Features = []string{proto.FeatureTextVerbatim, proto.FeatureAnswerReviewed, "Not A Feature", proto.FeatureAnswerReviewed}
	o := runPair(t, wc, hc, nil)
	o.mustSucceed(t)
	defer o.worker.Channel.Close()
	defer o.hub.Channel.Close()
	want := []string{proto.FeatureAnswerReviewed, proto.FeatureTextVerbatim}
	if got := o.hub.Worker.Features; !slices.Equal(got, want) {
		t.Errorf("hub saw features %v, want %v", got, want)
	}

	// A Worker that announces none omits the field, as an older Worker does.
	wc2, hc2, _, _ := connectConfigs(t)
	o2 := runPair(t, wc2, hc2, nil)
	o2.mustSucceed(t)
	defer o2.worker.Channel.Close()
	defer o2.hub.Channel.Close()
	wBody, err := proto.ReadFrame(bytes.NewReader(o2.wWire), proto.HandshakeFrameLimit)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(wBody, []byte(`"features"`)) || len(o2.hub.Worker.Features) != 0 {
		t.Errorf("a Worker without features sent %s", wBody)
	}

	// A relay that adds a feature to the hello is detected.
	wc3, hc3, _, _ := connectConfigs(t)
	o3 := runPair(t, wc3, hc3, func(fromWorker bool, idx int, body []byte) []byte {
		if fromWorker && idx == 0 {
			return bytes.Replace(body, []byte(`"ops":`), []byte(`"features":["`+proto.FeatureAnswerReviewed+`"],"ops":`), 1)
		}
		return body
	})
	if o3.worker != nil || o3.hub != nil {
		t.Fatal("a handshake over a hello with an added feature completed")
	}
	if !errors.Is(o3.workerErr, ErrBadSignature) {
		t.Errorf("worker error %v, want ErrBadSignature", o3.workerErr)
	}
}
