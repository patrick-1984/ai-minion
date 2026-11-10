package pake

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/sha512"
	"errors"
	"testing"
)

func testParams(ad string) Params {
	sid := sha256.Sum256([]byte("transcript"))
	return Params{
		Password:  []byte("MN-ABCD-EFGH"),
		SessionID: sid[:],
		ChannelID: []byte("minion/v1/pair"),
		AD:        []byte(ad),
	}
}

type run struct {
	a, b       *Session
	msgA, msgB []byte
	errB, errA error
}

func exchange(t *testing.T, pa, pb Params, tamperA, tamperB func([]byte)) run {
	t.Helper()
	init, msgA, err := Start(pa)
	if err != nil {
		t.Fatal(err)
	}
	if tamperA != nil {
		tamperA(msgA)
	}
	var r run
	r.msgA = msgA
	r.b, r.msgB, r.errB = Respond(pb, msgA)
	if r.errB != nil {
		return r
	}
	if tamperB != nil {
		tamperB(r.msgB)
	}
	r.a, r.errA = init.Finish(r.msgB)
	return r
}

// confirmed reports whether both directions of key confirmation succeed.
func confirmed(r run) bool {
	return r.a.VerifyConfirmation(r.b.Confirmation()) == nil &&
		r.b.VerifyConfirmation(r.a.Confirmation()) == nil
}

func TestRoundTrip(t *testing.T) {
	r := exchange(t, testParams("worker"), testParams("hub"), nil, nil)
	if r.errA != nil || r.errB != nil {
		t.Fatal(r.errA, r.errB)
	}
	if !bytes.Equal(r.a.Key(), r.b.Key()) || len(r.a.Key()) != KeySize {
		t.Fatal("keys differ")
	}
	if !confirmed(r) {
		t.Fatal("confirmation failed")
	}
	if len(r.a.Confirmation()) != ConfirmationSize {
		t.Fatal("bad tag size")
	}
	if bytes.Equal(r.a.Confirmation(), r.b.Confirmation()) {
		t.Fatal("tags of both directions must differ")
	}
	if string(r.b.PeerAD()) != "worker" || string(r.a.PeerAD()) != "hub" {
		t.Fatal("PeerAD wrong")
	}
	// A second run yields a fresh key.
	r2 := exchange(t, testParams("worker"), testParams("hub"), nil, nil)
	if bytes.Equal(r.a.Key(), r2.a.Key()) {
		t.Fatal("two runs produced the same key")
	}
}

func TestMismatchedInputsFail(t *testing.T) {
	cases := map[string]func(*Params){
		"wrong password":   func(p *Params) { p.Password = []byte("MN-ABCD-EFGX") },
		"wrong session id": func(p *Params) { p.SessionID = append(clone(p.SessionID[:31]), p.SessionID[31]^1) },
		"wrong channel id": func(p *Params) { p.ChannelID = []byte("other") },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			pb := testParams("hub")
			mutate(&pb)
			r := exchange(t, testParams("worker"), pb, nil, nil)
			if r.errA != nil || r.errB != nil {
				t.Fatal("CPace aborts only on invalid points; mismatch must show as different keys")
			}
			if bytes.Equal(r.a.Key(), r.b.Key()) {
				t.Fatal("keys match despite mismatch")
			}
			if r.a.VerifyConfirmation(r.b.Confirmation()) == nil || r.b.VerifyConfirmation(r.a.Confirmation()) == nil {
				t.Fatal("confirmation accepted despite mismatch")
			}
		})
	}
}

func TestTamperedMessageFails(t *testing.T) {
	cases := map[string]struct{ ta, tb func([]byte) }{
		"message A share":  {ta: func(m []byte) { m[0] ^= 0x04 }},
		"message A AD":     {ta: func(m []byte) { m[ElementSize] ^= 1 }},
		"message B share":  {tb: func(m []byte) { m[0] ^= 0x04 }},
		"message B AD":     {tb: func(m []byte) { m[len(m)-1] ^= 1 }},
		"message B Y high": {tb: func(m []byte) { m[ElementSize-1] ^= 0x80 }},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			r := exchange(t, testParams("worker"), testParams("hub"), c.ta, c.tb)
			if r.errA != nil || r.errB != nil {
				if !errors.Is(r.errA, ErrInvalidMessage) && !errors.Is(r.errB, ErrInvalidMessage) {
					t.Fatalf("unexpected error kind: %v %v", r.errA, r.errB)
				}
				return // aborted: fine
			}
			if confirmed(r) {
				t.Fatal("tampered exchange confirmed")
			}
		})
	}
}

func TestRejectsIdentityAndInvalidEncodings(t *testing.T) {
	p := testParams("x")
	identity := make([]byte, ElementSize)
	nonCanonical := bytes.Repeat([]byte{0xff}, ElementSize) // > field prime
	negative := make([]byte, ElementSize)
	negative[0] = 1 // odd field element: not a canonical ristretto encoding

	cases := map[string]struct {
		msg  []byte
		want error
	}{
		"identity":         {identity, ErrIdentityElement},
		"identity with AD": {append(clone(identity), "ad"...), ErrIdentityElement},
		"non-canonical":    {nonCanonical, ErrInvalidMessage},
		"negative":         {negative, ErrInvalidMessage},
		"short":            {identity[:31], ErrInvalidMessage},
		"empty":            {nil, ErrInvalidMessage},
		"oversized AD":     {make([]byte, ElementSize+MaxADSize+1), ErrInvalidMessage},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if _, _, err := Respond(p, c.msg); !errors.Is(err, c.want) {
				t.Errorf("Respond: got %v, want %v", err, c.want)
			}
			init, _, err := Start(p)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := init.Finish(c.msg); !errors.Is(err, c.want) {
				t.Errorf("Finish: got %v, want %v", err, c.want)
			}
		})
	}
}

func TestParamsValidation(t *testing.T) {
	for name, mutate := range map[string]func(*Params){
		"empty password": func(p *Params) { p.Password = nil },
		"empty sid":      func(p *Params) { p.SessionID = nil },
		"AD too long":    func(p *Params) { p.AD = make([]byte, MaxADSize+1) },
	} {
		p := testParams("x")
		mutate(&p)
		if _, _, err := Start(p); !errors.Is(err, ErrInvalidParams) {
			t.Errorf("%s: Start: %v", name, err)
		}
		_, msgA, _ := Start(testParams("x"))
		if _, _, err := Respond(p, msgA); !errors.Is(err, ErrInvalidParams) {
			t.Errorf("%s: Respond: %v", name, err)
		}
	}
}

func TestInitiatorSingleUse(t *testing.T) {
	init, msgA, err := Start(testParams("w"))
	if err != nil {
		t.Fatal(err)
	}
	_, msgB, err := Respond(testParams("h"), msgA)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := init.Finish(msgB); err != nil {
		t.Fatal(err)
	}
	if _, err := init.Finish(msgB); !errors.Is(err, ErrInitiatorFinished) {
		t.Fatalf("second Finish: %v", err)
	}
	// A failed Finish also consumes the state.
	init2, _, _ := Start(testParams("w"))
	if _, err := init2.Finish(nil); err == nil {
		t.Fatal("expected error")
	}
	if _, err := init2.Finish(msgB); !errors.Is(err, ErrInitiatorFinished) {
		t.Fatalf("Finish after failure: %v", err)
	}
	var nilInit *Initiator
	if _, err := nilInit.Finish(msgB); !errors.Is(err, ErrInitiatorFinished) {
		t.Fatal("nil initiator accepted")
	}
}

func TestVerifyConfirmationRejectsVariants(t *testing.T) {
	r := exchange(t, testParams("w"), testParams("h"), nil, nil)
	tag := r.b.Confirmation()
	for name, bad := range map[string][]byte{
		"empty":     nil,
		"truncated": tag[:len(tag)-1],
		"extended":  append(clone(tag), 0),
		"flipped":   append(append(clone(tag[:10]), tag[10]^1), tag[11:]...),
		"own tag":   r.a.Confirmation(),
	} {
		if err := r.a.VerifyConfirmation(bad); !errors.Is(err, ErrConfirmationFailed) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestKeyReturnsCopy(t *testing.T) {
	r := exchange(t, testParams("w"), testParams("h"), nil, nil)
	k := r.a.Key()
	k[0] ^= 0xff
	if !bytes.Equal(r.a.Key(), r.b.Key()) {
		t.Fatal("Key exposed internal state")
	}
}

// zeroThenReader yields zero bytes for the first n reads, then 0xff.
type zeroThenReader struct{ n int }

func (z *zeroThenReader) Read(p []byte) (int, error) {
	v := byte(0xff)
	if z.n > 0 {
		z.n--
		v = 0
	}
	for i := range p {
		p[i] = v
	}
	return len(p), nil
}

func TestSampleScalar(t *testing.T) {
	// All-0xff input: top bits are cleared, giving 2^252-1 < group order.
	s, err := sampleScalar(&zeroThenReader{})
	if err != nil {
		t.Fatal(err)
	}
	b := s.Bytes()
	if b[31] != 0x0f || !bytes.Equal(b[:31], bytes.Repeat([]byte{0xff}, 31)) {
		t.Fatalf("unexpected scalar %x", b)
	}
	// A zero sample is rejected and resampled.
	s, err = sampleScalar(&zeroThenReader{n: 1})
	if err != nil {
		t.Fatal(err)
	}
	if s.Bytes()[0] == 0 {
		t.Fatal("zero scalar accepted")
	}
	if _, err := sampleScalar(bytes.NewReader(nil)); err == nil {
		t.Fatal("expected error on short randomness")
	}
}

func TestFinishRejectsReflectedMessage(t *testing.T) {
	for _, ad := range []string{"", "worker-ad"} {
		init, msgA, err := Start(testParams(ad))
		if err != nil {
			t.Fatal(err)
		}
		s, err := init.Finish(msgA)
		if !errors.Is(err, ErrReflected) || s != nil {
			t.Fatalf("ad %q: want ErrReflected and nil session, got %v, %v", ad, s, err)
		}
	}
	// Same share, different AD: still reflected (share equality).
	init, msgA, _ := Start(testParams("x"))
	forged := append(append([]byte(nil), msgA[:ElementSize]...), []byte("other")...)
	if s, err := init.Finish(forged); !errors.Is(err, ErrReflected) || s != nil {
		t.Fatalf("same share, other AD: got %v, %v", s, err)
	}
}

func TestReflectedConfirmationRejected(t *testing.T) {
	for _, ad := range []string{"", "ad"} {
		r := exchange(t, testParams(ad), testParams(ad), nil, nil)
		if r.errA != nil || r.errB != nil {
			t.Fatal(r.errA, r.errB)
		}
		if r.a.VerifyConfirmation(r.a.Confirmation()) == nil {
			t.Fatal("initiator accepted its own tag")
		}
		if r.b.VerifyConfirmation(r.b.Confirmation()) == nil {
			t.Fatal("responder accepted its own tag")
		}
		if bytes.Equal(r.a.Confirmation(), r.b.Confirmation()) {
			t.Fatal("tags of both directions are equal")
		}
	}
}

// leb returns LEB128(len(b)) || b, written independently of the package code.
func leb(b []byte) []byte {
	n := len(b)
	var out []byte
	for n >= 0x80 {
		out = append(out, byte(n&0x7f|0x80))
		n >>= 7
	}
	out = append(out, byte(n))
	return append(out, b...)
}

// expectedTag computes the draft 10.4 tag from first principles:
// mac_key = SHA-512("CPaceMac" || sid || ISK), T = HMAC-SHA-512(mac_key, lv_cat(Y, AD)).
func expectedTag(sid, isk, y, ad []byte) []byte {
	mk := sha512.Sum512(bytes.Join([][]byte{[]byte("CPaceMac"), sid, isk}, nil))
	m := hmac.New(sha512.New, mk[:])
	m.Write(leb(y))
	m.Write(leb(ad))
	return m.Sum(nil)
}

func TestConfirmationMatchesDraft104(t *testing.T) {
	for _, ad := range [][2]string{{"", ""}, {"worker", "hub"}} {
		pa, pb := testParams(ad[0]), testParams(ad[1])
		r := exchange(t, pa, pb, nil, nil)
		if r.errA != nil || r.errB != nil {
			t.Fatal(r.errA, r.errB)
		}
		ta := expectedTag(pa.SessionID, r.a.Key(), r.msgA[:ElementSize], r.msgA[ElementSize:])
		tb := expectedTag(pa.SessionID, r.a.Key(), r.msgB[:ElementSize], r.msgB[ElementSize:])
		if !bytes.Equal(r.a.Confirmation(), ta) || !bytes.Equal(r.b.Confirmation(), tb) {
			t.Fatal("confirmation differs from independent draft 10.4 tag")
		}
		if r.a.VerifyConfirmation(tb) != nil || r.b.VerifyConfirmation(ta) != nil {
			t.Fatal("independent peer tags rejected")
		}
		if r.a.VerifyConfirmation(ta) == nil || r.b.VerifyConfirmation(tb) == nil {
			t.Fatal("own tag accepted")
		}
	}
}

func TestFreshScalarsPerSession(t *testing.T) {
	r1 := exchange(t, testParams("a"), testParams("b"), nil, nil)
	r2 := exchange(t, testParams("a"), testParams("b"), nil, nil)
	if bytes.Equal(r1.msgA, r2.msgA) || bytes.Equal(r1.msgB, r2.msgB) {
		t.Fatal("messages repeat across sessions")
	}
	if bytes.Equal(r1.a.Key(), r2.a.Key()) {
		t.Fatal("session keys repeat")
	}
}
