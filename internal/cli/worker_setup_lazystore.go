package cli

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/patrick-1984/ai-minion/internal/worker/store"
)

// lazySetupStore is the Worker state of the wizard, opened for writing (which
// creates the database and applies the migrations of this release) only at
// the first write. Until then reads go to a read-only view of the existing
// database, or see an empty state when there is none. A wizard that is
// cancelled before it saved anything therefore leaves the state exactly as an
// older installed Worker can still open it.
type lazySetupStore struct {
	dir  string
	open func(ctx context.Context, dir string) (setupStore, error)

	mu sync.Mutex
	rw setupStore
	ro *store.Store
}

var _ setupStore = (*lazySetupStore)(nil)

// writable opens the state for writing once.
func (l *lazySetupStore) writable(ctx context.Context) (setupStore, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.rw != nil {
		return l.rw, nil
	}
	if err := os.MkdirAll(l.dir, 0o700); err != nil {
		return nil, fmt.Errorf("cannot create the state directory %q: %w", l.dir, err)
	}
	rw, err := l.open(ctx, l.dir)
	if err != nil {
		return nil, fmt.Errorf("open the Worker state in %s: %w", l.dir, err)
	}
	if l.ro != nil {
		_ = l.ro.Close()
		l.ro = nil
	}
	l.rw = rw
	return rw, nil
}

// reader returns the writable state when it is open, else a read-only view
// (nil when no database exists yet).
func (l *lazySetupStore) reader(ctx context.Context) (setupStore, bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.rw != nil {
		return l.rw, true, nil
	}
	if l.ro != nil {
		return l.ro, true, nil
	}
	if _, err := os.Stat(filepath.Join(l.dir, store.DBFile)); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("look at the Worker state in %s: %w", l.dir, err)
	}
	ro, err := store.OpenReadOnlyContext(ctx, l.dir)
	if err != nil {
		return nil, false, fmt.Errorf("open the Worker state in %s: %w", l.dir, err)
	}
	l.ro = ro
	return ro, true, nil
}

func (l *lazySetupStore) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	var err error
	if l.ro != nil {
		err = l.ro.Close()
		l.ro = nil
	}
	if l.rw != nil {
		err = errors.Join(err, l.rw.Close())
		l.rw = nil
	}
	return err
}

// ---- reads ----

func (l *lazySetupStore) LoadShareContext(ctx context.Context) (store.ShareSelection, error) {
	r, ok, err := l.reader(ctx)
	if err != nil || !ok {
		return store.ShareSelection{Mode: store.ShareLater}, err
	}
	return r.LoadShareContext(ctx)
}

func (l *lazySetupStore) LoadEnrollment(ctx context.Context) (store.Enrollment, error) {
	r, ok, err := l.reader(ctx)
	if err != nil {
		return store.Enrollment{}, err
	}
	if !ok {
		return store.Enrollment{}, store.ErrNotEnrolled
	}
	return r.LoadEnrollment(ctx)
}

func (l *lazySetupStore) LoadPairResume(ctx context.Context) (store.PairResume, error) {
	r, ok, err := l.reader(ctx)
	if err != nil {
		return store.PairResume{}, err
	}
	if !ok {
		return store.PairResume{}, store.ErrNoPairResume
	}
	return r.LoadPairResume(ctx)
}

// ---- writes ----

func (l *lazySetupStore) SaveShareContext(ctx context.Context, sel store.ShareSelection) error {
	w, err := l.writable(ctx)
	if err != nil {
		return err
	}
	return w.SaveShareContext(ctx, sel)
}

func (l *lazySetupStore) SaveEnrollment(ctx context.Context, e store.Enrollment) error {
	w, err := l.writable(ctx)
	if err != nil {
		return err
	}
	return w.SaveEnrollment(ctx, e)
}

func (l *lazySetupStore) UpdatePin(ctx context.Context, pub ed25519.PublicKey, seq uint64, passed ...ed25519.PublicKey) error {
	w, err := l.writable(ctx)
	if err != nil {
		return err
	}
	return w.UpdatePin(ctx, pub, seq, passed...)
}

func (l *lazySetupStore) UpdatePinRatchet(ctx context.Context, pub ed25519.PublicKey, seq, maxSerial uint64, certSHA256 []byte, passed ...ed25519.PublicKey) error {
	w, err := l.writable(ctx)
	if err != nil {
		return err
	}
	return w.UpdatePinRatchet(ctx, pub, seq, maxSerial, certSHA256, passed...)
}

func (l *lazySetupStore) SavePairResume(ctx context.Context, r store.PairResume) error {
	w, err := l.writable(ctx)
	if err != nil {
		return err
	}
	return w.SavePairResume(ctx, r)
}

func (l *lazySetupStore) DeletePairResume(ctx context.Context) (bool, error) {
	w, err := l.writable(ctx)
	if err != nil {
		return false, err
	}
	return w.DeletePairResume(ctx)
}

func (l *lazySetupStore) DeletePairResumeIf(ctx context.Context, pairingID, code string) (bool, error) {
	w, err := l.writable(ctx)
	if err != nil {
		return false, err
	}
	return w.DeletePairResumeIf(ctx, pairingID, code)
}

func (l *lazySetupStore) UpdateWelcome(ctx context.Context, wi store.WelcomeInfo) error {
	w, err := l.writable(ctx)
	if err != nil {
		return err
	}
	return w.UpdateWelcome(ctx, wi)
}

func (l *lazySetupStore) SetHubCheck(ctx context.Context, pub ed25519.PublicKey, check, anchor string) error {
	w, err := l.writable(ctx)
	if err != nil {
		return err
	}
	return w.SetHubCheck(ctx, pub, check, anchor)
}

func (l *lazySetupStore) WipeEnrollment(ctx context.Context) (bool, error) {
	w, err := l.writable(ctx)
	if err != nil {
		return false, err
	}
	return w.WipeEnrollment(ctx)
}
