package identity

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestKeySaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "id.key")
	k := mustKey(t)
	if err := SaveKey(path, k); err != nil {
		t.Fatal(err)
	}
	got, err := LoadKey(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got.Public(), k.Public()) || got.Fingerprint() != k.Fingerprint() {
		t.Fatal("loaded key differs")
	}
	// overwrite is allowed and atomic: no temp files remain
	if err := SaveKey(path, mustKey(t)); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("leftover files: %v", entries)
	}
}

func TestLoadKeyRejectsCorruptFiles(t *testing.T) {
	dir := t.TempDir()
	for name, content := range map[string]string{
		"empty":      "",
		"no header":  EncodeB64(make([]byte, 32)) + "\n",
		"bad base64": keyFileHeader + "!!!\n",
		"short seed": keyFileHeader + EncodeB64(make([]byte, 16)) + "\n",
	} {
		p := filepath.Join(dir, name)
		if err := WritePrivate(p, []byte(content)); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadKey(p); !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	if _, err := LoadKey(filepath.Join(dir, "missing")); !os.IsNotExist(err) {
		t.Errorf("missing: %v", err)
	}
}

func TestKeyFromSeedBadLength(t *testing.T) {
	if _, err := KeyFromSeed(make([]byte, 31)); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("err = %v", err)
	}
}

func TestKeyFileDoesNotLeakIntoErrors(t *testing.T) {
	p := filepath.Join(t.TempDir(), "k")
	seed := EncodeB64(make([]byte, 31)) // wrong length but valid base64
	_ = WritePrivate(p, []byte(keyFileHeader+seed+"\n"))
	_, err := LoadKey(p)
	if err == nil || strings.Contains(err.Error(), seed) {
		t.Fatalf("error leaks key material: %v", err)
	}
}
