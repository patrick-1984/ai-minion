package store_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"path/filepath"
	"sync"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/hub/hubid"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/identity"
)

// A real hubid service with a fast fake sealer, to prove that backups taken in
// the middle of identity operations restore to a Hub that starts and opens its
// sealed root with the passphrase of the backup's time.

const (
	pw1 = "correct horse battery staple"
	pw2 = "another long pass phrase!!"
)

var (
	bg = context.Background()
	ui = hubid.Actor{ID: "admin-1", Channel: hubid.ChannelUI}
)

type fakeSealer struct {
	mu    sync.Mutex
	seeds map[string][]byte
}

func (f *fakeSealer) Seal(seed []byte, pass string) ([]byte, error) {
	h := sha256.Sum256([]byte(pass))
	k, _ := identity.KeyFromSeed(seed)
	f.mu.Lock()
	f.seeds[string(k.Public())] = bytes.Clone(seed)
	f.mu.Unlock()
	return append(append([]byte("FKS1"), h[:]...), seed...), nil
}

func (f *fakeSealer) Reseal(s *identity.RootSigner, pass string) ([]byte, error) {
	f.mu.Lock()
	seed := f.seeds[string(s.Public())]
	f.mu.Unlock()
	return f.Seal(seed, pass)
}

func (f *fakeSealer) Open(sealed []byte, pass string) (*identity.RootSigner, error) {
	if len(sealed) != 4+32+32 || string(sealed[:4]) != "FKS1" {
		return nil, identity.ErrSealedRootInvalid
	}
	h := sha256.Sum256([]byte(pass))
	if !bytes.Equal(sealed[4:36], h[:]) {
		return nil, identity.ErrWrongPassphrase
	}
	return identity.RootSignerFromSeed(sealed[36:])
}

func (f *fakeSealer) Dummy(string) {}

type hub struct {
	t   *testing.T
	dir string
	st  *store.Store
	fk  *fakeSealer
}

func newHub(t *testing.T, dir string, fk *fakeSealer) *hub {
	t.Helper()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if fk == nil {
		fk = &fakeSealer{seeds: map[string][]byte{}}
	}
	return &hub{t: t, dir: dir, st: st, fk: fk}
}

func (h *hub) open() *hubid.Service {
	h.t.Helper()
	s, err := hubid.Open(bg, hubid.Options{DataDir: h.dir, Store: h.st, Sealer: h.fk})
	if err != nil {
		h.t.Fatal(err)
	}
	return s
}

func (h *hub) backup(hook func(n int)) string {
	h.t.Helper()
	n := 0
	out := filepath.Join(h.t.TempDir(), "b.tar.gz")
	if _, err := store.Backup(bg, h.dir, out, "dev", store.WithBackupSnapshotHook(func() {
		n++
		if hook != nil {
			hook(n)
		}
	})); err != nil {
		h.t.Fatal(err)
	}
	return out
}

func (h *hub) restored(archive string) *hub {
	h.t.Helper()
	dir := filepath.Join(h.t.TempDir(), "restored")
	if _, err := store.Restore(bg, archive, dir, false); err != nil {
		h.t.Fatalf("restore: %v", err)
	}
	return newHub(h.t, dir, h.fk)
}

func TestRestoredEstablishedHubKeepsItsCertificateAndOpensWithTheBackupPassphrase(t *testing.T) {
	h := newHub(t, filepath.Join(t.TempDir(), "hub"), nil)
	s := h.open()
	if _, err := s.SetPassphrase(bg, ui, pw1, pw1, hubid.IssueOptions{}); err != nil {
		t.Fatal(err)
	}
	want, _ := s.Current()
	rs := h.restored(h.backup(nil)).open()
	if rs.State() != hubid.StateEstablished {
		t.Fatalf("state = %v", rs.State())
	}
	got, err := rs.Current()
	if err != nil || !bytes.Equal(got.Cert.Bytes(), want.Cert.Bytes()) || !bytes.Equal(got.RootPub, want.RootPub) {
		t.Fatalf("restored snapshot differs: %v", err)
	}
	if _, err := rs.Reissue(bg, ui, pw1, hubid.IssueOptions{}); err != nil {
		t.Fatal(err)
	}
}

func TestBackupDuringAPassphraseChangeOrRotationRestoresAWorkingHub(t *testing.T) {
	h := newHub(t, filepath.Join(t.TempDir(), "hub"), nil)
	s := h.open()
	if _, err := s.SetPassphrase(bg, ui, pw1, pw1, hubid.IssueOptions{}); err != nil {
		t.Fatal(err)
	}
	// The passphrase changes between the database copy and the read of the files.
	out := h.backup(func(n int) {
		if n == 1 {
			if _, err := s.ChangePassphrase(bg, ui, pw1, pw2, pw2); err != nil {
				t.Error(err)
			}
		}
	})
	if _, err := h.restored(out).open().Reissue(bg, ui, pw2, hubid.IssueOptions{}); err != nil {
		t.Fatal(err)
	}
	// A rotation between the copy and the read: the archive holds the new root.
	out = h.backup(func(n int) {
		if n == 1 {
			if _, err := s.RotateMaster(bg, ui, pw2, pw1, pw1, hubid.IssueOptions{}); err != nil {
				t.Error(err)
			}
		}
	})
	cur, err := h.restored(out).open().Current()
	if err != nil {
		t.Fatal(err)
	}
	now, _ := s.Current()
	if !bytes.Equal(cur.RootPub, now.RootPub) {
		t.Fatal("the archive holds the retired root")
	}
}
