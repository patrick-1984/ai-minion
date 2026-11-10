package identity

import (
	"bytes"
	"crypto/ed25519"
	"encoding/binary"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

var baseTime = time.Unix(1700000000, 0).UTC()

func mustKey(t *testing.T) *Key {
	t.Helper()
	k, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// buildChain returns keys k0..kn and the links k0->k1 (seq first), k1->k2, ...
func buildChain(t *testing.T, n int, first uint64) ([]*Key, []Transition) {
	t.Helper()
	keys := []*Key{mustKey(t)}
	var chain []Transition
	for i := 0; i < n; i++ {
		next := mustKey(t)
		tr, err := NewTransition(keys[i], next.Public(), first+uint64(i), baseTime.Add(time.Duration(i)*time.Second))
		if err != nil {
			t.Fatal(err)
		}
		keys = append(keys, next)
		chain = append(chain, tr)
	}
	return keys, chain
}

func TestVerifyChainValid(t *testing.T) {
	keys, chain := buildChain(t, 3, 1)
	pin, err := VerifyChain(Pin{Pub: keys[0].Public()}, chain)
	if err != nil {
		t.Fatal(err)
	}
	if pin.Seq != 3 || string(pin.Pub) != string(keys[3].Public()) {
		t.Fatalf("pin %+v", pin)
	}
	// A Worker that already followed the first link starts from its position.
	pin2, err := VerifyChain(Pin{Pub: keys[1].Public(), Seq: 1}, chain[1:])
	if err != nil || pin2.Seq != 3 || string(pin2.Pub) != string(keys[3].Public()) {
		t.Fatalf("%+v %v", pin2, err)
	}
}

func TestVerifyChainEmptyKeepsPin(t *testing.T) {
	k := mustKey(t)
	pin, err := VerifyChain(Pin{Pub: k.Public(), Seq: 4}, nil)
	if err != nil || pin.Seq != 4 || string(pin.Pub) != string(k.Public()) {
		t.Fatalf("%+v %v", pin, err)
	}
}

func TestVerifyChainDoesNotAliasPin(t *testing.T) {
	k := mustKey(t)
	in := Pin{Pub: k.Public()}
	out, err := VerifyChain(in, nil)
	if err != nil {
		t.Fatal(err)
	}
	out.Pub[0] ^= 0xff
	if in.Pub[0] == out.Pub[0] {
		t.Fatal("result aliases the input pin")
	}
}

func TestVerifyChainTable(t *testing.T) {
	keys, chain := buildChain(t, 3, 1)
	pin0 := Pin{Pub: keys[0].Public()}
	other := mustKey(t)

	mutate := func(f func(c []Transition) []Transition) []Transition {
		c := make([]Transition, len(chain))
		for i, tr := range chain {
			tr.OldPub = append(B64(nil), tr.OldPub...)
			tr.NewPub = append(B64(nil), tr.NewPub...)
			tr.SigByOld = append(B64(nil), tr.SigByOld...)
			c[i] = tr
		}
		return f(c)
	}
	forged, _ := NewTransition(other, mustKey(t).Public(), 1, baseTime)

	tests := []struct {
		name  string
		pin   Pin
		chain []Transition
	}{
		{"gap", pin0, []Transition{chain[0], chain[2]}},
		{"first link skips a seq", Pin{Pub: keys[0].Public(), Seq: 0}, chain[1:]},
		{"replayed seq", Pin{Pub: keys[1].Public(), Seq: 1}, chain[:1]},
		{"chain starts at or before the pin position", Pin{Pub: keys[2].Public(), Seq: 2}, chain[1:]},
		{"decreasing seq", pin0, []Transition{chain[0], chain[1], chain[0]}},
		{"same seq twice", pin0, []Transition{chain[0], chain[0]}},
		{"bad signature", pin0, mutate(func(c []Transition) []Transition { c[1].SigByOld[0] ^= 1; return c })},
		{"tampered new_pub", pin0, mutate(func(c []Transition) []Transition { c[0].NewPub[0] ^= 1; return c })},
		{"tampered created_at", pin0, mutate(func(c []Transition) []Transition { c[0].CreatedAt = c[0].CreatedAt.Add(time.Millisecond); return c })},
		{"empty signature", pin0, mutate(func(c []Transition) []Transition { c[0].SigByOld = nil; return c })},
		{"signed by a stranger", pin0, []Transition{forged}},
		{"chain does not start at the pin", Pin{Pub: other.Public()}, chain},
		{"link does not continue", pin0, []Transition{chain[0], chain[2]}},
		{"bad pin", Pin{Pub: []byte{1, 2, 3}}, chain},
		{"short old_pub", pin0, mutate(func(c []Transition) []Transition { c[0].OldPub = c[0].OldPub[:5]; return c })},
		{"key unchanged", pin0, func() []Transition {
			tr, _ := NewTransition(keys[0], keys[0].Public(), 1, baseTime)
			return []Transition{tr}
		}()},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := VerifyChain(tc.pin, tc.chain); !errors.Is(err, ErrInvalidArgument) {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

// The transition signature uses purpose "transition", not any other.
func TestTransitionSignaturePurpose(t *testing.T) {
	keys, chain := buildChain(t, 1, 1)
	tr := chain[0]
	for _, p := range allPurposes {
		sig, _ := keys[0].Sign(p, tr.signedData())
		tr2 := tr
		tr2.SigByOld = sig
		err := tr2.Verify()
		if (p == PurposeTransition) != (err == nil) {
			t.Errorf("purpose %s: err = %v", p, err)
		}
	}
}

func TestNewTransitionBadKey(t *testing.T) {
	if _, err := NewTransition(mustKey(t), ed25519.PublicKey{1}, 1, baseTime); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("err = %v", err)
	}
}

func TestTransitionJSON(t *testing.T) {
	_, chain := buildChain(t, 1, 1)
	raw, err := json.Marshal(chain[0])
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	for _, f := range []string{"seq", "old_pub", "new_pub", "created_at", "sig_by_old"} {
		if _, ok := m[f]; !ok {
			t.Errorf("missing field %s", f)
		}
	}
	var back Transition
	if err := json.Unmarshal(raw, &back); err != nil || back.Verify() != nil {
		t.Fatalf("round trip: %v", err)
	}
}

// Workers that enrolled after a rotation do not know the Hub's chain position:
// their pin has Seq 0 and the first link may carry any seq >= 1.
func TestVerifyChainUnknownPosition(t *testing.T) {
	k3, k4, k5 := mustKey(t), mustKey(t), mustKey(t)
	l4, err := NewTransition(k3, k4.Public(), 4, baseTime)
	if err != nil {
		t.Fatal(err)
	}
	l5, err := NewTransition(k4, k5.Public(), 5, baseTime)
	if err != nil {
		t.Fatal(err)
	}
	l7, err := NewTransition(k4, k5.Public(), 7, baseTime)
	if err != nil {
		t.Fatal(err)
	}
	l0, err := NewTransition(k3, k4.Public(), 0, baseTime)
	if err != nil {
		t.Fatal(err)
	}
	k := mustKey(t)

	t.Run("accepted", func(t *testing.T) {
		pin, err := VerifyChain(Pin{Pub: k3.Public()}, []Transition{l4, l5})
		if err != nil || pin.Seq != 5 || !bytes.Equal(pin.Pub, k5.Public()) {
			t.Fatalf("pin %+v err %v", pin, err)
		}
	})
	t.Run("known position accepts exactly the next link", func(t *testing.T) {
		pin, err := VerifyChain(Pin{Pub: k3.Public(), Seq: 3}, []Transition{l4, l5})
		if err != nil || pin.Seq != 5 {
			t.Fatalf("pin %+v err %v", pin, err)
		}
	})
	rejects := []struct {
		name  string
		pin   Pin
		chain []Transition
	}{
		{"known position, first link skips seq", Pin{Pub: k4.Public(), Seq: 5}, []Transition{l7}},
		{"known position, gap after the pin", Pin{Pub: k3.Public(), Seq: 2}, []Transition{l4}},
		{"unknown position, first link from another key", Pin{Pub: k.Public()}, []Transition{l4, l5}},
		{"unknown position, gap after the first link", Pin{Pub: k3.Public()}, []Transition{l4, l7}},
		{"unknown position, seq 0 link", Pin{Pub: k3.Public()}, []Transition{l0}},
		{"unknown position, first link is not from the pin key", Pin{Pub: k3.Public()}, []Transition{l5}},
	}
	for _, tc := range rejects {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := VerifyChain(tc.pin, tc.chain); !errors.Is(err, ErrInvalidArgument) {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

// The JSON form of created_at is an RFC 3339 UTC string with exactly three
// fractional digits (spine Consistency Conventions).
func TestTransitionJSONTimestampForm(t *testing.T) {
	at := time.Unix(1700000000, 0)
	tr, err := NewTransition(mustKey(t), mustKey(t).Public(), 1, at.In(time.FixedZone("x", 5*3600)))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(tr)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if got := m["created_at"]; got != "2023-11-14T22:13:20.000Z" {
		t.Fatalf("created_at = %#v", got)
	}
	// Milliseconds are kept, finer precision is truncated by NewTransition.
	tr2, _ := NewTransition(mustKey(t), mustKey(t).Public(), 1, time.Unix(1700000000, 123_456_789))
	raw2, _ := json.Marshal(tr2)
	if !bytes.Contains(raw2, []byte(`"created_at":"2023-11-14T22:13:20.123Z"`)) {
		t.Fatalf("%s", raw2)
	}
}

// A link received with a non-UTC offset for the same instant still verifies,
// because only the instant is signed. Unrepresentable precision is refused.
func TestTransitionJSONOffsetAndPrecision(t *testing.T) {
	keys, chain := buildChain(t, 1, 1)
	raw, _ := json.Marshal(chain[0])
	var m map[string]any
	_ = json.Unmarshal(raw, &m)

	decode := func(ts string) (Transition, error) {
		m["created_at"] = ts
		b, _ := json.Marshal(m)
		var tr Transition
		err := json.Unmarshal(b, &tr)
		return tr, err
	}
	// baseTime is 2023-11-14T22:13:20Z.
	for _, ts := range []string{"2023-11-15T03:13:20.000+05:00", "2023-11-14T17:13:20.000-05:00", "2023-11-14T22:13:20Z"} {
		tr, err := decode(ts)
		if err != nil {
			t.Fatalf("%s: %v", ts, err)
		}
		if tr.CreatedAt.Location() != time.UTC {
			t.Errorf("%s: not converted to UTC", ts)
		}
		if _, err := VerifyChain(Pin{Pub: keys[0].Public()}, []Transition{tr}); err != nil {
			t.Errorf("%s: %v", ts, err)
		}
	}
	for _, ts := range []string{"2023-11-14T22:13:20.0005Z", "2023-11-14 22:13:20", "1700000000", ""} {
		if _, err := decode(ts); err == nil {
			t.Errorf("%q accepted", ts)
		}
	}
	m["created_at"] = float64(1700000000)
	b, _ := json.Marshal(m)
	var tr Transition
	if err := json.Unmarshal(b, &tr); err == nil {
		t.Error("integer created_at accepted")
	}
}

// The signed layout is seq || old_pub || new_pub || created_at_ms (80 bytes).
func TestTransitionSignedDataLayout(t *testing.T) {
	tr := Transition{Seq: 2, OldPub: bytes.Repeat([]byte{1}, 32), NewPub: bytes.Repeat([]byte{2}, 32),
		CreatedAt: time.UnixMilli(1700000000123).UTC()}
	d := tr.signedData()
	if len(d) != 80 {
		t.Fatalf("len %d", len(d))
	}
	if binary.BigEndian.Uint64(d[:8]) != 2 || binary.BigEndian.Uint64(d[72:]) != 1700000000123 {
		t.Fatalf("%x", d)
	}
}
