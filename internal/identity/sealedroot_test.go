package identity

import (
	"bytes"
	"crypto/ed25519"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"filippo.io/age"
)

const testPass = "correct horse battery"

func TestSealOpenRoundTrip(t *testing.T) {
	k, _ := GenerateKey()
	priv := k.key()
	sealed, err := SealRootKey(priv, testPass)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sealed, priv.Seed()) || bytes.Contains(sealed, []byte(testPass)) {
		t.Fatal("plaintext in sealed file")
	}
	s, err := OpenSealedRootSigner(sealed, testPass)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(s.priv, priv) || !bytes.Equal(s.Public(), k.Public()) {
		t.Fatal("key differs")
	}
	s.Destroy()
}

func TestSealedHeaderStrength(t *testing.T) {
	k, _ := GenerateKey()
	sealed, err := SealRootKey(k.key(), testPass)
	if err != nil {
		t.Fatal(err)
	}
	hdr, _, ok := bytes.Cut(sealed, []byte("\n--- "))
	if !ok {
		t.Fatal("no header MAC line")
	}
	lines := strings.Split(string(hdr), "\n")
	if lines[0] != "age-encryption.org/v1" {
		t.Fatalf("version line %q", lines[0])
	}
	var stanzas [][]string
	for _, l := range lines[1:] {
		if strings.HasPrefix(l, "-> ") {
			stanzas = append(stanzas, strings.Fields(l))
		}
	}
	if len(stanzas) != 1 {
		t.Fatalf("%d stanzas", len(stanzas))
	}
	st := stanzas[0]
	if len(st) != 4 || st[1] != "scrypt" || st[3] != "18" {
		t.Fatalf("stanza %q, want scrypt <salt> 18", st)
	}
}

func TestSealPassphraseLength(t *testing.T) {
	k, _ := GenerateKey()
	short := "elf12345678" + "\u00e9" + "\u00e9" // 13 runes, 15 bytes
	if _, err := SealRootKey(k.key(), short); !errors.Is(err, ErrPassphraseTooShort) {
		t.Fatalf("13 runes: %v", err)
	}
	if _, err := SealRootKey(k.key(), ""); !errors.Is(err, ErrPassphraseTooShort) {
		t.Fatal("empty")
	}
	if _, err := SealRootKey(k.key(), short+"\u00e9"); err != nil {
		t.Fatalf("14 runes: %v", err)
	}
	sealed, _ := SealRootKey(k.key(), testPass)
	if _, err := OpenSealedRootSigner(sealed, "short"); !errors.Is(err, ErrPassphraseTooShort) {
		t.Fatal("open short")
	}
	if _, err := SealRootKey(k.key()[:5], testPass); !errors.Is(err, ErrInvalidArgument) {
		t.Fatal("bad key")
	}
}

func TestOpenWrongPassphraseAndGarbage(t *testing.T) {
	k, _ := GenerateKey()
	sealed, _ := SealRootKey(k.key(), testPass)
	_, err := OpenSealedRootSigner(sealed, "another passphrase")
	if !errors.Is(err, ErrWrongPassphrase) {
		t.Fatal(err)
	}
	if strings.Contains(err.Error(), "age") || strings.Contains(err.Error(), testPass) {
		t.Fatal("error reveals detail")
	}
	for name, b := range map[string][]byte{"empty": nil, "garbage": []byte("not an age file at all"), "huge": make([]byte, maxSealedRootFile+1)} {
		if _, err := OpenSealedRootSigner(b, testPass); !errors.Is(err, ErrSealedRootInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// Authenticated plaintexts of the wrong size, oversized ones starting
	// with a valid seed; only exactly 32 bytes is accepted. The header is
	// valid, so the derivation ran: ErrSealedRootPayload (which matches
	// ErrSealedRootInvalid too).
	seed := k.key().Seed()
	for _, n := range []int{0, 5, 33} {
		pt := make([]byte, n)
		copy(pt, seed)
		got, err := OpenSealedRootSigner(sealPlain(t, pt, SealedRootWorkFactor), testPass)
		if !errors.Is(err, ErrSealedRootInvalid) || !errors.Is(err, ErrSealedRootPayload) || got != nil {
			t.Errorf("%d bytes: %v", n, err)
		}
	}
	if got, err := OpenSealedRootSigner(sealPlain(t, seed, SealedRootWorkFactor), testPass); err != nil || !bytes.Equal(got.priv, k.key()) {
		t.Fatalf("32 bytes: %v", err)
	}
}

// Only the one header the sealer writes is accepted, and everything else is
// refused before the derivation (so quickly), including every lower factor.
func TestOpenRequiresExactlyOneScryptRecipientAtFactor18(t *testing.T) {
	seed := make([]byte, 32)
	quick := func(name string, file []byte) {
		t.Helper()
		start := time.Now()
		_, err := OpenSealedRootSigner(file, testPass)
		if !errors.Is(err, ErrSealedRootInvalid) || errors.Is(err, ErrSealedRootPayload) || errors.Is(err, ErrWrongPassphrase) {
			t.Errorf("%s: %v, want a refusal before the derivation", name, err)
		}
		if d := time.Since(start); d > 400*time.Millisecond {
			t.Errorf("%s took %v: a derivation ran", name, d)
		}
	}
	for _, wf := range []int{1, 10, 17} {
		quick("factor "+strconv.Itoa(wf), sealPlain(t, seed, wf))
	}
	// A non-scrypt file, and a header with the scrypt stanza twice (age itself
	// cannot write one; the MAC no longer fits, which only a derivation would find).
	x, _ := age.GenerateX25519Identity()
	var xbuf bytes.Buffer
	xw, err := age.Encrypt(&xbuf, x.Recipient())
	if err != nil {
		t.Fatal(err)
	}
	_, _ = xw.Write(seed)
	_ = xw.Close()
	quick("x25519 only", xbuf.Bytes())
	f0 := sealPlain(t, seed, SealedRootWorkFactor)
	i := bytes.Index(f0, []byte("-> scrypt"))
	j := bytes.Index(f0, []byte("---"))
	if i < 0 || j < i {
		t.Fatal("unexpected header layout")
	}
	two := append(append(append([]byte{}, f0[:j]...), f0[i:j]...), f0[j:]...)
	quick("two scrypt recipients", two)
	// A non-canonical work factor and a broken salt in an otherwise valid header.
	f := sealPlain(t, seed, 10)
	quick("factor 018", bytes.Replace(f, []byte(" 10\n"), []byte(" 018\n"), 1))
	quick("factor 19", bytes.Replace(f, []byte(" 10\n"), []byte(" 19\n"), 1))
	quick("empty", nil)
	quick("garbage", []byte("not an age file at all"))
	// A matching file at factor 18 is accepted (one real derivation).
	if _, err := OpenSealedRootSigner(sealPlain(t, seed, SealedRootWorkFactor), testPass); err != nil {
		t.Fatalf("factor 18: %v", err)
	}
}

// A damaged payload under a valid header is detected after the derivation.
func TestOpenPayloadDamageIsAfterTheDerivation(t *testing.T) {
	k, _ := GenerateKey()
	sealed, err := SealRootKey(k.key(), testPass)
	if err != nil {
		t.Fatal(err)
	}
	bad := bytes.Clone(sealed)
	bad[len(bad)-1] ^= 0xff
	if _, err := OpenSealedRootSigner(bad, testPass); !errors.Is(err, ErrSealedRootPayload) {
		t.Fatalf("damaged payload: %v", err)
	}
	// A wrong passphrase is still the wrong-passphrase answer.
	if _, err := OpenSealedRootSigner(sealed, "another passphrase"); !errors.Is(err, ErrWrongPassphrase) || errors.Is(err, ErrSealedRootPayload) {
		t.Fatalf("wrong passphrase: %v", err)
	}
}

func sealPlain(t *testing.T, pt []byte, wf int) []byte {
	t.Helper()
	rcp, err := age.NewScryptRecipient(testPass)
	if err != nil {
		t.Fatal(err)
	}
	rcp.SetWorkFactor(wf)
	var buf bytes.Buffer
	w, err := age.Encrypt(&buf, rcp)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(pt); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestSealedRootFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "root.key")
	k, _ := GenerateKey()
	if err := SaveSealedRootKey(path, k.key(), testPass); err != nil {
		t.Fatal(err)
	}
	got, err := LoadSealedRootSigner(path, testPass)
	if err != nil || got == nil || !bytes.Equal(got.priv, k.key()) {
		t.Fatal(err)
	}
	if _, err := LoadSealedRootSigner(path, "wrong passphrase!"); !errors.Is(err, ErrWrongPassphrase) {
		t.Fatal(err)
	}
	if err := SaveSealedRootKey(filepath.Join(dir, "x"), k.key(), "short"); !errors.Is(err, ErrPassphraseTooShort) {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "x")); err == nil {
		t.Fatal("file written despite error")
	}
	if _, err := LoadSealedRootSigner(filepath.Join(dir, "missing"), testPass); err == nil {
		t.Fatal("missing file")
	}
}

func TestRootSignerSignVerify(t *testing.T) {
	k, _ := GenerateKey()
	sealed, err := SealRootKey(k.key(), testPass)
	if err != nil {
		t.Fatal(err)
	}
	s, err := OpenSealedRootSigner(sealed, testPass)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Destroy()
	sig, err := s.Sign(PurposeRevoke, []byte("data"))
	if err != nil {
		t.Fatal(err)
	}
	if err := Verify(s.Public(), PurposeRevoke, []byte("data"), sig); err != nil {
		t.Fatal(err)
	}
	want, _ := k.Sign(PurposeRevoke, []byte("data"))
	if !bytes.Equal(sig, want) {
		t.Fatal("differs from identity.Sign")
	}
	if _, err := s.Sign(Purpose("bogus"), nil); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("purpose: %v", err)
	}
	pub := s.Public()
	pub[0] ^= 1
	if bytes.Equal(pub, s.Public()) {
		t.Fatal("Public aliases internal state")
	}
}

func TestRootSignerDestroy(t *testing.T) {
	k, _ := GenerateKey()
	sealed, _ := SealRootKey(k.key(), testPass)
	s, err := OpenSealedRootSigner(sealed, testPass)
	if err != nil {
		t.Fatal(err)
	}
	priv := s.priv // keep the slice header to inspect the storage
	pub := s.Public()
	s.Destroy()
	if len(priv) != ed25519.PrivateKeySize || !bytes.Equal(priv, make([]byte, ed25519.PrivateKeySize)) {
		t.Fatal("private key bytes not zero after Destroy")
	}
	if _, err := s.Sign(PurposeRevoke, []byte("x")); !errors.Is(err, ErrSignerDestroyed) {
		t.Fatalf("Sign after Destroy: %v", err)
	}
	if _, err := s.IssueHubCert(make([]byte, 32), 1, 0, 10); !errors.Is(err, ErrSignerDestroyed) {
		t.Fatalf("IssueHubCert after Destroy: %v", err)
	}
	s.Destroy() // idempotent
	if !bytes.Equal(s.Public(), pub) {
		t.Fatal("public key lost after Destroy")
	}
}

func TestRootSignerHubCertVectors(t *testing.T) {
	n := 0
	for _, v := range loadCertVectors(t) {
		if v.Expect != "ok" || v.Payload == "" {
			continue
		}
		n++
		t.Run(v.Name, func(t *testing.T) {
			p, _ := hex.DecodeString(v.Payload)
			sealed, err := SealRootKey(seedKey(t, v.RootSeed), testPass)
			if err != nil {
				t.Fatal(err)
			}
			s, err := OpenSealedRootSigner(sealed, testPass)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Destroy()
			c, err := s.IssueHubCert(p[33:65], binary.BigEndian.Uint64(p[65:73]),
				int64(binary.BigEndian.Uint64(p[73:81])), int64(binary.BigEndian.Uint64(p[81:89])))
			if err != nil {
				t.Fatal(err)
			}
			if c.Encode() != v.Cert || hex.EncodeToString(c.Payload()) != v.Payload || hex.EncodeToString(c.Signature) != v.Signature {
				t.Fatal("certificate differs from vector")
			}
		})
	}
	if n == 0 {
		t.Fatal("no vectors")
	}
}
