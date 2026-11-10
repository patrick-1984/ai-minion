package local

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync/atomic"

	"golang.org/x/crypto/argon2"
)

// The Argon2id parameters of AD-10: 64 MiB, 3 iterations, parallelism 2.
const (
	// MemoryKiB is the memory cost in KiB (64 MiB).
	MemoryKiB uint32 = 64 * 1024
	// Iterations is the time cost.
	Iterations uint32 = 3
	// Parallelism is the number of lanes.
	Parallelism uint8 = 2
	// SaltLen is the salt length in bytes.
	SaltLen = 16
	// KeyLen is the derived key length in bytes.
	KeyLen uint32 = 32

	// MaxPasswordLen is the longest accepted password in bytes. Longer input
	// is refused before it reaches the hash function.
	MaxPasswordLen = 1024
)

// Bounds applied to the parameters found in a stored hash, so that a damaged
// or malicious row cannot make a login allocate gigabytes.
const (
	maxStoredMemoryKiB   = 1 << 20 // 1 GiB
	maxStoredIterations  = 16
	maxStoredParallelism = 16
)

// maxConcurrentHashes bounds how many Argon2id computations run at once. Each
// holds 64 MiB, so an unbounded burst of logins would exhaust memory.
const maxConcurrentHashes = 4

// ErrPasswordTooLong is returned for a password longer than [MaxPasswordLen].
var ErrPasswordTooLong = errors.New("local: password is too long")

// ErrBadHash is returned by [Hasher.Verify] for a stored value that is not a
// valid Argon2id PHC string. The error never contains the value.
var ErrBadHash = errors.New("local: stored password hash is malformed")

// Params are the Argon2id cost parameters.
type Params struct {
	MemoryKiB   uint32
	Iterations  uint32
	Parallelism uint8
}

// DefaultParams returns the parameters of AD-10.
func DefaultParams() Params {
	return Params{MemoryKiB: MemoryKiB, Iterations: Iterations, Parallelism: Parallelism}
}

// Hasher hashes and verifies passwords with Argon2id. It is safe for
// concurrent use; it limits the number of computations running at once.
type Hasher struct {
	params Params
	slots  chan struct{}
	rand   func([]byte) error
	// computations counts the Argon2id computations this Hasher has run. Tests
	// use it to prove that two code paths do the same work.
	computations atomic.Int64
}

// compute runs one Argon2id computation and counts it.
func (h *Hasher) compute(password, salt []byte, p Params, keyLen uint32) []byte {
	h.computations.Add(1)
	return argon2.IDKey(password, salt, p.Iterations, p.MemoryKiB, p.Parallelism, keyLen)
}

// HasherOption changes how [NewHasher] builds a [Hasher]. They exist for tests.
type HasherOption func(*Hasher)

// WithParams replaces the cost parameters. Production code never does.
func WithParams(p Params) HasherOption { return func(h *Hasher) { h.params = p } }

// WithSaltSource replaces the source of salts.
func WithSaltSource(f func([]byte) error) HasherOption { return func(h *Hasher) { h.rand = f } }

// NewHasher returns a Hasher with the AD-10 parameters.
func NewHasher(opts ...HasherOption) *Hasher {
	h := &Hasher{
		params: DefaultParams(),
		slots:  make(chan struct{}, maxConcurrentHashes),
		rand:   func(b []byte) error { _, err := rand.Read(b); return err },
	}
	for _, o := range opts {
		o(h)
	}
	return h
}

// Params returns the cost parameters used for new hashes.
func (h *Hasher) Params() Params { return h.params }

func (h *Hasher) acquire(ctx context.Context) error {
	select {
	case h.slots <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (h *Hasher) release() { <-h.slots }

// Hash returns the PHC string of password with a fresh random salt:
//
//	$argon2id$v=19$m=65536,t=3,p=2$<salt>$<key>
func (h *Hasher) Hash(ctx context.Context, password string) (string, error) {
	if len(password) > MaxPasswordLen {
		return "", ErrPasswordTooLong
	}
	salt := make([]byte, SaltLen)
	if err := h.rand(salt); err != nil {
		return "", fmt.Errorf("local: read random salt: %w", err)
	}
	if err := h.acquire(ctx); err != nil {
		return "", err
	}
	defer h.release()
	key := h.compute([]byte(password), salt, h.params, KeyLen)
	return encode(h.params, salt, key), nil
}

// Verify reports whether password matches the PHC string encoded. The
// comparison is constant-time. needsRehash is true when the stored hash uses
// weaker parameters than the Hasher's current ones. A malformed encoded value
// returns [ErrBadHash]; a wrong password is (false, false, nil).
//
// A password longer than [MaxPasswordLen] never matches, but it still costs
// exactly one Argon2id computation with the stored parameters, so that the
// response time of a login does not tell an existing account from an unknown
// one (which [Hasher.Burn] covers).
func (h *Hasher) Verify(ctx context.Context, password, encoded string) (ok, needsRehash bool, err error) {
	p, salt, want, err := decode(encoded)
	if err != nil {
		return false, false, err
	}
	if err := h.acquire(ctx); err != nil {
		return false, false, err
	}
	defer h.release()
	if len(password) > MaxPasswordLen {
		_ = h.compute(nil, salt, p, uint32(len(want)))
		return false, false, nil
	}
	got := h.compute([]byte(password), salt, p, uint32(len(want)))
	if subtle.ConstantTimeCompare(got, want) != 1 {
		return false, false, nil
	}
	return true, p != h.params, nil
}

// Burn spends the time of one verification without a stored hash. A login for
// an unknown account calls it so that response time does not tell whether the
// account exists.
func (h *Hasher) Burn(ctx context.Context, password string) error {
	if len(password) > MaxPasswordLen {
		password = ""
	}
	if err := h.acquire(ctx); err != nil {
		return err
	}
	defer h.release()
	salt := make([]byte, SaltLen)
	_ = h.compute([]byte(password), salt, h.params, KeyLen)
	return nil
}

func encode(p Params, salt, key []byte) string {
	b64 := base64.RawStdEncoding
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, p.MemoryKiB, p.Iterations, p.Parallelism, b64.EncodeToString(salt), b64.EncodeToString(key))
}

// decode parses a PHC Argon2id string and range-checks its parameters.
func decode(encoded string) (p Params, salt, key []byte, err error) {
	parts := strings.Split(encoded, "$")
	// "", "argon2id", "v=19", "m=..,t=..,p=..", salt, key
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" {
		return p, nil, nil, ErrBadHash
	}
	if parts[2] != "v="+strconv.Itoa(argon2.Version) {
		return p, nil, nil, ErrBadHash
	}
	var m, t, par uint64
	fields := strings.Split(parts[3], ",")
	if len(fields) != 3 {
		return p, nil, nil, ErrBadHash
	}
	for i, f := range fields {
		name, val, ok := strings.Cut(f, "=")
		if !ok || name != string("mtp"[i]) {
			return p, nil, nil, ErrBadHash
		}
		n, perr := strconv.ParseUint(val, 10, 32)
		if perr != nil {
			return p, nil, nil, ErrBadHash
		}
		switch i {
		case 0:
			m = n
		case 1:
			t = n
		case 2:
			par = n
		}
	}
	if m < 8 || m > maxStoredMemoryKiB || t < 1 || t > maxStoredIterations || par < 1 || par > maxStoredParallelism {
		return p, nil, nil, ErrBadHash
	}
	b64 := base64.RawStdEncoding
	salt, err = b64.DecodeString(parts[4])
	if err != nil || len(salt) < 8 {
		return p, nil, nil, ErrBadHash
	}
	key, err = b64.DecodeString(parts[5])
	if err != nil || len(key) < 16 || len(key) > 64 {
		return p, nil, nil, ErrBadHash
	}
	return Params{MemoryKiB: uint32(m), Iterations: uint32(t), Parallelism: uint8(par)}, salt, key, nil
}

// generatedPasswordBytes is the entropy of a generated password (144 bits).
const generatedPasswordBytes = 18

// GeneratePassword returns a random password of 24 URL-safe characters read
// from r (crypto/rand when r is nil).
func GeneratePassword(r io.Reader) (string, error) {
	if r == nil {
		r = rand.Reader
	}
	b := make([]byte, generatedPasswordBytes)
	if _, err := io.ReadFull(r, b); err != nil {
		return "", fmt.Errorf("local: generate password: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
