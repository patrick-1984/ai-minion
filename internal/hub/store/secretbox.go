package store

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/patrick-1984/ai-minion/internal/identity"
)

const (
	// KeySize is the size of the data key: AES-256.
	KeySize = 32
	// DataKeyFile is the name of the key file inside the data directory.
	DataKeyFile = "data.key"
)

// ErrDecrypt is returned by [SecretBox.Open] when a value does not
// authenticate: wrong key, wrong table, column or row, or damaged data. It
// never carries the value.
var ErrDecrypt = errors.New("store: cannot decrypt value (wrong key, wrong binding or damaged data)")

// SecretBox encrypts column values with AES-256-GCM. Every value is bound to
// its place, (table, column, row_id), through the GCM additional data, so a
// sealed value copied to another row or column does not open.
//
// The sealed form is nonce || ciphertext || tag with a random 12-byte nonce.
type SecretBox struct {
	aead cipher.AEAD
	key  []byte // private copy, used only by [SecretBox.Derive]
}

// deriveLabel separates every [SecretBox.Derive] output from the data key itself and from
// every other use of it.
const deriveLabel = "minion-derive-v1"

// Derive returns a 32-byte key for one named purpose: HMAC-SHA256 of the purpose under the
// data key. The same data key and purpose always give the same key (so a value derived from
// it stays valid across restarts of the Hub, and across a restore of the data directory),
// another purpose or another data key gives an unrelated one, and the data key cannot be
// recovered from it. It is for keyed digests that must be stable but are not secrets to be
// stored: it never encrypts anything and never replaces the sealed-column form.
func (b *SecretBox) Derive(purpose string) []byte {
	m := hmac.New(sha256.New, b.key)
	m.Write([]byte(deriveLabel))
	m.Write([]byte{0})
	m.Write([]byte(purpose))
	return m.Sum(nil)
}

// NewSecretBox returns a box for a 32-byte key.
func NewSecretBox(key []byte) (*SecretBox, error) {
	if len(key) != KeySize {
		return nil, fmt.Errorf("store: data key must be %d bytes, got %d", KeySize, len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("store: data key: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("store: data key: %w", err)
	}
	return &SecretBox{aead: aead, key: append([]byte(nil), key...)}, nil
}

// aad is the additional authenticated data "table/column/row_id".
func aad(table, column, rowID string) []byte {
	return []byte(table + "/" + column + "/" + rowID)
}

// Seal encrypts plaintext for the place (table, column, rowID).
func (b *SecretBox) Seal(table, column, rowID string, plaintext []byte) ([]byte, error) {
	nonce := make([]byte, b.aead.NonceSize(), b.aead.NonceSize()+len(plaintext)+b.aead.Overhead())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("store: random nonce: %w", err)
	}
	return b.aead.Seal(nonce, nonce, plaintext, aad(table, column, rowID)), nil
}

// Open decrypts a value sealed for the place (table, column, rowID). A
// different place, a different key or modified data fail with [ErrDecrypt].
func (b *SecretBox) Open(table, column, rowID string, sealed []byte) ([]byte, error) {
	n := b.aead.NonceSize()
	if len(sealed) < n+b.aead.Overhead() {
		return nil, ErrDecrypt
	}
	plain, err := b.aead.Open(nil, sealed[:n], sealed[n:], aad(table, column, rowID))
	if err != nil {
		return nil, ErrDecrypt
	}
	return plain, nil
}

// LoadOrCreateKey returns the data key stored in path. When the file does not
// exist it is created with 32 random bytes, private to the owner. An existing
// file must be safe to use: a regular file (not a link), owned by the user that
// runs the Hub, with no access for group or others (a protected DACL on
// Windows), in a directory that other local users cannot write. An unsafe key
// is refused with guidance and never changed: a key that other users could read
// may already have been read, so quietly tightening its mode would only hide
// that. A file of another size is an error too: it is never overwritten,
// because that would make every encrypted column unreadable.
func LoadOrCreateKey(path string) ([]byte, error) {
	if err := checkKeyDir(path); err != nil {
		return nil, err
	}
	key, err := readKey(path)
	if err == nil {
		return key, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("store: create data directory: %w", err)
	}
	if err := checkKeyDir(path); err != nil {
		return nil, err
	}
	key = make([]byte, KeySize)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		return nil, fmt.Errorf("store: random data key: %w", err)
	}
	return createKeyFile(path, key)
}

// keyGuidance is what the operator is told about a data key that is not safe.
// The key seals the OIDC client secrets and refresh tokens, so an exposed one is
// a rotation matter, not only a mode to fix.
const keyGuidance = "The data key protects the sealed OIDC client secrets and refresh tokens, so the Hub " +
	"will not start with it, and the file was not changed: a key that other users could read or replace may " +
	"already have been read, so tightening its mode would only hide that. " +
	"If only you ever had access to it (for example you just restored it yourself), fix it and start again: " +
	"make it a plain file owned by the Hub user (chown, chmod 600) in a directory that only that user can write. " +
	"If anyone else may have had access, treat the key as exposed: revoke the OIDC client secret and the refresh " +
	"tokens at your identity provider and start a new Hub data directory (the sealed values cannot be opened with another key)"

// checkKeyDir refuses a data directory that another local user could use to
// replace the key (writable by others, or owned by another user, on the path).
// A directory that does not exist yet is not checked here: it is created
// private and then checked.
func checkKeyDir(path string) error {
	err := identity.CheckPrivateDir(filepath.Dir(path))
	switch {
	case err == nil, errors.Is(err, os.ErrNotExist):
		return nil
	case errors.Is(err, identity.ErrInvalidArgument):
		return fmt.Errorf("store: the data key %q cannot be used: %w. %s", path, err, keyGuidance)
	}
	return fmt.Errorf("store: data directory of %q: %w", path, err)
}

// openDataKey opens an existing data key through [identity.OpenPrivate]: one
// open that does not follow a link, with every check made on the handle, so the
// bytes that are read are the bytes of the file that was checked.
func openDataKey(path string) (*os.File, error) {
	f, err := identity.OpenPrivate(path)
	switch {
	case err == nil:
		return f, nil
	case errors.Is(err, identity.ErrInvalidArgument):
		return nil, fmt.Errorf("store: the data key %q cannot be used: %w. %s", path, err, keyGuidance)
	}
	return nil, err
}

// createKeyFile publishes key at path without ever exposing a partial file:
// it writes a private temporary file and links it into place, which fails if
// path exists (a concurrent starter won; its key is used). On a file system
// without hard links it falls back to exclusive creation.
func createKeyFile(path string, key []byte) ([]byte, error) {
	// Private from its creation: mode 0600 on Unix, a protected owner-and-SYSTEM-only
	// DACL on Windows (os.CreateTemp would inherit the directory's ACL there, and
	// the key would then not pass the check of the next start).
	tmp, err := createPrivateTemp(filepath.Dir(path), ".data.key-")
	if err != nil {
		return nil, fmt.Errorf("store: create data key %q: %w", path, err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := tmp.Write(key); err != nil {
		_ = tmp.Close()
		return nil, fmt.Errorf("store: write data key %q: %w", path, err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return nil, fmt.Errorf("store: write data key %q: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		return nil, fmt.Errorf("store: write data key %q: %w", path, err)
	}
	err = os.Link(tmpName, path)
	switch {
	case err == nil:
		return key, nil
	case errors.Is(err, os.ErrExist):
		return readKey(path)
	}
	// No hard links here: exclusive creation instead.
	f, err := createPrivate(path)
	if errors.Is(err, os.ErrExist) {
		return readKey(path)
	}
	if err != nil {
		return nil, fmt.Errorf("store: create data key %q: %w", path, err)
	}
	if _, err := f.Write(key); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return nil, fmt.Errorf("store: write data key %q: %w", path, err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(path)
		return nil, fmt.Errorf("store: write data key %q: %w", path, err)
	}
	return key, nil
}

func readKey(path string) ([]byte, error) {
	f, err := openDataKey(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	key, err := io.ReadAll(io.LimitReader(f, KeySize+1))
	if err != nil {
		return nil, fmt.Errorf("store: read data key %q: %w", path, err)
	}
	if len(key) != KeySize {
		return nil, fmt.Errorf("store: data key %q must be exactly %d bytes; it was not changed", path, KeySize)
	}
	return key, nil
}

// createPrivateTemp creates a new private file in dir whose name starts with
// prefix (see createPrivate).
func createPrivateTemp(dir, prefix string) (*os.File, error) {
	for range 16 {
		var r [8]byte
		if _, err := io.ReadFull(rand.Reader, r[:]); err != nil {
			return nil, err
		}
		f, err := createPrivate(filepath.Join(dir, prefix+hex.EncodeToString(r[:])))
		if errors.Is(err, os.ErrExist) {
			continue
		}
		return f, err
	}
	return nil, &os.PathError{Op: "createtemp", Path: filepath.Join(dir, prefix), Err: os.ErrExist}
}
