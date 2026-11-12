package identity

import (
	"bytes"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRootSignerHubHelpers(t *testing.T) {
	seed, err := NewRootSeed()
	if err != nil {
		t.Fatal(err)
	}
	s, err := RootSignerFromSeed(seed)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Destroy()
	k, _ := KeyFromSeed(seed)
	if !bytes.Equal(s.Public(), k.Public()) {
		t.Fatal("public key differs from KeyFromSeed")
	}
	if _, err := RootSignerFromSeed(seed[:5]); err == nil {
		t.Fatal("short seed accepted")
	}
	// A transition signed by the signer is the same format as NewTransition.
	next, _ := GenerateKey()
	at := time.UnixMilli(1_700_000_000_123)
	tr, err := NewTransitionBy(s, next.Public(), 1, at)
	if err != nil || tr.Verify() != nil {
		t.Fatalf("transition: %v %v", err, tr.Verify())
	}
	want, _ := NewTransition(k, next.Public(), 1, at)
	if !bytes.Equal(tr.SigByOld, want.SigByOld) {
		t.Fatal("transition differs from NewTransition")
	}
	// ReadKeyFileSeed reads a 0.6 identity.key.
	path := filepath.Join(t.TempDir(), "identity.key")
	if err := SaveKey(path, k); err != nil {
		t.Fatal(err)
	}
	got, err := ReadKeyFileSeed(path)
	if err != nil || !bytes.Equal(got, seed) {
		t.Fatalf("ReadKeyFileSeed = %x, %v", got, err)
	}
	// Destroy makes Seal fail.
	s.Destroy()
	if _, err := s.Seal("a passphrase of enough length"); err == nil {
		t.Fatal("Seal after Destroy")
	}
}

func TestPlainKeyPublicUsesADestroyedSignerAndRejectsBadFiles(t *testing.T) {
	dir := t.TempDir()
	k, _ := GenerateKey()
	path := filepath.Join(dir, "identity.key")
	if err := SaveKey(path, k); err != nil {
		t.Fatal(err)
	}
	var seen *RootSigner
	pub, err := PlainKeyPublic(path, func(s *RootSigner) { seen = s })
	if err != nil || !bytes.Equal(pub, k.Public()) {
		t.Fatalf("PlainKeyPublic = %x, %v", pub, err)
	}
	if seen == nil {
		t.Fatal("no signer was used")
	}
	if _, err := seen.Sign(PurposeHubCert, []byte("x")); !errors.Is(err, ErrSignerDestroyed) {
		t.Fatalf("the temporary signer was not destroyed: %v", err)
	}
	enc := EncodeB64(k.key().Seed())
	for name, content := range map[string]string{
		"no header":      enc + "\n",
		"short":          keyFileHeader + enc[:40] + "\n",
		"long":           keyFileHeader + enc + "AAAA\n",
		"inner newline":  keyFileHeader + enc[:20] + "\n" + enc[20:] + "\n",
		"padding":        keyFileHeader + enc + "=\n",
		"standard alpha": keyFileHeader + strings.NewReplacer("-", "+", "_", "/").Replace(enc) + "A\n",
		"huge":           keyFileHeader + strings.Repeat("A", 5000),
	} {
		p := filepath.Join(dir, "bad-"+strings.ReplaceAll(name, " ", "-"))
		if err := WritePrivate(p, []byte(content)); err != nil {
			t.Fatal(err)
		}
		if seed, err := ReadKeyFileSeed(p); err == nil {
			t.Errorf("%s: accepted %x", name, seed)
		}
		if _, err := PlainKeyPublic(p, nil); err == nil {
			t.Errorf("%s: PlainKeyPublic accepted", name)
		}
	}
	// A file without a trailing newline and with surrounding space is accepted as before.
	p := filepath.Join(dir, "spaced")
	if err := WritePrivate(p, []byte(" \n"+keyFileHeader+enc+" \r\n")); err != nil {
		t.Fatal(err)
	}
	if seed, err := ReadKeyFileSeed(p); err != nil || !bytes.Equal(seed, k.key().Seed()) {
		t.Fatalf("spaced: %v", err)
	}
}
