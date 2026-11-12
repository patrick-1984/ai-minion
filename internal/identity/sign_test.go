package identity

import (
	"bytes"
	"crypto/ed25519"
	"errors"
	"testing"
)

var allPurposes = []Purpose{PurposeHandshakeHub, PurposeHandshakeWorker, PurposeTransition, PurposeRevoke, PurposePAKEConfirm, PurposeHubCert}

func TestSignedMessageLayout(t *testing.T) {
	got, err := SignedMessage(PurposeRevoke, []byte("abc"))
	if err != nil {
		t.Fatal(err)
	}
	want := append([]byte("minion/v1/revoke"), 0, 'a', 'b', 'c')
	if !bytes.Equal(got, want) {
		t.Fatalf("got %q", got)
	}
}

func TestPurposeNames(t *testing.T) {
	want := []string{"hs-hub", "hs-worker", "transition", "revoke", "pake-confirm", "hub-cert"}
	for i, p := range allPurposes {
		if string(p) != want[i] || !p.Valid() {
			t.Fatalf("purpose %d = %q", i, p)
		}
	}
}

func TestUnknownPurposeRejected(t *testing.T) {
	k, _ := GenerateKey()
	for _, p := range []Purpose{"", "hs", "HS-HUB", "hs-hub\x00", "other"} {
		if _, err := k.Sign(p, []byte("x")); !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("sign %q: %v", p, err)
		}
		if err := Verify(k.Public(), p, []byte("x"), make([]byte, 64)); !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("verify %q: %v", p, err)
		}
	}
}

// A signature for one purpose never verifies for another.
func TestPurposeSeparation(t *testing.T) {
	k, _ := GenerateKey()
	data := []byte("same data")
	for _, p := range allPurposes {
		sig, err := k.Sign(p, data)
		if err != nil {
			t.Fatal(err)
		}
		for _, q := range allPurposes {
			err := Verify(k.Public(), q, data, sig)
			if p == q && err != nil {
				t.Errorf("%s: own purpose failed: %v", p, err)
			}
			if p != q && !errors.Is(err, ErrUnauthenticated) {
				t.Errorf("signature for %s verified for %s: %v", p, q, err)
			}
		}
	}
}

func TestVerifyRejects(t *testing.T) {
	k, _ := GenerateKey()
	other, _ := GenerateKey()
	sig, _ := k.Sign(PurposeHandshakeHub, []byte("d"))
	if err := Verify(other.Public(), PurposeHandshakeHub, []byte("d"), sig); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("wrong key: %v", err)
	}
	if err := Verify(k.Public(), PurposeHandshakeHub, []byte("e"), sig); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("wrong data: %v", err)
	}
	if err := Verify(k.Public(), PurposeHandshakeHub, []byte("d"), sig[:63]); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("short sig: %v", err)
	}
	if err := Verify(ed25519.PublicKey(make([]byte, 5)), PurposeHandshakeHub, nil, sig); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("short key: %v", err)
	}
	if _, err := Sign(ed25519.PrivateKey(make([]byte, 5)), PurposeRevoke, nil); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("short private key: %v", err)
	}
}
