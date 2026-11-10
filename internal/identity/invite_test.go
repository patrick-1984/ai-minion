package identity

import (
	"crypto/sha256"
	"errors"
	"strings"
	"testing"
)

func testFpr(t *testing.T) Fingerprint {
	t.Helper()
	k, err := KeyFromSeed(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	return k.Fingerprint()
}

func TestInviteKeyRoundTrip(t *testing.T) {
	fpr := testFpr(t)
	k, err := NewInviteKey(fpr)
	if err != nil {
		t.Fatal(err)
	}
	s := k.String()
	if !strings.HasPrefix(s, "mnk_"+string(fpr)+".") {
		t.Fatalf("bad format %q", s)
	}
	secretText := s[strings.Index(s, ".")+1:]
	if len(secretText) != 52 || secretText != strings.ToLower(secretText) {
		t.Fatalf("secret text %q", secretText)
	}
	if len(k.Secret) != 32 {
		t.Fatalf("secret len %d", len(k.Secret))
	}
	p, err := ParseInviteKey(s)
	if err != nil {
		t.Fatal(err)
	}
	if p.Fingerprint != fpr || string(p.Secret) != string(k.Secret) {
		t.Fatal("round trip mismatch")
	}
	if p.SecretHash() != sha256.Sum256(k.Secret) {
		t.Fatal("SecretHash is not SHA-256 of the secret bytes")
	}
	k2, _ := NewInviteKey(fpr)
	if string(k2.Secret) == string(k.Secret) {
		t.Fatal("secrets repeat")
	}
}

func TestInviteKeyUppercaseFingerprintAccepted(t *testing.T) {
	fpr := testFpr(t)
	k, _ := NewInviteKey(fpr)
	s := "mnk_" + strings.ToUpper(string(fpr)) + "." + encodeB32(k.Secret)
	p, err := ParseInviteKey(s)
	if err != nil || p.Fingerprint != fpr {
		t.Fatalf("%v %v", p, err)
	}
}

func TestNewInviteKeyRejectsBadFingerprint(t *testing.T) {
	if _, err := NewInviteKey("short"); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("err = %v", err)
	}
}

func TestParseInviteKeyRejects(t *testing.T) {
	fpr := string(testFpr(t))
	secret := strings.Repeat("a", 52)
	cases := map[string]string{
		"empty":            "",
		"wrong prefix":     "mn_" + fpr + "." + secret,
		"uppercase prefix": "MNK_" + fpr + "." + secret,
		"no dot":           "mnk_" + fpr + secret,
		"short fpr":        "mnk_" + fpr[:51] + "." + secret,
		"long fpr":         "mnk_" + fpr + "a." + secret,
		"bad fpr alphabet": "mnk_" + fpr[:51] + "1." + secret,
		"short secret":     "mnk_" + fpr + "." + secret[:51],
		"long secret":      "mnk_" + fpr + "." + secret + "a",
		"bad alphabet":     "mnk_" + fpr + "." + secret[:51] + "8",
		"uppercase secret": "mnk_" + fpr + "." + strings.ToUpper(secret),
		"padding":          "mnk_" + fpr + "." + secret[:51] + "=",
		"extra dot":        "mnk_" + fpr + "." + secret + ".a",
		"empty secret":     "mnk_" + fpr + ".",
		"non-canonical":    "mnk_" + fpr + "." + secret[:51] + "b",
	}
	for name, in := range cases {
		_, err := ParseInviteKey(in)
		if !errors.Is(err, ErrInviteInvalid) {
			t.Errorf("%s: err = %v", name, err)
		}
		if err != nil && in != "" && strings.Contains(err.Error(), in) {
			t.Errorf("%s: error echoes input", name)
		}
	}
}

func FuzzParseInviteKey(f *testing.F) {
	fpr := "kzdvvj2umnduyauf35o36k6kw462mujvra46tn3uqgzovmihocga"
	f.Add("mnk_" + fpr + "." + strings.Repeat("a", 52))
	f.Add("")
	f.Add("mnk_.")
	f.Add("mnk_" + strings.ToUpper(fpr) + "." + strings.Repeat("7", 52))
	f.Fuzz(func(t *testing.T, s string) {
		k, err := ParseInviteKey(s)
		if err != nil {
			if !errors.Is(err, ErrInviteInvalid) {
				t.Fatalf("unexpected error code: %v", err)
			}
			return
		}
		// Whatever parses must re-parse to the same value from its canonical form.
		k2, err := ParseInviteKey(k.String())
		if err != nil || k2.Fingerprint != k.Fingerprint || string(k2.Secret) != string(k.Secret) {
			t.Fatalf("round trip failed for %q", s)
		}
	})
}
