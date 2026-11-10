package local

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var fastParams = Params{MemoryKiB: 64, Iterations: 1, Parallelism: 1}

func fastHasher() *Hasher { return NewHasher(WithParams(fastParams)) }

func TestDefaultParamsAreTheADRule(t *testing.T) {
	p := DefaultParams()
	if p.MemoryKiB != 64*1024 || p.Iterations != 3 || p.Parallelism != 2 {
		t.Fatalf("params = %+v, want 64 MiB, 3 iterations, parallelism 2", p)
	}
	h := NewHasher()
	enc, err := h.Hash(context.Background(), "correct horse")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(enc, "$argon2id$v=19$m=65536,t=3,p=2$") {
		t.Errorf("encoded hash %q does not carry the AD-10 parameters", enc)
	}
	ok, rehash, err := h.Verify(context.Background(), "correct horse", enc)
	if err != nil || !ok || rehash {
		t.Errorf("Verify = %v, %v, %v", ok, rehash, err)
	}
}

func TestHashVerifyAndSalt(t *testing.T) {
	ctx := context.Background()
	h := fastHasher()
	a, _ := h.Hash(ctx, "pw-probe")
	b, _ := h.Hash(ctx, "pw-probe")
	if a == b {
		t.Fatal("two hashes of one password are identical: the salt is not random")
	}
	if strings.Contains(a, "pw-probe") {
		t.Error("the hash contains the password")
	}
	if ok, _, err := h.Verify(ctx, "pw-probe", a); err != nil || !ok {
		t.Errorf("right password: %v, %v", ok, err)
	}
	if ok, _, err := h.Verify(ctx, "pw-probf", a); err != nil || ok {
		t.Errorf("wrong password: %v, %v", ok, err)
	}
	if ok, _, err := h.Verify(ctx, "", a); err != nil || ok {
		t.Errorf("empty password: %v, %v", ok, err)
	}
}

func TestVerifyReportsWeakerStoredParameters(t *testing.T) {
	ctx := context.Background()
	old := NewHasher(WithParams(Params{MemoryKiB: 32, Iterations: 1, Parallelism: 1}))
	enc, _ := old.Hash(ctx, "pw")
	ok, rehash, err := fastHasher().Verify(ctx, "pw", enc)
	if err != nil || !ok || !rehash {
		t.Fatalf("Verify = %v, %v, %v; want ok with rehash", ok, rehash, err)
	}
}

func TestVerifyRejectsMalformedHashes(t *testing.T) {
	ctx := context.Background()
	good, _ := fastHasher().Hash(ctx, "pw")
	parts := strings.Split(good, "$")
	mk := func(i int, v string) string {
		c := append([]string(nil), parts...)
		c[i] = v
		return strings.Join(c, "$")
	}
	for name, enc := range map[string]string{
		"empty":           "",
		"plain":           "notahash",
		"bcrypt":          "$2a$10$abcdefghijklmnopqrstuuABCDEFGHIJKLMNOPQRSTUVWXYZ012345",
		"argon2i":         mk(1, "argon2i"),
		"version":         mk(2, "v=16"),
		"missing params":  mk(3, "m=64,t=1"),
		"order":           mk(3, "t=1,m=64,p=1"),
		"huge memory":     mk(3, "m=4194304,t=1,p=1"),
		"huge iterations": mk(3, "m=64,t=1000,p=1"),
		"zero lanes":      mk(3, "m=64,t=1,p=0"),
		"not a number":    mk(3, "m=x,t=1,p=1"),
		"salt":            mk(4, "***"),
		"short salt":      mk(4, "AAAA"),
		"key":             mk(5, "***"),
		"short key":       mk(5, "AAAA"),
		"extra field":     good + "$x",
	} {
		ok, _, err := fastHasher().Verify(ctx, "pw", enc)
		if ok || !errors.Is(err, ErrBadHash) {
			t.Errorf("%s: Verify = %v, %v; want ErrBadHash", name, ok, err)
		}
		if err != nil && enc != "" && strings.Contains(err.Error(), enc) {
			t.Errorf("%s: the error contains the stored value", name)
		}
	}
}

func TestPasswordLengthLimit(t *testing.T) {
	ctx := context.Background()
	h := fastHasher()
	long := strings.Repeat("a", MaxPasswordLen+1)
	if _, err := h.Hash(ctx, long); !errors.Is(err, ErrPasswordTooLong) {
		t.Errorf("Hash of a long password: %v", err)
	}
	enc, err := h.Hash(ctx, strings.Repeat("a", MaxPasswordLen))
	if err != nil {
		t.Fatal(err)
	}
	if ok, _, err := h.Verify(ctx, long, enc); ok || err != nil {
		t.Errorf("Verify of a long password = %v, %v", ok, err)
	}
	if err := h.Burn(ctx, long); err != nil {
		t.Errorf("Burn: %v", err)
	}
}

func TestSaltSourceFailureIsAnError(t *testing.T) {
	h := NewHasher(WithParams(fastParams), WithSaltSource(func([]byte) error { return errors.New("no entropy") }))
	if _, err := h.Hash(context.Background(), "pw"); err == nil {
		t.Fatal("a salt failure was ignored")
	}
}

func TestConcurrentHashesAreBounded(t *testing.T) {
	// Fill every slot, then check that a further call waits for its context.
	h := fastHasher()
	for range cap(h.slots) {
		h.slots <- struct{}{}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := h.Hash(ctx, "pw"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Hash with all slots busy: %v", err)
	}
	if _, _, err := h.Verify(ctx, "pw", "$argon2id$v=19$m=64,t=1,p=1$AAAAAAAAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAA"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Verify with all slots busy: %v", err)
	}
	for range cap(h.slots) {
		<-h.slots
	}
	var wg sync.WaitGroup
	var done atomic.Int32
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := h.Hash(context.Background(), "pw"); err == nil {
				done.Add(1)
			}
		}()
	}
	wg.Wait()
	if done.Load() != 12 {
		t.Errorf("%d of 12 hashes finished", done.Load())
	}
}

func TestGeneratePassword(t *testing.T) {
	seen := map[string]bool{}
	for range 20 {
		p, err := GeneratePassword(nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(p) != 24 || seen[p] {
			t.Fatalf("password %q: length %d or repeated", p, len(p))
		}
		seen[p] = true
	}
	if _, err := GeneratePassword(strings.NewReader("short")); err == nil {
		t.Error("a short random source was accepted")
	}
}

// TestOverlongPasswordCostsOneComputationOnEveryPath proves the timing rule of
// the login: a password over MaxPasswordLen takes one full Argon2id
// computation whether the account exists (Verify) or not (Burn), so response
// time does not reveal which accounts exist.
func TestOverlongPasswordCostsOneComputationOnEveryPath(t *testing.T) {
	ctx := context.Background()
	h := fastHasher()
	enc, err := h.Hash(ctx, "right")
	if err != nil {
		t.Fatal(err)
	}
	long := strings.Repeat("a", 2000)
	for name, run := range map[string]func() error{
		"Verify, known account": func() error {
			ok, _, err := h.Verify(ctx, long, enc)
			if ok {
				t.Error("an over-long password verified")
			}
			return err
		},
		"Burn, unknown account": func() error { return h.Burn(ctx, long) },
	} {
		before := h.computations.Load()
		if err := run(); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if n := h.computations.Load() - before; n != 1 {
			t.Errorf("%s: %d Argon2id computations, want exactly 1", name, n)
		}
	}
}
