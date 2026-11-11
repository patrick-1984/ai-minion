package identity

import (
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestAPIKey(t *testing.T) {
	k := NewAPIKey()
	s := k.String()
	rest, ok := strings.CutPrefix(s, "mn_")
	if !ok || len(rest) != 52 || rest != strings.ToLower(rest) {
		t.Fatalf("bad format %q", s)
	}
	if k.Hint() != rest[:8] || len(k.Hint()) != 8 {
		t.Fatalf("hint %q", k.Hint())
	}
	if k.Hash() != sha256.Sum256([]byte(s)) {
		t.Fatal("hash is not SHA-256 of the key text")
	}
	p, err := ParseAPIKey(s)
	if err != nil || p != k {
		t.Fatalf("%v %v", p, err)
	}
	if NewAPIKey() == k {
		t.Fatal("keys repeat")
	}
}

func TestAPIKeyGolden(t *testing.T) {
	b := make([]byte, 32)
	for i := range b {
		b[i] = byte(i)
	}
	// base32 of bytes 0..31 (RFC 4648, lowercase, no padding)
	k := apiKeyFromBytes(b)
	if len(k) != 3+52 {
		t.Fatalf("len %d", len(k))
	}
	if string(k) != "mn_aaaqeayeaudaocajbifqydiob4ibceqtcqkrmfyydenbwha5dypq" {
		t.Fatalf("got %s", k)
	}
	if k.Hint() != "aaaqeaye" {
		t.Fatalf("hint %s", k.Hint())
	}
}

func TestParseAPIKeyRejects(t *testing.T) {
	good := string(NewAPIKey())
	cases := map[string]string{
		"empty":         "",
		"no prefix":     good[3:],
		"wrong prefix":  "mnk_" + good[3:],
		"short":         good[:len(good)-1],
		"long":          good + "a",
		"uppercase":     strings.ToUpper(good),
		"bad alphabet":  good[:len(good)-1] + "1",
		"non-canonical": good[:len(good)-1] + "b",
	}
	for name, in := range cases {
		if _, err := ParseAPIKey(in); !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

// TestReadAPIKeyFileOfAPrivateFile runs on every platform (Windows included):
// a key file written with WritePrivate is accepted, and a rotation is read at
// once.
func TestReadAPIKeyFileOfAPrivateFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "call.key")
	k1, k2 := NewAPIKey(), NewAPIKey()
	for _, k := range []APIKey{k1, k2} {
		if err := WritePrivate(p, []byte(k.String()+"\n")); err != nil {
			t.Fatal(err)
		}
		if got, err := ReadAPIKeyFile(p); err != nil || got != k {
			t.Fatalf("ReadAPIKeyFile = %q, %v, want the key just written", got, err)
		}
	}
}

func TestReadAPIKeyFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the Windows private-file check needs a protected DACL; see perm_windows_test.go")
	}
	dir := t.TempDir()
	write := func(name, content string, mode os.FileMode) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
		return p
	}
	key := NewAPIKey()
	for _, content := range []string{key.String(), key.String() + "\n", key.String() + "\r\n"} {
		got, err := ReadAPIKeyFile(write("ok", content, 0o600))
		if err != nil || got != key {
			t.Fatalf("%q: %v, %v", content, got, err)
		}
	}
	// A rotated key is picked up by the next read.
	other := NewAPIKey()
	p := write("rot", key.String(), 0o600)
	_, _ = ReadAPIKeyFile(p)
	write("rot", other.String(), 0o600)
	if got, err := ReadAPIKeyFile(p); err != nil || got != other {
		t.Fatalf("after rotation: %v, %v", got, err)
	}
	for name, content := range map[string]string{
		"empty": "", "garbage": "not a key", "two lines": key.String() + "\n" + key.String() + "\n",
		"key plus text": key.String() + " x", "upper": strings.ToUpper(key.String()), "big": strings.Repeat("a", MaxAPIKeyFileBytes+10),
	} {
		_, err := ReadAPIKeyFile(write(name, content, 0o600))
		if err == nil {
			t.Errorf("%s was accepted", name)
		} else if strings.Contains(err.Error(), key.String()) {
			t.Errorf("%s: the error holds the key", name)
		}
	}
	if _, err := ReadAPIKeyFile(""); err == nil {
		t.Error("an empty path was accepted")
	}
	if _, err := ReadAPIKeyFile(filepath.Join(dir, "missing")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("missing file: %v", err)
	}
	if runtime.GOOS != "windows" {
		if _, err := ReadAPIKeyFile(write("open", key.String(), 0o644)); err == nil {
			t.Error("a file open to group and others was accepted")
		}
		link := filepath.Join(dir, "link")
		if err := os.Symlink(write("target", key.String(), 0o600), link); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadAPIKeyFile(link); err == nil {
			t.Error("a symbolic link was accepted")
		}
	}
}
