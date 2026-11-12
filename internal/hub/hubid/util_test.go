package hubid

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/identity"
)

func osReadFile(dir, name string) ([]byte, error) { return os.ReadFile(filepath.Join(dir, name)) }

func osWriteFile(dir, name string, b []byte) error {
	return os.WriteFile(filepath.Join(dir, name), b, 0o600)
}

func rm(e *env, name string) {
	if err := os.Remove(filepath.Join(e.dir, name)); err != nil {
		e.t.Fatal(err)
	}
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }

func identityKey(t *testing.T) (*identity.Key, error) {
	t.Helper()
	return identity.GenerateKey()
}

func saveKey(e *env, name string, k *identity.Key) error {
	return identity.SaveKey(filepath.Join(e.dir, name), k)
}
