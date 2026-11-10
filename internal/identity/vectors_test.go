package identity

import (
	"crypto/ed25519"
	"encoding/json"
	"os"
	"testing"
)

type vectorFile struct {
	Keys []struct {
		Name        string `json:"name"`
		Seed        B64    `json:"seed_b64"`
		Pub         B64    `json:"pub_b64"`
		Fingerprint string `json:"fingerprint"`
		Signatures  []struct {
			Purpose string `json:"purpose"`
			Data    B64    `json:"data_b64"`
			Sig     B64    `json:"sig_b64"`
		} `json:"signatures"`
	} `json:"keys"`
	Transition Transition `json:"transition"`
}

func loadVectors(t *testing.T) vectorFile {
	t.Helper()
	raw, err := os.ReadFile("testdata/vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var v vectorFile
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

// Golden vectors were generated independently with Python's cryptography
// package: the fingerprint, public key and every signature must match.
func TestGoldenVectors(t *testing.T) {
	v := loadVectors(t)
	if len(v.Keys) == 0 {
		t.Fatal("no vectors")
	}
	for _, kv := range v.Keys {
		t.Run(kv.Name, func(t *testing.T) {
			k, err := KeyFromSeed(kv.Seed)
			if err != nil {
				t.Fatal(err)
			}
			if string(k.Public()) != string(kv.Pub) {
				t.Fatal("public key mismatch")
			}
			if got := k.Fingerprint().String(); got != kv.Fingerprint {
				t.Fatalf("fingerprint = %s, want %s", got, kv.Fingerprint)
			}
			for _, s := range kv.Signatures {
				got, err := k.Sign(Purpose(s.Purpose), s.Data)
				if err != nil {
					t.Fatal(err)
				}
				if string(got) != string(s.Sig) {
					t.Fatalf("signature mismatch for %s", s.Purpose)
				}
				if err := Verify(k.Public(), Purpose(s.Purpose), s.Data, s.Sig); err != nil {
					t.Fatalf("verify %s: %v", s.Purpose, err)
				}
			}
		})
	}
}

func TestGoldenTransition(t *testing.T) {
	v := loadVectors(t)
	if err := v.Transition.Verify(); err != nil {
		t.Fatal(err)
	}
	pin, err := VerifyChain(Pin{Pub: ed25519.PublicKey(v.Transition.OldPub)}, []Transition{v.Transition})
	if err != nil {
		t.Fatal(err)
	}
	if pin.Seq != 1 || string(pin.Pub) != string(v.Transition.NewPub) {
		t.Fatalf("unexpected pin %+v", pin)
	}
}
