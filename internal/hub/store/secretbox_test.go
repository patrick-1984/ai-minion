package store

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
)

func testBox(t *testing.T) *SecretBox {
	t.Helper()
	b, err := NewSecretBox(bytes.Repeat([]byte{7}, KeySize))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestSecretBoxRoundTrip(t *testing.T) {
	b := testBox(t)
	for _, pt := range [][]byte{[]byte("hunter2"), {}, bytes.Repeat([]byte{0xAB}, 10_000)} {
		sealed, err := b.Seal("users", "totp", "row-1", pt)
		if err != nil {
			t.Fatal(err)
		}
		got, err := b.Open("users", "totp", "row-1", sealed)
		if err != nil || !bytes.Equal(got, pt) {
			t.Fatalf("round trip of %d bytes: %v", len(pt), err)
		}
	}
}

func TestSecretBoxLayoutIsNoncePlusCiphertextPlusTag(t *testing.T) {
	b := testBox(t)
	sealed, err := b.Seal("t", "c", "r", []byte("12345"))
	if err != nil {
		t.Fatal(err)
	}
	if len(sealed) != 12+5+16 {
		t.Fatalf("sealed length = %d, want nonce 12 + data 5 + tag 16", len(sealed))
	}
	if bytes.Contains(sealed, []byte("12345")) {
		t.Fatal("plaintext visible in sealed value")
	}
}

func TestSecretBoxNonceIsRandomPerSeal(t *testing.T) {
	b := testBox(t)
	seen := map[string]bool{}
	for range 200 {
		sealed, err := b.Seal("t", "c", "r", []byte("same"))
		if err != nil {
			t.Fatal(err)
		}
		n := string(sealed[:12])
		if seen[n] {
			t.Fatal("nonce reused")
		}
		seen[n] = true
	}
}

func TestSecretBoxAADBindsTableColumnRow(t *testing.T) {
	b := testBox(t)
	sealed, err := b.Seal("users", "secret", "row-1", []byte("v"))
	if err != nil {
		t.Fatal(err)
	}
	wrong := [][3]string{
		{"other", "secret", "row-1"},
		{"users", "other", "row-1"},
		{"users", "secret", "row-2"},
		{"users/secret", "", "row-1"}, // same concatenation is not enough
		{"users", "secret/row-1", ""}, // ... however the parts are cut
	}
	for _, w := range wrong {
		if _, err := b.Open(w[0], w[1], w[2], sealed); !errors.Is(err, ErrDecrypt) {
			t.Errorf("Open%v = %v, want ErrDecrypt", w, err)
		}
	}
	if _, err := b.Open("users", "secret", "row-1", sealed); err != nil {
		t.Fatal(err)
	}
}

func TestSecretBoxRejectsTamperingTruncationAndWrongKey(t *testing.T) {
	b := testBox(t)
	sealed, _ := b.Seal("t", "c", "r", []byte("value"))
	for i := range sealed {
		mod := bytes.Clone(sealed)
		mod[i] ^= 1
		if _, err := b.Open("t", "c", "r", mod); !errors.Is(err, ErrDecrypt) {
			t.Fatalf("flip at byte %d accepted", i)
		}
	}
	for _, short := range [][]byte{nil, {}, sealed[:11], sealed[:27]} {
		if _, err := b.Open("t", "c", "r", short); !errors.Is(err, ErrDecrypt) {
			t.Fatalf("short value of %d bytes accepted", len(short))
		}
	}
	other, _ := NewSecretBox(bytes.Repeat([]byte{8}, KeySize))
	if _, err := other.Open("t", "c", "r", sealed); !errors.Is(err, ErrDecrypt) {
		t.Fatal("wrong key accepted")
	}
}

func TestNewSecretBoxKeySize(t *testing.T) {
	for _, n := range []int{0, 16, 31, 33, 64} {
		if _, err := NewSecretBox(make([]byte, n)); err == nil {
			t.Errorf("%d-byte key accepted", n)
		}
	}
}

func TestLoadOrCreateKeyCreatesOwnerOnlyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", DataKeyFile)
	key, err := LoadOrCreateKey(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(key) != KeySize || bytes.Equal(key, make([]byte, KeySize)) {
		t.Fatalf("key = %x", key)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && st.Mode().Perm() != 0o600 {
		t.Fatalf("key file mode = %v, want 0600", st.Mode().Perm())
	}
	again, err := LoadOrCreateKey(path)
	if err != nil || !bytes.Equal(again, key) {
		t.Fatalf("second load differs: %v", err)
	}
	if ents, _ := os.ReadDir(filepath.Dir(path)); len(ents) != 1 {
		t.Fatalf("directory holds %d entries, want only the key file", len(ents))
	}
}

func TestLoadOrCreateKeyNeverOverwritesBadFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), DataKeyFile)
	if err := os.WriteFile(path, []byte("short"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreateKey(path); err == nil {
		t.Fatal("bad key file accepted")
	}
	if b, _ := os.ReadFile(path); string(b) != "short" {
		t.Fatalf("bad key file was overwritten: %q", b)
	}
	// A file that is too long is refused too.
	if err := os.WriteFile(path, make([]byte, KeySize+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreateKey(path); err == nil {
		t.Fatal("oversized key file accepted")
	}
}

func TestLoadOrCreateKeyConcurrentStartersAgree(t *testing.T) {
	path := filepath.Join(t.TempDir(), DataKeyFile)
	const n = 8
	keys := make([][]byte, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			keys[i], errs[i] = LoadOrCreateKey(path)
		}()
	}
	wg.Wait()
	final, err := LoadOrCreateKey(path)
	if err != nil {
		t.Fatal(err)
	}
	for i := range n {
		if errs[i] != nil {
			t.Fatalf("starter %d: %v", i, errs[i])
		}
		if !bytes.Equal(keys[i], final) {
			t.Fatalf("starter %d got a different key", i)
		}
	}
}

func TestSecretBoxDeriveIsStablePurposeSeparatedAndHidesTheKey(t *testing.T) {
	keyA := bytes.Repeat([]byte{7}, KeySize)
	a1, err := NewSecretBox(keyA)
	if err != nil {
		t.Fatal(err)
	}
	a2, _ := NewSecretBox(keyA) // a second box on the same key: another process after a restart
	other, _ := NewSecretBox(bytes.Repeat([]byte{8}, KeySize))

	d := a1.Derive("inspector-context")
	if len(d) != 32 {
		t.Fatalf("derived key has %d bytes, want 32", len(d))
	}
	if !bytes.Equal(d, a2.Derive("inspector-context")) {
		t.Error("the same data key and purpose gave different keys: a restart would change what is derived")
	}
	if bytes.Equal(d, a1.Derive("another-purpose")) {
		t.Error("two purposes share a derived key")
	}
	if bytes.Equal(d, other.Derive("inspector-context")) {
		t.Error("two data keys share a derived key")
	}
	if bytes.Equal(d, keyA) || bytes.Contains(d, keyA[:8]) {
		t.Error("the derived key is, or contains, the data key")
	}
	// The result is a copy: changing it changes nothing in the box.
	d[0] ^= 0xff
	if bytes.Equal(d, a1.Derive("inspector-context")) {
		t.Error("the derived key aliases the box")
	}
}
