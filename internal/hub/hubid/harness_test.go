package hubid

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"github.com/patrick-1984/ai-minion/internal/testutil/hubstore"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/identity"
)

const (
	pw1 = "correct horse battery staple"
	pw2 = "another long pass phrase!!"
	pw3 = "third long pass phrase here"
)

var (
	ui  = Actor{ID: "admin-1", Channel: ChannelUI}
	cli = Actor{ID: "local:1000", Channel: ChannelCLI}
)

// fakeSealer is a fast stand-in for the age scrypt sealer: the file is
// "FKS1" + SHA-256(passphrase) + seed. The real sealer is covered by one
// test; each real derivation costs about a second.
type fakeSealer struct {
	mu     sync.Mutex
	seeds  map[string][]byte
	dummy  int
	opened int
	last   *identity.RootSigner // the signer of the latest successful Open
}

func newFakeSealer() *fakeSealer { return &fakeSealer{seeds: map[string][]byte{}} }

func (f *fakeSealer) Seal(seed []byte, pass string) ([]byte, error) {
	h := sha256.Sum256([]byte(pass))
	f.remember(seed)
	return append(append([]byte("FKS1"), h[:]...), seed...), nil
}

func (f *fakeSealer) remember(seed []byte) {
	k, _ := identity.KeyFromSeed(seed)
	f.mu.Lock()
	f.seeds[string(k.Public())] = bytes.Clone(seed)
	f.mu.Unlock()
}

func (f *fakeSealer) Reseal(s *identity.RootSigner, pass string) ([]byte, error) {
	f.mu.Lock()
	seed := f.seeds[string(s.Public())]
	f.mu.Unlock()
	return f.Seal(seed, pass)
}

func (f *fakeSealer) Open(sealed []byte, pass string) (*identity.RootSigner, error) {
	f.mu.Lock()
	f.opened++
	f.mu.Unlock()
	if len(sealed) != 4+32+32 || string(sealed[:4]) != "FKS1" {
		return nil, identity.ErrSealedRootInvalid
	}
	h := sha256.Sum256([]byte(pass))
	if !bytes.Equal(sealed[4:36], h[:]) {
		return nil, identity.ErrWrongPassphrase
	}
	f.remember(sealed[36:])
	sg, err := identity.RootSignerFromSeed(sealed[36:])
	f.mu.Lock()
	f.last = sg
	f.mu.Unlock()
	return sg, err
}

func (f *fakeSealer) Dummy(string) { f.mu.Lock(); f.dummy++; f.mu.Unlock() }

type env struct {
	t       *testing.T
	dir     string
	st      *store.Store
	fk      *fakeSealer
	clock   time.Time
	log     *bytes.Buffer
	closes  int
	signers []*identity.RootSigner

	point       func(string) error
	makeDurable func(context.Context) error
	rename      func(from, to string) error
	syncDir     func(string) error
	opts        func(*Options)
}

func newEnv(t *testing.T) *env {
	t.Helper()
	dir := t.TempDir()
	st, err := hubstore.Open(t, dir)
	if err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, dir: dir, st: st, fk: newFakeSealer(), clock: time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC), log: &bytes.Buffer{}}
	t.Cleanup(func() { _ = e.st.Close() })
	return e
}

// reopen closes the Store and opens a new one on the same files: what a
// restarted process sees.
func (e *env) reopen() {
	e.t.Helper()
	if err := e.st.Close(); err != nil {
		e.t.Fatal(err)
	}
	st, err := store.Open(e.dir)
	if err != nil {
		e.t.Fatal(err)
	}
	e.st = st
}

// snapshotDB copies the database as it is now (a consistent copy, without
// closing the Store) to a file and returns its path.
func (e *env) snapshotDB() string {
	e.t.Helper()
	p := filepath.Join(e.t.TempDir(), "snapshot.db")
	if _, err := e.st.DB().Exec(`VACUUM INTO ?`, p); err != nil {
		e.t.Fatal(err)
	}
	return p
}

// restoreDB replaces the database by an older snapshot and reopens the Store:
// the power was lost before the later commits reached the disk, while every
// file written since stayed.
func (e *env) restoreDB(snapshot string) {
	e.t.Helper()
	if err := e.st.Close(); err != nil {
		e.t.Fatal(err)
	}
	b, err := os.ReadFile(snapshot)
	if err != nil {
		e.t.Fatal(err)
	}
	db := filepath.Join(e.dir, store.DBFile)
	for _, suffix := range []string{"-wal", "-shm"} {
		_ = os.Remove(db + suffix)
	}
	if err := os.WriteFile(db, b, 0o600); err != nil {
		e.t.Fatal(err)
	}
	st, err := store.Open(e.dir)
	if err != nil {
		e.t.Fatal(err)
	}
	e.st = st
}

func (e *env) options() Options {
	o := Options{
		DataDir: e.dir, Store: e.st, Sealer: e.fk, LockWait: 200 * time.Millisecond,
		Now:           func() time.Time { return e.clock },
		Logger:        slog.New(slog.NewTextHandler(e.log, nil)),
		CloseSessions: func() { e.closes++ },
		Hooks: Hooks{
			Point: func(n string) error {
				if e.point != nil {
					return e.point(n)
				}
				return nil
			},
			Signer: func(s *identity.RootSigner) { e.signers = append(e.signers, s) },
		},
	}
	o.Hooks.MakeDurable = func(ctx context.Context) error {
		if e.makeDurable != nil {
			if err := e.makeDurable(ctx); err != nil {
				return err
			}
		}
		return e.st.MakeDurable(ctx)
	}
	o.Hooks.Rename = func(from, to string) error {
		if e.rename != nil {
			if err := e.rename(from, to); err != nil {
				return err
			}
		}
		return os.Rename(from, to)
	}
	o.Hooks.SyncDir = func(d string) error {
		if e.syncDir != nil {
			if err := e.syncDir(d); err != nil {
				return err
			}
		}
		return syncDirOS(d)
	}
	if e.opts != nil {
		e.opts(&o)
	}
	return o
}

// svc starts a (new) service on the directory: a Hub start or restart.
func (e *env) svc() *Service {
	e.t.Helper()
	s, err := Open(context.Background(), e.options())
	if err != nil {
		e.t.Fatalf("start: %v", err)
	}
	return s
}

func (e *env) startErr() error {
	_, err := Open(context.Background(), e.options())
	return err
}

// crashAt makes the named point simulate the death of the process.
func (e *env) crashAt(name string) {
	e.point = func(n string) error {
		if n == name {
			e.point = nil
			return ErrCrash
		}
		return nil
	}
}

// plain creates the state of a migrated 0.6.3 Hub: identity.key and the
// legacy row; it returns the key.
func (e *env) plain() *identity.Key {
	e.t.Helper()
	k, err := identity.GenerateKey()
	if err != nil {
		e.t.Fatal(err)
	}
	if err := identity.SaveKey(filepath.Join(e.dir, PlainFile), k); err != nil {
		e.t.Fatal(err)
	}
	if err := e.st.PutHubIdentity(context.Background(), store.HubIdentityRow{Pub: k.Public(), Fingerprint: k.Fingerprint().String(), CreatedAt: 1}); err != nil {
		e.t.Fatal(err)
	}
	// The forward-only migration flags an identity that existed before it.
	if _, err := e.st.DB().Exec(`UPDATE hub_identity SET migrated_from_06 = 1`); err != nil {
		e.t.Fatal(err)
	}
	return k
}

// confirmFailsAfterCommit makes the durability confirmation fail while *fail is
// set, but only for the confirmations that follow the commit of an identity
// change (the gate's confirmation of a counted attempt keeps working).
func (e *env) confirmFailsAfterCommit(fail *bool) (rearm func()) {
	committed := false
	e.point = func(n string) error {
		switch n {
		case PointCommitted, PointWCommitted, PointBSCommitted:
			committed = true
		case PointAfterUnseal:
			committed = false
		}
		return nil
	}
	e.makeDurable = func(context.Context) error {
		if *fail && committed {
			return errors.New("checkpoint failed")
		}
		return nil
	}
	return func() { committed = false }
}

func (e *env) row() store.HubIdentityV2 {
	e.t.Helper()
	r, ok, err := e.st.HubIdentityV2(context.Background())
	if err != nil || !ok {
		e.t.Fatalf("row: %v ok=%v", err, ok)
	}
	return r
}

func (e *env) cert() *identity.HubCert {
	e.t.Helper()
	c, err := identity.ParseHubCert(e.row().CurrentCert)
	if err != nil {
		e.t.Fatal(err)
	}
	return c
}

func (e *env) has(name string) bool {
	_, err := os.Lstat(filepath.Join(e.dir, name))
	return err == nil
}

func (e *env) opPub(name string) []byte {
	e.t.Helper()
	k, err := identity.LoadKey(filepath.Join(e.dir, name))
	if err != nil {
		e.t.Fatalf("load %s: %v", name, err)
	}
	return k.Public()
}

// rootPub opens root.key.age with pass through the fake sealer.
func (e *env) rootOpens(pass string) ([]byte, bool) {
	b, err := os.ReadFile(filepath.Join(e.dir, RootFile))
	if err != nil {
		e.t.Fatal(err)
	}
	s, err := e.fk.Open(b, pass)
	if err != nil {
		return nil, false
	}
	defer s.Destroy()
	return s.Public(), true
}

func (e *env) audits(action string) []store.AuditRow {
	e.t.Helper()
	rows, err := e.st.ListAudit(context.Background(), store.AuditQuery{Action: action, Limit: 100})
	if err != nil {
		e.t.Fatal(err)
	}
	return rows
}

func (e *env) established() *Service {
	e.t.Helper()
	s := e.svc()
	if _, err := s.SetPassphrase(context.Background(), ui, pw1, pw1, IssueOptions{}); err != nil {
		e.t.Fatal(err)
	}
	return s
}

func wantCode(t *testing.T, err error, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("error = %v, want %v", err, want)
	}
}

func (e *env) allSignersDestroyed() {
	e.t.Helper()
	for i, s := range e.signers {
		if _, err := s.Sign(identity.PurposeHubCert, []byte("x")); !errors.Is(err, identity.ErrSignerDestroyed) {
			e.t.Fatalf("signer %d was not destroyed: %v", i, err)
		}
	}
}
