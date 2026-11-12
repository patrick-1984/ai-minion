//go:build unix

package server

import (
	"bytes"
	"errors"
	"github.com/patrick-1984/ai-minion/internal/testutil/hubstore"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/identity"
)

// generatedPair creates the Hub's own pair in a new data directory and returns
// the directory, the key path and the key bytes.
func generatedPair(t *testing.T) (data, key string, keyBytes []byte) {
	t.Helper()
	data = t.TempDir()
	if _, err := LoadCertSource(CertOptions{DataDir: data}); err != nil {
		t.Fatal(err)
	}
	key = filepath.Join(data, TLSDir, TLSKeyFile)
	b, err := os.ReadFile(key)
	if err != nil {
		t.Fatal(err)
	}
	return data, key, b
}

func mode(t *testing.T, path string) os.FileMode {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Mode().Perm()
}

func assertKeyUntouched(t *testing.T, key string, want []byte, wantMode os.FileMode) {
	t.Helper()
	got, err := os.ReadFile(key)
	if err != nil || !bytes.Equal(got, want) {
		t.Errorf("the key was changed or removed: %v", err)
	}
	if m := mode(t, key); m != wantMode {
		t.Errorf("key mode = %04o, want %04o (an unsafe key must not be repaired)", m, wantMode)
	}
}

// The generated key is created private and stays usable under a normal start.
func TestGeneratedKeyIsPrivateAndReusable(t *testing.T) {
	data, key, _ := generatedPair(t)
	if m := mode(t, key); m != 0o600 {
		t.Fatalf("generated key mode = %04o", m)
	}
	first, err := LoadCertSource(CertOptions{DataDir: data})
	if err != nil || first.Generated {
		t.Fatalf("restart: generated=%v err=%v; want the pair reused", first != nil && first.Generated, err)
	}
	// A restored key is fine when it is private, including read-only 0400.
	if err := os.Chmod(key, 0o400); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadCertSource(CertOptions{DataDir: data}); err != nil {
		t.Fatalf("a 0400 key was refused: %v", err)
	}
}

// A key that group or others can access is refused, never repaired or replaced:
// the proof of the final audit (a generated key at 0644 under traversable
// directories) ends in a refusal.
func TestExistingGeneratedKeyWithPublicModeIsRefusedUnchanged(t *testing.T) {
	for _, m := range []os.FileMode{0o644, 0o640, 0o604, 0o660, 0o666} {
		t.Run(m.String(), func(t *testing.T) {
			data, key, keyBytes := generatedPair(t)
			certBefore, _ := os.ReadFile(filepath.Join(data, TLSDir, TLSCertFile))
			for _, p := range []string{data, filepath.Dir(key)} {
				if err := os.Chmod(p, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Chmod(key, m); err != nil {
				t.Fatal(err)
			}
			_, err := LoadCertSource(CertOptions{DataDir: data})
			if !errors.Is(err, identity.ErrInvalidArgument) {
				t.Fatalf("err = %v, want a refusal", err)
			}
			if strings.Contains(err.Error(), string(keyBytes[:40])) {
				t.Error("the error contains key material")
			}
			assertKeyUntouched(t, key, keyBytes, m)
			if b, _ := os.ReadFile(filepath.Join(data, TLSDir, TLSCertFile)); !bytes.Equal(b, certBefore) {
				t.Error("the certificate was regenerated")
			}
		})
	}
}

func TestGeneratedKeyInAnUnsafeDirectoryIsRefused(t *testing.T) {
	data, key, keyBytes := generatedPair(t)
	if err := os.Chmod(filepath.Dir(key), 0o777); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadCertSource(CertOptions{DataDir: data}); !errors.Is(err, identity.ErrInvalidArgument) {
		t.Fatalf("world-writable tls directory: err = %v", err)
	}
	assertKeyUntouched(t, key, keyBytes, 0o600)

	// The data directory above it is just as able to swap the tls directory.
	if err := os.Chmod(filepath.Dir(key), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(data, 0o777); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadCertSource(CertOptions{DataDir: data}); !errors.Is(err, identity.ErrInvalidArgument) {
		t.Fatalf("world-writable data directory: err = %v", err)
	}
	if err := os.Chmod(data, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadCertSource(CertOptions{DataDir: data}); err != nil {
		t.Fatalf("a safe directory is usable again: %v", err)
	}
}

func TestGeneratedKeyThatIsALinkIsRefused(t *testing.T) {
	data, key, keyBytes := generatedPair(t)
	moved := filepath.Join(data, "elsewhere.pem")
	if err := os.Rename(key, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(moved, key); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadCertSource(CertOptions{DataDir: data}); !errors.Is(err, identity.ErrInvalidArgument) {
		t.Fatalf("err = %v", err)
	}
	if b, _ := os.ReadFile(moved); !bytes.Equal(b, keyBytes) {
		t.Error("the key changed")
	}
}

// A first start never creates the key in a directory others can write.
func TestKeyIsNotCreatedInAnUnsafeTLSDirectory(t *testing.T) {
	data := hubstore.Dir(t)
	dir := filepath.Join(data, TLSDir)
	if err := os.Mkdir(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadCertSource(CertOptions{DataDir: data}); !errors.Is(err, identity.ErrInvalidArgument) {
		t.Fatalf("err = %v", err)
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 0 {
		t.Errorf("files were created in an unsafe directory: %v", ents)
	}
}

// Reload (SIGHUP) takes the same decision: a key that became accessible stops
// the reload and the certificate in service stays.
func TestReloadRefusesAKeyThatBecamePublic(t *testing.T) {
	data, key, _ := generatedPair(t)
	cs, err := LoadCertSource(CertOptions{DataDir: data})
	if err != nil {
		t.Fatal(err)
	}
	before := cs.Fingerprint()
	if err := os.Chmod(key, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := cs.Reload(); !errors.Is(err, identity.ErrInvalidArgument) {
		t.Fatalf("Reload err = %v", err)
	}
	if cs.Fingerprint() != before {
		t.Error("the certificate in service changed after a refused reload")
	}
	if err := os.Chmod(key, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := cs.Reload(); err != nil {
		t.Fatalf("Reload with a private key: %v", err)
	}
}

// The operator's own key is held to the same rule; a public certificate is not.
func TestOperatorSuppliedKeyMustBePrivate(t *testing.T) {
	cert, key := ownPair(t, "own.example")
	data := hubstore.Dir(t)
	if err := os.Chmod(cert, 0o644); err != nil {
		t.Fatal(err)
	}
	cs, err := LoadCertSource(CertOptions{DataDir: data, CertFile: cert, KeyFile: key})
	if err != nil {
		t.Fatalf("private key and readable certificate: %v", err)
	}
	for _, m := range []os.FileMode{0o644, 0o640} {
		if err := os.Chmod(key, m); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadCertSource(CertOptions{DataDir: data, CertFile: cert, KeyFile: key}); !errors.Is(err, identity.ErrInvalidArgument) {
			t.Errorf("mode %04o: err = %v", m, err)
		}
		if err := cs.Reload(); !errors.Is(err, identity.ErrInvalidArgument) {
			t.Errorf("mode %04o: Reload err = %v", m, err)
		}
		if got := mode(t, key); got != m {
			t.Errorf("the operator's key was changed to %04o", got)
		}
	}
	if err := os.Chmod(key, 0o400); err != nil {
		t.Fatal(err)
	}
	if err := cs.Reload(); err != nil {
		t.Errorf("a read-only key (a secret mount) was refused: %v", err)
	}
}

// Certificate tooling and secret mounts publish keys through links
// (/etc/letsencrypt/live/<name>/privkey.pem -> ../../archive/<name>/privkey1.pem).
// A link in a directory only the Hub user can change, to a private key, works;
// a link anyone else could redirect does not.
func TestOperatorSuppliedKeyThroughALink(t *testing.T) {
	cert, key := ownPair(t, "own.example")
	data := hubstore.Dir(t)

	live := filepath.Join(t.TempDir(), "live")
	if err := os.Mkdir(live, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(live, "privkey.pem")
	if err := os.Symlink(key, link); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadCertSource(CertOptions{DataDir: data, CertFile: cert, KeyFile: link}); err != nil {
		t.Fatalf("a link to a private key in a trusted directory was refused: %v", err)
	}

	open := filepath.Join(t.TempDir(), "open")
	if err := os.Mkdir(open, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(open, 0o777); err != nil {
		t.Fatal(err)
	}
	bad := filepath.Join(open, "privkey.pem")
	if err := os.Symlink(key, bad); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadCertSource(CertOptions{DataDir: data, CertFile: cert, KeyFile: bad}); !errors.Is(err, identity.ErrInvalidArgument) {
		t.Fatalf("a link in a world-writable directory: err = %v", err)
	}

	// The target must itself be private.
	if err := os.Chmod(key, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadCertSource(CertOptions{DataDir: data, CertFile: cert, KeyFile: link}); !errors.Is(err, identity.ErrInvalidArgument) {
		t.Fatalf("a link to a public key: err = %v", err)
	}
}

// The parsed key is the key that was checked: a key that does not belong to
// the certificate is refused, not silently replaced.
func TestOperatorKeyThatDoesNotMatchTheCertificateIsRefused(t *testing.T) {
	cert, _ := ownPair(t, "a.example")
	_, otherKey := ownPair(t, "b.example")
	if _, err := LoadCertSource(CertOptions{DataDir: hubstore.Dir(t), CertFile: cert, KeyFile: otherKey}); err == nil {
		t.Fatal("a mismatched key was accepted")
	}
}
