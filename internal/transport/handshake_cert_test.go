package transport

import (
	"bytes"
	"encoding/json"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// fakeWM is a WallMono whose two clocks move independently.
type fakeWM struct {
	mu   sync.Mutex
	wall time.Time
	mono time.Duration
}

func (f *fakeWM) Wall() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.wall
}

func (f *fakeWM) Mono() time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.mono
}

func (f *fakeWM) set(wall time.Time, mono time.Duration) {
	f.mu.Lock()
	f.wall, f.mono = wall, mono
	f.mu.Unlock()
}

// helloCapture records both hello bodies in a relay so a test can sign over
// the transcript the Worker computes.
type helloCapture struct {
	mu     sync.Mutex
	worker []byte
	hub    []byte
}

func (c *helloCapture) see(fromWorker bool, idx int, body []byte) {
	if idx != 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if fromWorker {
		c.worker = bytes.Clone(body)
	} else {
		c.hub = bytes.Clone(body)
	}
}

func (c *helloCapture) hash() [TranscriptHashSize]byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return TranscriptHash(c.worker, c.hub)
}

func mutateHubHello(t *testing.T, body []byte, f func(m map[string]json.RawMessage)) []byte {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatal(err)
	}
	f(m)
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// onlyHello fails the test unless the Worker wrote nothing but its hello: no
// sig_worker, no pin, nothing.
func onlyHello(t *testing.T, o outcome) {
	t.Helper()
	if got := strings.Join(frameTypes(t, o.wWire), ","); got != "hello" {
		t.Errorf("worker sent %q, want only its hello", got)
	}
}

func rawB64(t *testing.T, b []byte) json.RawMessage {
	t.Helper()
	out, err := json.Marshal(identity.B64(b))
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestHubHelloCarriesRootAndCertificate(t *testing.T) {
	wc, hc, _, hk := connectConfigs(t)
	o := runPair(t, wc, hc, nil)
	o.mustSucceed(t)
	defer o.worker.Channel.Close()
	defer o.hub.Channel.Close()
	body, err := proto.ReadFrame(bytes.NewReader(o.hWire), proto.HandshakeFrameLimit)
	if err != nil {
		t.Fatal(err)
	}
	var hub HubHello
	if err := json.Unmarshal(body, &hub); err != nil {
		t.Fatal(err)
	}
	cert, err := identity.ParseHubCert(hub.HubCert)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(hub.HubPub, hk.Public()) || !bytes.Equal(cert.RootPub, hk.Public()) || !bytes.Equal(cert.OpPub, hc.Key.Public()) {
		t.Error("hub_pub is not the root, or the certificate does not name the operational key")
	}
	if len(hub.HubCert) != identity.HubCertSize || !bytes.Equal(o.worker.OpPub, hc.Key.Public()) || o.worker.Cert.Serial != 1 {
		t.Errorf("result does not carry the validated certificate: %+v", o.worker.Cert)
	}
	// sig_hub verifies under the operational key and not under the root.
	r := bytes.NewReader(o.hWire)
	var sig []byte
	for r.Len() > 0 {
		b, _ := proto.ReadFrame(r, proto.HandshakeFrameLimit)
		if ty, _ := proto.FrameType(b); ty == proto.TypeSig {
			var f sigFrame
			_ = json.Unmarshal(b, &f)
			sig = f.Sig
		}
	}
	h := o.worker.TranscriptHash
	if identity.Verify(hc.Key.Public(), identity.PurposeHandshakeHub, h[:], sig) != nil {
		t.Error("sig_hub is not made by the operational key")
	}
	if identity.Verify(hk.Public(), identity.PurposeHandshakeHub, h[:], sig) == nil {
		t.Error("sig_hub verifies under the root key")
	}
}

func TestEveryHubHelloModeCarriesACertificate(t *testing.T) {
	for _, name := range []string{"enroll_invite", "pair_code"} {
		t.Run(name, func(t *testing.T) {
			var o outcome
			if name == "enroll_invite" {
				wc, hc, _ := enrollConfigs(t)
				o = runPair(t, wc, hc, nil)
			} else {
				wc, hc, _ := pairConfigs(t)
				o = runPair(t, wc, hc, nil)
			}
			o.mustSucceed(t)
			defer o.worker.Channel.Close()
			defer o.hub.Channel.Close()
			body, _ := proto.ReadFrame(bytes.NewReader(o.hWire), proto.HandshakeFrameLimit)
			var hub HubHello
			_ = json.Unmarshal(body, &hub)
			if len(hub.HubCert) != identity.HubCertSize {
				t.Errorf("hub_cert has %d bytes", len(hub.HubCert))
			}
			if !o.worker.RatchetChanged || o.worker.Ratchet.MaxSerial != 1 || !bytes.Equal(o.worker.Ratchet.Root, hub.HubPub) {
				t.Errorf("first ratchet %+v", o.worker.Ratchet)
			}
		})
	}
}

// Worker validation order (11.2 step 4): shape, root, signature, window,
// then the serial verdict after sig_hub.
func TestWorkerValidationSteps(t *testing.T) {
	type tc struct {
		name   string
		mutate func(t *testing.T, hub *tHub, body []byte) []byte
		want   error
		code   proto.Code
	}
	cases := []tc{
		{"a: missing certificate", func(t *testing.T, _ *tHub, b []byte) []byte {
			return mutateHubHello(t, b, func(m map[string]json.RawMessage) { delete(m, "hub_cert") })
		}, ErrCertMissing, proto.CodeInvalidArgument},
		{"a: truncated certificate", func(t *testing.T, h *tHub, b []byte) []byte {
			return mutateHubHello(t, b, func(m map[string]json.RawMessage) { m["hub_cert"] = rawB64(t, h.cert.Bytes()[:152]) })
		}, identity.ErrCertMalformed, proto.CodeInvalidArgument},
		{"a: unknown certificate version", func(t *testing.T, h *tHub, b []byte) []byte {
			c := h.cert.Bytes()
			c[0] = 2
			return mutateHubHello(t, b, func(m map[string]json.RawMessage) { m["hub_cert"] = rawB64(t, c) })
		}, identity.ErrCertMalformed, proto.CodeInvalidArgument},
		{"c: hub_pub differs from the certificate root", func(t *testing.T, h *tHub, b []byte) []byte {
			// hub_pub is still the pin, the certificate comes from another root.
			other := newHub(t)
			return mutateHubHello(t, b, func(m map[string]json.RawMessage) { m["hub_cert"] = rawB64(t, other.cert.Bytes()) })
		}, identity.ErrCertRootMismatch, proto.CodeUnauthenticated},
		{"c: certificate signature flipped", func(t *testing.T, h *tHub, b []byte) []byte {
			c := h.cert.Bytes()
			c[len(c)-1] ^= 1
			return mutateHubHello(t, b, func(m map[string]json.RawMessage) { m["hub_cert"] = rawB64(t, c) })
		}, identity.ErrCertBadSignature, proto.CodeUnauthenticated},
		{"c: certificate payload flipped (serial)", func(t *testing.T, h *tHub, b []byte) []byte {
			c := h.cert.Bytes()
			c[71] ^= 1
			return mutateHubHello(t, b, func(m map[string]json.RawMessage) { m["hub_cert"] = rawB64(t, c) })
		}, identity.ErrCertBadSignature, proto.CodeUnauthenticated},
		{"c: certificate signed by another key under the same root name", func(t *testing.T, h *tHub, b []byte) []byte {
			other := newHub(t)
			forged, err := other.signer.IssueHubCert(h.op.Public(), 1, h.cert.NotBeforeMs, h.cert.NotAfterMs)
			if err != nil {
				t.Fatal(err)
			}
			c := forged.Bytes()
			copy(c[1:33], h.cert.RootPub) // claims the pinned root
			return mutateHubHello(t, b, func(m map[string]json.RawMessage) { m["hub_cert"] = rawB64(t, c) })
		}, identity.ErrCertBadSignature, proto.CodeUnauthenticated},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			wc, hc, _, _ := connectConfigs(t)
			hub := &tHub{cert: hc.Cert, op: hc.Key}
			o := runPair(t, wc, hc, func(fromWorker bool, idx int, body []byte) []byte {
				if fromWorker || idx != 0 {
					return body
				}
				return c.mutate(t, hub, body)
			})
			if !errors.Is(o.workerErr, c.want) {
				t.Fatalf("worker error %v, want %v", o.workerErr, c.want)
			}
			if got := ErrorCode(o.workerErr); got != c.code {
				t.Errorf("code %s, want %s", got, c.code)
			}
			if o.worker != nil || o.hub != nil {
				t.Error("handshake completed")
			}
			onlyHello(t, o)
		})
	}
}

func TestWindowBoundaries(t *testing.T) {
	base := time.Unix(1_900_000_000, 0)
	nb := base.UnixMilli()
	na := base.Add(24 * time.Hour).UnixMilli()
	tol := identity.HubCertToleranceMs
	cases := []struct {
		name    string
		nowMs   int64
		want    error
		expired bool // the session admits nothing at once
	}{
		{"not_before - 1h accepted", nb - tol, nil, false},
		{"not_before - 1h - 1ms refused", nb - tol - 1, identity.ErrCertNotYetValid, false},
		{"inside the window", nb + 1000, nil, false},
		// The window check accepts now == not_after, but the >= rule of section
		// 11.3 ends the connection before sig_worker (RC1 release gate).
		{"now == not_after passes the window, closed before sig_worker", na, ErrExpiredBeforeSigWorker, true},
		{"now == not_after + 1ms refused", na + 1, identity.ErrCertExpired, false},
		{"far past not_after refused", na + 400*24*3600*1000, identity.ErrCertExpired, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			wc, _, _, _ := connectConfigs(t)
			hub := newHub(t)
			hub.reissueWindow(5, nb, na)
			hc := hub.cfg()
			wc.Pin = identity.Pin{Pub: hub.root.Public()}
			clk := &fakeWM{wall: time.UnixMilli(c.nowMs), mono: 7 * time.Second}
			wc.Clock = clk
			hc.Now = func() time.Time { return time.UnixMilli(na - 1) }
			o := runPair(t, wc, hc, nil)
			if c.want != nil {
				if !errors.Is(o.workerErr, c.want) {
					t.Fatalf("worker error %v, want %v", o.workerErr, c.want)
				}
				onlyHello(t, o)
				return
			}
			o.mustSucceed(t)
			defer o.worker.Channel.Close()
			defer o.hub.Channel.Close()
			if got := o.worker.Expiry; got.WallMs != na || got.DeadlineMono != 7*time.Second+time.Duration(na-c.nowMs)*time.Millisecond {
				t.Errorf("expiry %+v", got)
			}
			if got := o.worker.Expiry.Expired(clk); got != c.expired {
				t.Errorf("Expired at the check = %v, want %v", got, c.expired)
			}
		})
	}
}

func TestExpiryUsesBothClocksIndependently(t *testing.T) {
	cert := &identity.HubCert{NotAfterMs: 1_000_000}
	now := int64(900_000)
	mono0 := 50 * time.Second
	e, err := newExpiry(cert, now, mono0)
	if err != nil {
		t.Fatal(err)
	}
	if e.WallMs != 1_000_000 || e.DeadlineMono != 150*time.Second {
		t.Fatalf("expiry %+v", e)
	}
	clk := &fakeWM{}
	for _, c := range []struct {
		name string
		wall int64
		mono time.Duration
		want bool
	}{
		{"both before", 999_999, 149*time.Second + 999*time.Millisecond, false},
		{"wall jumps to exactly not_after, mono far from deadline", 1_000_000, 60 * time.Second, true},
		{"wall jumps far past, mono unmoved", 5_000_000, 50 * time.Second, true},
		{"mono equals the deadline, wall far in the past", 100, 150 * time.Second, true},
		{"mono past the deadline, wall moved backwards", 0, 151 * time.Second, true},
		{"wall moved backwards does not extend the session", 100, 150*time.Second + time.Nanosecond, true},
		{"wall one ms before, mono one ns before", 999_999, 150*time.Second - time.Nanosecond, false},
	} {
		clk.set(time.UnixMilli(c.wall), c.mono)
		if got := e.Expired(clk); got != c.want {
			t.Errorf("%s: Expired = %v, want %v", c.name, got, c.want)
		}
	}
	if _, err := newExpiry(cert, 2_000_000, 0); err == nil {
		t.Error("an already expired certificate got an expiry")
	}
}

func TestSerialScopeVerdicts(t *testing.T) {
	root := bytes.Repeat([]byte{1}, 32)
	h1 := [32]byte{1}
	h2 := [32]byte{2}
	r := Ratchet{Root: root, MaxSerial: 10, CertSHA256: h1[:]}
	if err := r.verdict(root, 10, h1); err != nil {
		t.Errorf("equal serial, same bytes: %v", err)
	}
	if err := r.verdict(root, 11, h2); err != nil {
		t.Errorf("higher serial: %v", err)
	}
	if err := r.verdict(root, 9, h1); !errors.Is(err, ErrCertSerialTooLow) {
		t.Errorf("lower serial: %v", err)
	}
	if err := r.verdict(root, 10, h2); !errors.Is(err, ErrCertSerialConflict) {
		t.Errorf("equal serial, other bytes: %v", err)
	}
	if err := r.verdict(bytes.Repeat([]byte{9}, 32), 1, h2); err != nil {
		t.Errorf("another root starts a new scope at 0: %v", err)
	}
	// A pin from 0.6 has max_serial 0 and no hash.
	if err := (Ratchet{Root: root}).verdict(root, 1, h1); err != nil {
		t.Errorf("0.6 pin: %v", err)
	}
	// Raise never lowers, keeps the first hash at an equal serial, and a new
	// root resets the scope first.
	if got := r.Raise(root, 5, h2); got.MaxSerial != 10 || !bytes.Equal(got.CertSHA256, h1[:]) {
		t.Errorf("raise lowered: %+v", got)
	}
	if got := (Ratchet{Root: root, MaxSerial: 3}).Raise(root, 3, h2); !bytes.Equal(got.CertSHA256, h2[:]) {
		t.Errorf("first hash at an equal serial not stored: %+v", got)
	}
	if got := r.Raise(bytes.Repeat([]byte{9}, 32), 2, h2); got.MaxSerial != 2 || got.Root[0] != 9 {
		t.Errorf("new root: %+v", got)
	}
	e := &RefusalError{Reason: ReasonCertSerialTooLow, Serial: 3, MaxSerial: 42}
	if e.Command() != "minion hub identity reissue --serial-floor 42" || !strings.Contains(e.Error(), e.Command()) {
		t.Errorf("operator line: %s", e)
	}
}

func TestRollbackAndSerialConflictRefusalSendsNoSigWorker(t *testing.T) {
	for _, tc := range []struct {
		name   string
		serial uint64
		stored func(h *tHub) Ratchet
		want   error
	}{
		{"rollback", 5, func(h *tHub) Ratchet {
			return Ratchet{Root: h.root.Public(), MaxSerial: 9, CertSHA256: bytes.Repeat([]byte{3}, 32)}
		}, ErrCertSerialTooLow},
		{"equal serial, different bytes", 9, func(h *tHub) Ratchet {
			return Ratchet{Root: h.root.Public(), MaxSerial: 9, CertSHA256: bytes.Repeat([]byte{3}, 32)}
		}, ErrCertSerialConflict},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wc, _, _, _ := connectConfigs(t)
			hub := newHub(t)
			hub.reissue(tc.serial)
			hc := hub.cfg()
			wc.Pin = identity.Pin{Pub: hub.root.Public()}
			wc.Ratchet = tc.stored(hub)
			called := false
			wc.BeforeSigWorker = func(HubAuth) error { called = true; return nil }
			o := runPair(t, wc, hc, nil)
			if !errors.Is(o.workerErr, tc.want) {
				t.Fatalf("worker error %v, want %v", o.workerErr, tc.want)
			}
			var re *RefusalError
			if !errors.As(o.workerErr, &re) || re.MaxSerial != 9 || re.Serial != tc.serial {
				t.Fatalf("refusal numbers: %+v", re)
			}
			if !strings.Contains(o.workerErr.Error(), "minion hub identity reissue --serial-floor 9") {
				t.Errorf("no operator command in %q", o.workerErr)
			}
			if called {
				t.Error("BeforeSigWorker ran for a refused certificate")
			}
			// The Hub's sig_hub arrived (the verdict is applied after it) but
			// the Worker sent only its hello and no frame naming max_serial.
			onlyHello(t, o)
			if bytes.Contains(o.wWire, []byte("9")) && bytes.Contains(o.wWire, []byte("max_serial")) {
				t.Error("max_serial on the wire")
			}
			if got := strings.Join(frameTypes(t, o.hWire), ","); got != "hello,sig" {
				t.Errorf("hub frames %s", got)
			}
			if ErrorCode(o.workerErr) != proto.CodeUnauthenticated {
				t.Errorf("code %s", ErrorCode(o.workerErr))
			}
		})
	}
}

func TestHigherSerialRaisesTheRatchetAndAnOtherRootResetsIt(t *testing.T) {
	wc, _, _, _ := connectConfigs(t)
	hub := newHub(t)
	hub.reissue(12)
	wc.Pin = identity.Pin{Pub: hub.root.Public()}
	wc.Ratchet = Ratchet{Root: hub.root.Public(), MaxSerial: 9, CertSHA256: bytes.Repeat([]byte{3}, 32)}
	o := runPair(t, wc, hub.cfg(), nil)
	o.mustSucceed(t)
	defer o.worker.Channel.Close()
	defer o.hub.Channel.Close()
	if !o.worker.RatchetChanged || o.worker.Ratchet.MaxSerial != 12 || !bytes.Equal(o.worker.Ratchet.CertSHA256, o.worker.CertSHA256[:]) {
		t.Errorf("ratchet %+v", o.worker.Ratchet)
	}

	// Same serial and same bytes again: accepted, nothing to commit.
	wc2, _, _, _ := connectConfigs(t)
	wc2.Pin = identity.Pin{Pub: hub.root.Public()}
	wc2.Ratchet = o.worker.Ratchet
	o2 := runPair(t, wc2, hub.cfg(), nil)
	o2.mustSucceed(t)
	defer o2.worker.Channel.Close()
	defer o2.hub.Channel.Close()
	if o2.worker.RatchetChanged {
		t.Error("an unchanged ratchet reported as changed")
	}

	// A verified root transition: scope resets (the new root starts at 0).
	keys, chain, hubs := rotate(t, 1)
	hubs[1].reissue(2)
	wc3 := WorkerConfig{Key: mustKey(t), Name: "w", Mode: ModeConnect, Pin: identity.Pin{Pub: keys[0].Public()},
		Ratchet: Ratchet{Root: keys[0].Public(), MaxSerial: 1 << 40, CertSHA256: bytes.Repeat([]byte{4}, 32)}}
	hc3 := hubs[1].cfg()
	hc3.Chain = chain
	o3 := runPair(t, wc3, hc3, nil)
	o3.mustSucceed(t)
	defer o3.worker.Channel.Close()
	defer o3.hub.Channel.Close()
	if o3.worker.Ratchet.MaxSerial != 2 || !bytes.Equal(o3.worker.Ratchet.Root, keys[1].Public()) || !o3.worker.RatchetChanged {
		t.Errorf("ratchet after a root transition: %+v", o3.worker.Ratchet)
	}
	if o3.worker.Pin.Seq != 1 {
		t.Errorf("pin %+v", o3.worker.Pin)
	}
}

func TestRootTransitionOrderAndRejectedCandidate(t *testing.T) {
	keys, chain, hubs := rotate(t, 1)
	// A certificate under the old root with the new hub_pub is refused.
	wc := WorkerConfig{Key: mustKey(t), Name: "w", Mode: ModeConnect, Pin: identity.Pin{Pub: keys[0].Public()},
		Ratchet: Ratchet{Root: keys[0].Public(), MaxSerial: 4}}
	hc := hubs[1].cfg()
	hc.Chain = chain
	o := runPair(t, wc, hc, func(fromWorker bool, idx int, body []byte) []byte {
		if fromWorker || idx != 0 {
			return body
		}
		return mutateHubHello(t, body, func(m map[string]json.RawMessage) { m["hub_cert"] = rawB64(t, hubs[0].cert.Bytes()) })
	})
	if !errors.Is(o.workerErr, identity.ErrCertRootMismatch) {
		t.Fatalf("worker error %v", o.workerErr)
	}
	onlyHello(t, o)
	// A chain that does not verify is reported before the certificate is looked
	// at (root before certificate).
	bad := bytes.Clone(chain[0].SigByOld)
	bad[0] ^= 1
	hc.Chain = []identity.Transition{{Seq: 1, OldPub: chain[0].OldPub, NewPub: chain[0].NewPub, CreatedAt: chain[0].CreatedAt, SigByOld: bad}}
	o = runPair(t, wc, hc, func(fromWorker bool, idx int, body []byte) []byte {
		if fromWorker || idx != 0 {
			return body
		}
		return mutateHubHello(t, body, func(m map[string]json.RawMessage) { m["hub_cert"] = rawB64(t, hubs[0].cert.Bytes()) })
	})
	if !errors.Is(o.workerErr, ErrChainInvalid) {
		t.Fatalf("worker error %v", o.workerErr)
	}
}

func TestPrecedenceEarliestReasonWins(t *testing.T) {
	// expired + too low reports cert_expired.
	wc, _, _, _ := connectConfigs(t)
	hub := newHub(t)
	now := time.Now()
	hub.reissueWindow(3, now.Add(-48*time.Hour).UnixMilli(), now.Add(-time.Hour).UnixMilli())
	wc.Pin = identity.Pin{Pub: hub.root.Public()}
	wc.Ratchet = Ratchet{Root: hub.root.Public(), MaxSerial: 10}
	hc := hub.cfg()
	hc.Now = func() time.Time { return now.Add(-2 * time.Hour) }
	o := runPair(t, wc, hc, nil)
	if !errors.Is(o.workerErr, identity.ErrCertExpired) || errors.Is(o.workerErr, ErrCertSerialTooLow) {
		t.Errorf("expired + too low: %v", o.workerErr)
	}

	// bad sig_hub + too low reports hub_sig_invalid: the old (stolen) operational
	// key signs sig_hub under the new certificate.
	wc2, _, _, _ := connectConfigs(t)
	hub2 := newHub(t)
	stolen := hub2.op
	hub2.reissue(3)
	wc2.Pin = identity.Pin{Pub: hub2.root.Public()}
	wc2.Ratchet = Ratchet{Root: hub2.root.Public(), MaxSerial: 10}
	var cap helloCapture
	o = runPair(t, wc2, hub2.cfg(), func(fromWorker bool, idx int, body []byte) []byte {
		cap.see(fromWorker, idx, body)
		if !fromWorker && idx == 1 {
			h := cap.hash()
			sig, err := stolen.Sign(identity.PurposeHandshakeHub, h[:])
			if err != nil {
				t.Fatal(err)
			}
			out, _ := json.Marshal(sigFrame{Type: proto.TypeSig, Sig: sig})
			return out
		}
		return body
	})
	if !errors.Is(o.workerErr, ErrHubSigInvalid) || errors.Is(o.workerErr, ErrCertSerialTooLow) {
		t.Errorf("bad sig_hub + too low: %v", o.workerErr)
	}
	onlyHello(t, o)
}

func TestSigHubByTheRootOrAStolenKeyIsRefused(t *testing.T) {
	for _, name := range []string{"root", "previous operational key"} {
		t.Run(name, func(t *testing.T) {
			wc, _, _, _ := connectConfigs(t)
			hub := newHub(t)
			prev := hub.op
			hub.reissue(2)
			wc.Pin = identity.Pin{Pub: hub.root.Public()}
			signer := hub.root
			if name != "root" {
				signer = prev
			}
			var cap helloCapture
			o := runPair(t, wc, hub.cfg(), func(fromWorker bool, idx int, body []byte) []byte {
				cap.see(fromWorker, idx, body)
				if !fromWorker && idx == 1 {
					h := cap.hash()
					sig, _ := signer.Sign(identity.PurposeHandshakeHub, h[:])
					out, _ := json.Marshal(sigFrame{Type: proto.TypeSig, Sig: sig})
					return out
				}
				return body
			})
			if !errors.Is(o.workerErr, ErrHubSigInvalid) || !errors.Is(o.workerErr, ErrBadSignature) {
				t.Fatalf("worker error %v", o.workerErr)
			}
			onlyHello(t, o)
		})
	}
}

func TestOneFlippedCertificateByteBreaksTheHandshake(t *testing.T) {
	// The certificate is in the hello, hence in the transcript: flipping a byte
	// of it (here the not_after, which the signature covers) is caught at the
	// certificate; flipping the hello framing instead breaks sig_hub.
	wc, hc, _, _ := connectConfigs(t)
	o := runPair(t, wc, hc, func(fromWorker bool, idx int, body []byte) []byte {
		if fromWorker || idx != 0 {
			return body
		}
		c := hc.Cert.Bytes()
		c[88] ^= 1
		return mutateHubHello(t, body, func(m map[string]json.RawMessage) { m["hub_cert"] = rawB64(t, c) })
	})
	if o.workerErr == nil || o.worker != nil || o.hub != nil {
		t.Fatalf("handshake survived a flipped certificate byte: %v", o.workerErr)
	}
	if o.hubErr == nil {
		t.Error("hub completed")
	}
}

func TestBeforeSigWorkerRunsAfterHubAuthAndGatesSigWorker(t *testing.T) {
	wc, hc, _, _ := connectConfigs(t)
	var got HubAuth
	wc.BeforeSigWorker = func(a HubAuth) error { got = a; return nil }
	o := runPair(t, wc, hc, nil)
	o.mustSucceed(t)
	defer o.worker.Channel.Close()
	defer o.hub.Channel.Close()
	if got.Cert == nil || got.Cert.Serial != 1 || !bytes.Equal(got.Root, hc.Cert.RootPub) || got.Ratchet.MaxSerial != 1 || got.Expiry.WallMs != hc.Cert.NotAfterMs {
		t.Errorf("HubAuth %+v", got)
	}

	// An error from the hook (the pair_resume record could not be made durable)
	// stops the handshake before sig_worker.
	wc, hc, _, _ = connectConfigs(t)
	boom := errors.New("disk full")
	wc.BeforeSigWorker = func(HubAuth) error { return boom }
	o = runPair(t, wc, hc, nil)
	if !errors.Is(o.workerErr, boom) {
		t.Fatalf("worker error %v", o.workerErr)
	}
	onlyHello(t, o)
}

func TestInvitationFingerprintIsComparedWithTheRootNotTheOperationalKey(t *testing.T) {
	wc, hc, _ := enrollConfigs(t)
	wc.HubFingerprint = hc.Key.Fingerprint() // the operational key's fingerprint
	o := runPair(t, wc, hc, nil)
	if !errors.Is(o.workerErr, ErrFingerprintMismatch) {
		t.Fatalf("worker error %v", o.workerErr)
	}
	onlyHello(t, o)
}

func TestPairResumeBindsTheRootAndTheSerial(t *testing.T) {
	// (e) a Hub with a different root answers the resume: refused before any
	// CPace message and before sig_worker, even though it knows the code.
	wc, hc, code := pairConfigs(t)
	wc.Ratchet = Ratchet{Root: bytes.Repeat([]byte{5}, 32), MaxSerial: 1, CertSHA256: bytes.Repeat([]byte{6}, 32)}
	called := false
	wc.BeforeSigWorker = func(HubAuth) error { called = true; return nil }
	o := runPair(t, wc, hc, nil)
	if !errors.Is(o.workerErr, ErrResumeRootMismatch) {
		t.Fatalf("worker error %v", o.workerErr)
	}
	if called {
		t.Error("record written for a substituted Hub")
	}
	onlyHello(t, o)
	_ = code

	// Same root: a higher serial is accepted and raises the ratchet; a lower
	// serial and an equal serial with other bytes are refused after CPace and
	// sig_hub, with no sig_worker.
	hub := newHub(t)
	hub.reissue(5)
	pw := identity.NewPairingCode()
	mk := func(r Ratchet) outcome {
		wc := WorkerConfig{Key: mustKey(t), Name: "p", Mode: ModePairCode, PairingCode: pw, Ratchet: r}
		hc := hub.cfg()
		hc.PairingPassword = func(string) ([]byte, error) { return pw.Password(), nil }
		return runPair(t, wc, hc, nil)
	}
	root := hub.root.Public()
	o = mk(Ratchet{Root: root, MaxSerial: 3, CertSHA256: bytes.Repeat([]byte{1}, 32)})
	o.mustSucceed(t)
	defer o.worker.Channel.Close()
	defer o.hub.Channel.Close()
	if o.worker.Ratchet.MaxSerial != 5 || !o.worker.RatchetChanged {
		t.Errorf("ratchet %+v", o.worker.Ratchet)
	}
	o = mk(Ratchet{Root: root, MaxSerial: 8, CertSHA256: bytes.Repeat([]byte{1}, 32)})
	if !errors.Is(o.workerErr, ErrCertSerialTooLow) {
		t.Fatalf("lower serial: %v", o.workerErr)
	}
	for _, ty := range frameTypes(t, o.wWire) {
		if ty == proto.TypeSig {
			t.Error("sig_worker sent after a serial refusal")
		}
	}
	o = mk(Ratchet{Root: root, MaxSerial: 5, CertSHA256: bytes.Repeat([]byte{1}, 32)})
	if !errors.Is(o.workerErr, ErrCertSerialConflict) {
		t.Fatalf("equal serial, other bytes: %v", o.workerErr)
	}
}

func TestPairCodeConfirmationIsSignedByTheOperationalKey(t *testing.T) {
	wc, hc, _ := pairConfigs(t)
	var cap helloCapture
	o := runPair(t, wc, hc, func(fromWorker bool, idx int, body []byte) []byte {
		cap.see(fromWorker, idx, body)
		// Hub's pake.confirm is its frame index 2 (hello, pake.msg, pake.confirm):
		// re-sign it with the ROOT key instead of the operational key.
		if !fromWorker && idx == 2 {
			var f pakeConfirmFrame
			if err := json.Unmarshal(body, &f); err != nil {
				t.Fatal(err)
			}
			h := cap.hash()
			rootKey := mustKey(t) // any key but the operational one
			sig, _ := rootKey.Sign(identity.PurposePAKEConfirm, pakeConfirmData(h[:], f.Tag))
			f.Sig = sig
			out, _ := json.Marshal(f)
			return out
		}
		return body
	})
	if !errors.Is(o.workerErr, ErrBadSignature) {
		t.Fatalf("worker error %v, want the pake-confirm signature refused", o.workerErr)
	}
	onlyHelloAndPake(t, o)
}

func onlyHelloAndPake(t *testing.T, o outcome) {
	t.Helper()
	for _, ty := range frameTypes(t, o.wWire) {
		if ty == proto.TypeSig {
			t.Error("sig_worker sent")
		}
	}
}

func TestHubRefusesToSendAnExpiredCertificate(t *testing.T) {
	wc, _, _, _ := connectConfigs(t)
	hub := newHub(t)
	wc.Pin = identity.Pin{Pub: hub.root.Public()}
	hc := hub.cfg()
	na := hub.cert.NotAfterMs
	hc.Now = func() time.Time { return time.UnixMilli(na) } // now >= not_after
	o := runPair(t, wc, hc, nil)
	if proto.CodeOf(o.hubErr) != proto.CodeHubCertExpired {
		t.Fatalf("hub error %v", o.hubErr)
	}
	if proto.CodeOf(o.workerErr) != proto.CodeHubCertExpired {
		t.Fatalf("worker error %v", o.workerErr)
	}
	if got := strings.Join(frameTypes(t, o.hWire), ","); got != "error" {
		t.Errorf("hub sent %s, want only the error frame (no hello with an expired certificate)", got)
	}
	// One millisecond before not_after it is still served.
	hc.Now = func() time.Time { return time.UnixMilli(na - 1) }
	wc2, _, _, _ := connectConfigs(t)
	wc2.Pin = identity.Pin{Pub: hub.root.Public()}
	o = runPair(t, wc2, hc, nil)
	o.mustSucceed(t)
	o.roundTrip(t)
}

func TestHubConfigNeedsACertificateOfItsOwnKey(t *testing.T) {
	hub := newHub(t)
	other := newHub(t)
	for name, hc := range map[string]HubConfig{
		"no certificate": {Key: hub.op},
		"no key":         {Cert: hub.cert},
		"foreign key":    {Key: other.op, Cert: hub.cert},
		"unsigned":       {Key: hub.op, Cert: &identity.HubCert{RootPub: hub.cert.RootPub, OpPub: hub.cert.OpPub, Serial: 1, NotBeforeMs: 1, NotAfterMs: 2}},
	} {
		a, b := net.Pipe()
		_, err := HubHandshake(a, hc)
		_ = b.Close()
		if !errors.Is(err, ErrConfig) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestA2xWorkerGetsProtoIncompatible(t *testing.T) {
	wc, hc, _, _ := connectConfigs(t)
	wc.Protocols = proto.Range{Min: proto.Version{Major: 2}, Max: proto.Version{Major: 2}}
	o := runPair(t, wc, hc, nil)
	if proto.CodeOf(o.hubErr) != proto.CodeProtoIncompatible || proto.CodeOf(o.workerErr) != proto.CodeProtoIncompatible {
		t.Errorf("hub %v worker %v", o.hubErr, o.workerErr)
	}
	// And a 3.0 Worker refuses a Hub that answers 2.0.
	wc, hc, _, _ = connectConfigs(t)
	hc.Current = proto.Version{Major: 2}
	o = runPair(t, wc, hc, nil)
	if proto.CodeOf(o.workerErr) != proto.CodeProtoIncompatible {
		t.Errorf("worker %v", o.workerErr)
	}
}

func TestErrorCodesOfTheCertificateReasons(t *testing.T) {
	for err, want := range map[error]proto.Code{
		ErrCertMissing:                 proto.CodeInvalidArgument,
		identity.ErrCertMalformed:      proto.CodeInvalidArgument,
		identity.ErrCertRootMismatch:   proto.CodeUnauthenticated,
		identity.ErrCertBadSignature:   proto.CodeUnauthenticated,
		identity.ErrCertNotYetValid:    proto.CodeUnauthenticated,
		identity.ErrCertExpired:        proto.CodeUnauthenticated,
		ErrResumeRootMismatch:          proto.CodeUnauthenticated,
		ErrHubSigInvalid:               proto.CodeUnauthenticated,
		ErrCertSerialTooLow:            proto.CodeUnauthenticated,
		ErrCertSerialConflict:          proto.CodeUnauthenticated,
		ErrPairingConfirm:              proto.CodePairingCodeInvalid,
		&RefusalError{Reason: "other"}: proto.CodeInternal,
	} {
		if got := ErrorCode(err); got != want {
			t.Errorf("%v: %s, want %s", err, got, want)
		}
	}
}

func TestHubRechecksTheDeadlineAfterABlockingPairingPassword(t *testing.T) {
	wc, hc, code := pairConfigs(t)
	na := hc.Cert.NotAfterMs
	var mu sync.Mutex
	nowMs := na - 1000
	hc.Now = func() time.Time { mu.Lock(); defer mu.Unlock(); return time.UnixMilli(nowMs) }
	hc.PairingPassword = func(string) ([]byte, error) {
		// The user claims the code a moment later: the certificate ended meanwhile.
		mu.Lock()
		nowMs = na
		mu.Unlock()
		return code.Password(), nil
	}
	o := runPair(t, wc, hc, nil)
	if proto.CodeOf(o.hubErr) != proto.CodeHubCertExpired {
		t.Fatalf("hub error %v", o.hubErr)
	}
	if got := strings.Join(frameTypes(t, o.hWire), ","); got != "error" {
		t.Errorf("hub sent %s, want only the error frame (no hello after the deadline)", got)
	}
}

func TestTranscriptBindsTheCertificate(t *testing.T) {
	// A certificate with another payload (here another not_after) that the same
	// root signs validly passes every certificate step; the Hub signed the
	// transcript of the original hello, so sig_hub fails, and the Worker sends
	// no sig_worker.
	wc, _, _, _ := connectConfigs(t)
	hub := newHub(t)
	wc.Pin = identity.Pin{Pub: hub.root.Public()}
	other, err := hub.signer.IssueHubCert(hub.op.Public(), hub.cert.Serial, hub.cert.NotBeforeMs, hub.cert.NotAfterMs+1000)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(other.Bytes(), hub.cert.Bytes()) {
		t.Fatal("the re-signed certificate equals the original")
	}
	o := runPair(t, wc, hub.cfg(), func(fromWorker bool, idx int, body []byte) []byte {
		if fromWorker || idx != 0 {
			return body
		}
		return mutateHubHello(t, body, func(m map[string]json.RawMessage) { m["hub_cert"] = rawB64(t, other.Bytes()) })
	})
	if !errors.Is(o.workerErr, ErrHubSigInvalid) {
		t.Fatalf("worker error %v, want sig_hub to fail after the certificate steps passed", o.workerErr)
	}
	onlyHello(t, o)
	if o.worker != nil {
		t.Error("handshake succeeded")
	}
}
