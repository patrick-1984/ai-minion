package workers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/audit"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/transport"
)

// fakeInvitations records the calls of the invitation revoker.
type fakeInvitations struct {
	mu      sync.Mutex
	calls   int
	pending int // invitations revoked by the next call
	err     error
}

func (f *fakeInvitations) RevokeAllInvitations(context.Context) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return 0, f.err
	}
	n := f.pending
	f.pending = 0
	return n, nil
}

func (f *fakeInvitations) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func rotator(t *testing.T, h *testHub) *Rotator {
	t.Helper()
	r := h.svc.Rotator()
	if r == nil {
		t.Fatal("the Service has no rotator")
	}
	return r
}

func rotateOnce(t *testing.T, r *Rotator) RotateResult {
	t.Helper()
	res, err := r.Rotate(context.Background(), RotateActor{ID: "admin-1"})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestRotationCreatesANewKeyAppendsASignedTransitionAndAuditsIt(t *testing.T) {
	h := newTestHub(t, nil)
	inv := &fakeInvitations{pending: 3}
	r := rotator(t, h)
	r.SetInvitations(inv)
	old := h.svc.Identity().Public()
	oldFP := h.svc.Identity().Fingerprint()

	res, err := r.Rotate(context.Background(), RotateActor{ID: "admin-1", ActingAs: "user-9"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Seq != 1 || res.OldFingerprint != oldFP || res.NewFingerprint == oldFP || res.RevokedInvitations != 3 {
		t.Errorf("result = %+v", res)
	}
	if inv.callCount() < 1 {
		t.Error("the invitation keys were not revoked")
	}

	// The running Hub uses the new key; the key file and the record agree.
	cur := h.svc.Identity()
	if bytes.Equal(cur.Public(), old) || cur.Fingerprint() != res.NewFingerprint {
		t.Errorf("the identity did not change: %s", cur.Fingerprint())
	}
	onDisk, err := ReadIdentity(h.dataDir())
	if err != nil || !bytes.Equal(onDisk.Public(), cur.Public()) {
		t.Fatalf("identity.key = %v, %v", onDisk, err)
	}
	if _, err := os.Stat(filepath.Join(h.dataDir(), PendingKeyFile)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the pending key file was left behind: %v", err)
	}
	rec, ok, err := h.st.HubIdentity(context.Background())
	if err != nil || !ok || !bytes.Equal(rec.Pub, cur.Public()) || rec.Fingerprint != res.NewFingerprint.String() {
		t.Errorf("recorded identity = %+v, %v, %v", rec, ok, err)
	}

	// The transition record is signed by the old key (purpose transition).
	chain, err := ChainSource(h.st)(context.Background())
	if err != nil || len(chain) != 1 {
		t.Fatalf("chain = %v, %v", chain, err)
	}
	tr := chain[0]
	if tr.Seq != 1 || !bytes.Equal(tr.OldPub, old) || !bytes.Equal(tr.NewPub, cur.Public()) || tr.Verify() != nil {
		t.Errorf("transition = %+v (verify: %v)", tr, tr.Verify())
	}
	if got, err := identity.VerifyChain(identity.Pin{Pub: old}, chain); err != nil || !bytes.Equal(got.Pub, cur.Public()) || got.Seq != 1 {
		t.Errorf("VerifyChain = %+v, %v", got, err)
	}

	// The event is audited with the actor and the new fingerprint, never a key.
	page, err := h.audit.List(context.Background(), audit.Filter{Action: RotateAction})
	if err != nil || len(page.Events) != 1 {
		t.Fatalf("audit = %+v, %v", page, err)
	}
	ev := page.Events[0]
	if ev.Actor != "admin-1" || ev.ActingAs != "user-9" || ev.Target != res.NewFingerprint.String() || ev.Outcome != "success" {
		t.Errorf("audit event = %+v", ev)
	}
	if strings.Contains(h.logs.String(), "BEGIN") || !strings.Contains(h.logs.String(), "hub identity rotated") {
		t.Errorf("logs = %s", h.logs.String())
	}
}

func TestRotationSurvivesARestartOfTheHub(t *testing.T) {
	dir := t.TempDir()
	st := openStore(t, dir)
	var logs syncBuf
	svc, err := NewService(context.Background(), ServiceOptions{DataDir: dir, Store: st, Logger: textLogger(&logs)})
	if err != nil {
		t.Fatal(err)
	}
	first := svc.Identity().Public()
	res, err := svc.Rotator().Rotate(context.Background(), RotateActor{ID: "a"})
	if err != nil {
		t.Fatal(err)
	}
	svc.Close()

	svc2, err := NewService(context.Background(), ServiceOptions{DataDir: dir, Store: st, Logger: textLogger(&logs)})
	if err != nil {
		t.Fatalf("the Hub does not start after a rotation: %v", err)
	}
	defer svc2.Close()
	if svc2.Identity().Fingerprint() != res.NewFingerprint || svc2.Identity().Created() {
		t.Errorf("identity after restart = %s", svc2.Identity().Fingerprint())
	}
	chain, err := ChainSource(st)(context.Background())
	if err != nil || len(chain) != 1 || !bytes.Equal(chain[0].OldPub, first) {
		t.Errorf("chain after restart = %v, %v", chain, err)
	}
}

func TestAWorkerPinnedToAnyOlderKeyFollowsTheChainAndConnects(t *testing.T) {
	h := newTestHub(t, nil)
	r := rotator(t, h)
	key := newKey(t)
	h.enrol(key, "owner-1", "pi")

	pinned := []identity.Pin{h.pin()} // the original key, position unknown
	rotateOnce(t, r)
	pinned = append(pinned, identity.Pin{Pub: h.svc.Identity().Public(), Seq: 1})
	rotateOnce(t, r)
	rotateOnce(t, r)
	cur := h.svc.Identity().Public()

	for i, pin := range pinned {
		cfg := h.workerConfig(key)
		cfg.Pin = pin
		res, err := h.dial(cfg)
		if err != nil {
			t.Fatalf("pin %d: %v", i, err)
		}
		if !res.PinChanged || res.Pin.Seq != 3 || !bytes.Equal(res.Pin.Pub, cur) {
			t.Errorf("pin %d: new pin = %+v changed=%v", i, res.Pin, res.PinChanged)
		}
		if w := decodeWelcome(t, res); w.Name != "pi" {
			t.Errorf("pin %d: welcome = %+v", i, w)
		}
		_ = res.Channel.Close()
	}

	// A Worker already at the end of the chain is unchanged.
	cfg := h.workerConfig(key)
	cfg.Pin = identity.Pin{Pub: cur, Seq: 3}
	res, err := h.dial(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Channel.Close()
	if res.PinChanged {
		t.Errorf("a current pin changed: %+v", res.Pin)
	}
}

func TestAWorkerRejectsABadChainAndKeepsItsPin(t *testing.T) {
	good := newTestHub(t, nil)
	r := rotator(t, good)
	oldPin := good.pin()
	rotateOnce(t, r)
	rotateOnce(t, r)
	chain, err := ChainSource(good.st)(context.Background())
	if err != nil || len(chain) != 2 {
		t.Fatal(chain, err)
	}

	corruptSig := func() []identity.Transition {
		c := cloneChain(chain)
		c[1].SigByOld = append(identity.B64(nil), c[1].SigByOld...)
		c[1].SigByOld[0] ^= 0xff
		return c
	}
	decreasing := func() []identity.Transition {
		c := cloneChain(chain)
		c[1].Seq = 1 // replayed seq: not increasing
		return c
	}
	gap := func() []identity.Transition {
		c := cloneChain(chain)
		c[1].Seq = 5
		return c
	}
	forgedLink := func() []identity.Transition {
		// A link signed by a key that is not the pinned one's successor.
		other := newKey(t)
		tr, err := identity.NewTransition(other, good.svc.Identity().Public(), 1, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		c := cloneChain(chain)
		c[0] = tr
		return c
	}
	for name, mk := range map[string]func() []identity.Transition{
		"bad signature": corruptSig, "decreasing seq": decreasing, "gap in seq": gap, "forged link": forgedLink,
	} {
		t.Run(name, func(t *testing.T) {
			bad := mk()
			// The same Hub key and store, a tampered chain on the wire.
			h2 := newTestHub(t, func(o *ServiceOptions) {
				o.Chain = func(context.Context) ([]identity.Transition, error) { return bad, nil }
			})
			// h2 has its own key: serve with the key of the rotated Hub.
			h2.svc.Identity().swap(good.svc.Identity().Key())
			key := newKey(t)
			h2.enrol(key, "owner-1", "pi")
			cfg := h2.workerConfig(key)
			cfg.Pin = oldPin
			res, err := h2.dial(cfg)
			if err == nil {
				_ = res.Channel.Close()
				t.Fatal("the Worker accepted a bad chain")
			}
			if !errors.Is(err, transport.ErrChainInvalid) && !errors.Is(err, transport.ErrPinMismatch) {
				t.Errorf("err = %v", err)
			}
			// The Worker's pin is the caller's value: nothing was rewritten
			// and the next honest connect still works.
			if !bytes.Equal(cfg.Pin.Pub, oldPin.Pub) || cfg.Pin.Seq != 0 {
				t.Errorf("the pin changed: %+v", cfg.Pin)
			}
		})
	}

	// After the bad chains, the honest Hub still serves the same Worker.
	key := newKey(t)
	good.enrol(key, "owner-1", "pi")
	cfg := good.workerConfig(key)
	cfg.Pin = oldPin
	res, err := good.dial(cfg)
	if err != nil {
		t.Fatalf("the honest chain was refused: %v", err)
	}
	_ = res.Channel.Close()
}

func cloneChain(c []identity.Transition) []identity.Transition {
	out := make([]identity.Transition, len(c))
	copy(out, c)
	return out
}

func TestRotationRevokesInvitationsBeforeAndAfterAndAbortsWhenRevocationFails(t *testing.T) {
	h := newTestHub(t, nil)
	r := rotator(t, h)
	inv := &fakeInvitations{err: errors.New("db busy")}
	r.SetInvitations(inv)
	before := h.svc.Identity().Fingerprint()
	if _, err := r.Rotate(context.Background(), RotateActor{ID: "a"}); err == nil || !strings.Contains(err.Error(), "invitation") {
		t.Fatalf("err = %v", err)
	}
	if h.svc.Identity().Fingerprint() != before {
		t.Error("the identity rotated although the invitations could not be revoked")
	}
	if chain, _ := ChainSource(h.st)(context.Background()); len(chain) != 0 {
		t.Error("a transition was appended")
	}

	inv.err = nil
	rotateOnce(t, r)
	if inv.callCount() != 3 { // one failed call, then before and after the rotation
		t.Errorf("revoker calls = %d, want 3", inv.callCount())
	}
	r.SetInvitations(nil) // back to the default
	rotateOnce(t, r)
}

func TestRotationNeedsAnActorAndRefusesAFullChain(t *testing.T) {
	h := newTestHub(t, nil)
	r := rotator(t, h)
	if _, err := r.Rotate(context.Background(), RotateActor{}); err == nil {
		t.Error("a rotation without an actor was accepted")
	}

	// A chain at the limit refuses another link.
	full := &fullStore{RotationStore: h.st, n: MaxChainLinks}
	r2, err := NewRotator(RotatorOptions{Identity: h.svc.Identity(), DataDir: h.dataDir(), Store: full})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r2.Rotate(context.Background(), RotateActor{ID: "a"}); !errors.Is(err, ErrChainFull) {
		t.Errorf("err = %v, want ErrChainFull", err)
	}
}

// fullStore reports a transition chain of n links.
type fullStore struct {
	RotationStore
	n int
}

func (f *fullStore) Transitions(context.Context) ([]store.TransitionRow, error) {
	rows := make([]store.TransitionRow, f.n)
	for i := range rows {
		rows[i] = store.TransitionRow{Seq: uint64(i + 1)}
	}
	return rows, nil
}

func TestTheLargestChainFitsTheHandshakeFrame(t *testing.T) {
	chain := make([]identity.Transition, MaxChainLinks)
	keys := make([]*identity.Key, MaxChainLinks+1)
	for i := range keys {
		keys[i] = newKey(t)
	}
	for i := range chain {
		tr, err := identity.NewTransition(keys[i], keys[i+1].Public(), uint64(i+1), time.Now())
		if err != nil {
			t.Fatal(err)
		}
		chain[i] = tr
	}
	body, err := json.Marshal(chain)
	if err != nil {
		t.Fatal(err)
	}
	if len(body) > proto.HandshakeFrameLimit-16*1024 { // leaves room for the rest of the Hub hello
		t.Errorf("%d links are %d bytes of JSON; the handshake frame limit is %d", MaxChainLinks, len(body), proto.HandshakeFrameLimit)
	}
	if _, err := identity.VerifyChain(identity.Pin{Pub: keys[0].Public()}, chain); err != nil {
		t.Errorf("the longest chain does not verify: %v", err)
	}
}

func TestAnInterruptedRotationIsCompletedAtTheNextStart(t *testing.T) {
	dir := t.TempDir()
	st := openStore(t, dir)
	var logs syncBuf
	svc, err := NewService(context.Background(), ServiceOptions{DataDir: dir, Store: st, Logger: textLogger(&logs)})
	if err != nil {
		t.Fatal(err)
	}
	old := svc.Identity().Key()
	svc.Close()

	// Crash after the commit, before the rename: the database records the new
	// key, identity.key is still the old one and the new key sits in .next.
	next := newKey(t)
	tr, err := identity.NewTransition(old, next.Public(), 1, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := identity.SaveKey(filepath.Join(dir, PendingKeyFile), next); err != nil {
		t.Fatal(err)
	}
	if err := st.RotateHubIdentity(context.Background(), store.RotateParams{
		Expect: old.Public(), Transition: rowOf(tr),
		Identity: store.HubIdentityRow{Pub: next.Public(), Fingerprint: next.Fingerprint().String(), CreatedAt: tr.CreatedAt.UnixMilli()},
	}); err != nil {
		t.Fatal(err)
	}

	svc2, err := NewService(context.Background(), ServiceOptions{DataDir: dir, Store: st, Logger: textLogger(&logs)})
	if err != nil {
		t.Fatalf("start after the interrupted rotation: %v", err)
	}
	defer svc2.Close()
	if !bytes.Equal(svc2.Identity().Public(), next.Public()) {
		t.Error("the Hub did not adopt the new key")
	}
	if _, err := os.Stat(filepath.Join(dir, PendingKeyFile)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("pending file left: %v", err)
	}
	if !strings.Contains(logs.String(), "rotation completed") {
		t.Errorf("logs = %s", logs.String())
	}
}

func TestAPendingKeyOfARotationThatNeverCommittedIsDropped(t *testing.T) {
	dir := t.TempDir()
	st := openStore(t, dir)
	var logs syncBuf
	svc, err := NewService(context.Background(), ServiceOptions{DataDir: dir, Store: st, Logger: textLogger(&logs)})
	if err != nil {
		t.Fatal(err)
	}
	cur := svc.Identity().Public()
	svc.Close()
	if err := identity.SaveKey(filepath.Join(dir, PendingKeyFile), newKey(t)); err != nil {
		t.Fatal(err)
	}
	svc2, err := NewService(context.Background(), ServiceOptions{DataDir: dir, Store: st, Logger: textLogger(&logs)})
	if err != nil {
		t.Fatal(err)
	}
	defer svc2.Close()
	if !bytes.Equal(svc2.Identity().Public(), cur) {
		t.Error("the Hub adopted a key that was never recorded")
	}
	if _, err := os.Stat(filepath.Join(dir, PendingKeyFile)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("pending file left: %v", err)
	}
}

func TestACorruptPendingKeyFileStopsTheStartWithAClearMessage(t *testing.T) {
	dir := t.TempDir()
	st := openStore(t, dir)
	svc, err := NewService(context.Background(), ServiceOptions{DataDir: dir, Store: st})
	if err != nil {
		t.Fatal(err)
	}
	svc.Close()
	if err := os.WriteFile(filepath.Join(dir, PendingKeyFile), []byte("junk"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewService(context.Background(), ServiceOptions{DataDir: dir, Store: st}); err == nil ||
		!strings.Contains(err.Error(), PendingKeyFile) {
		t.Fatalf("err = %v", err)
	}
}

func TestRotationWhileWorkersConnectIsRaceFree(t *testing.T) {
	h := newTestHub(t, nil)
	r := rotator(t, h)
	key := newKey(t)
	h.enrol(key, "owner-1", "pi")
	pin := h.pin()

	var wg sync.WaitGroup
	var ok, mismatch atomic.Int32
	stop := make(chan struct{})
	for range 3 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				cfg := h.workerConfig(key)
				cfg.Pin = pin
				res, err := h.dial(cfg)
				if err != nil {
					// A connect that straddles a rotation may see the old chain
					// with the new key; the Worker keeps its pin and retries.
					mismatch.Add(1)
					continue
				}
				ok.Add(1)
				_ = res.Channel.Close()
			}
		}()
	}
	for range 3 {
		rotateOnce(t, r)
	}
	// A connect after the rotations always succeeds.
	eventuallyTrue(t, func() bool { return ok.Load() > 0 })
	close(stop)
	wg.Wait()
	cfg := h.workerConfig(key)
	cfg.Pin = pin
	res, err := h.dial(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if res.Pin.Seq != 3 {
		t.Errorf("pin seq = %d, want 3", res.Pin.Seq)
	}
	_ = res.Channel.Close()
}

func eventuallyTrue(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestNewRotatorNeedsItsDependencies(t *testing.T) {
	id := NewIdentity(newKey(t))
	for name, o := range map[string]RotatorOptions{
		"identity": {DataDir: "d", Store: &fullStore{}},
		"data dir": {Identity: id, Store: &fullStore{}},
		"store":    {Identity: id, DataDir: "d"},
	} {
		if _, err := NewRotator(o); err == nil {
			t.Errorf("a rotator without %s was accepted", name)
		}
	}
}

func TestServiceWithoutAChainStoreHasNoRotator(t *testing.T) {
	dir := t.TempDir()
	st := openStore(t, dir)
	svc, err := NewService(context.Background(), ServiceOptions{DataDir: dir, Store: plainStore{st}})
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	if svc.Rotator() != nil {
		t.Error("a Service over a store without a chain has a rotator")
	}
}

// plainStore hides the chain methods of the store.
type plainStore struct{ s *store.Store }

func (p plainStore) HubIdentity(ctx context.Context) (store.HubIdentityRow, bool, error) {
	return p.s.HubIdentity(ctx)
}
func (p plainStore) PutHubIdentity(ctx context.Context, r store.HubIdentityRow) error {
	return p.s.PutHubIdentity(ctx, r)
}
func (p plainStore) WorkerByPub(ctx context.Context, pub []byte) (store.WorkerRow, error) {
	return p.s.WorkerByPub(ctx, pub)
}
func (p plainStore) TombstoneByPub(ctx context.Context, pub []byte) (store.TombstoneRow, error) {
	return p.s.TombstoneByPub(ctx, pub)
}
func (p plainStore) WorkerOwner(ctx context.Context, id string) (string, error) {
	return p.s.WorkerOwner(ctx, id)
}

func (h *testHub) dataDir() string { return h.svc.dataDir }

func TestAnUnreadableChainRefusesTheConnectBeforeTheUpgrade(t *testing.T) {
	h := newTestHub(t, func(o *ServiceOptions) {
		o.Chain = func(context.Context) ([]identity.Transition, error) { return nil, errors.New("database is locked") }
	})
	key := newKey(t)
	h.enrol(key, "owner-1", "pi")
	began := time.Now()
	res, err := h.dial(h.workerConfig(key))
	if err == nil {
		_ = res.Channel.Close()
		t.Fatal("the Worker connected although the Hub could not read its chain")
	}
	if time.Since(began) > 3*time.Second {
		t.Errorf("the refusal took %s", time.Since(began))
	}
	if !strings.Contains(h.logs.String(), "cannot read the transition chain") || strings.Contains(h.logs.String(), "database is locked\n\n") {
		t.Errorf("logs = %s", h.logs.String())
	}
}

// blockIdentityFile makes identity.key impossible to replace by a rename (a
// directory stands in its place) and returns a function that undoes it by
// putting the original content back.
func blockIdentityFile(t *testing.T, dir string) (unblock func()) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("renaming a file over a directory behaves differently on Windows")
	}
	path := filepath.Join(dir, IdentityFile)
	saved, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	return func() {
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, saved, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// A rotation that committed but could not replace identity.key leaves the
// current key only in identity.key.next. A later rotation must neither
// overwrite nor delete it.
func TestACommittedPendingKeyIsNeverLostToALaterRotation(t *testing.T) {
	dir := t.TempDir()
	st := openStore(t, dir)
	var logs syncBuf
	svc, err := NewService(context.Background(), ServiceOptions{DataDir: dir, Store: st, Logger: textLogger(&logs)})
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	r := svc.Rotator()
	unblock := blockIdentityFile(t, dir)

	res, err := r.Rotate(context.Background(), RotateActor{ID: "a"})
	if err == nil || res.Seq != 1 {
		t.Fatalf("first rotation: res = %+v, err = %v; want committed with a follow-up error", res, err)
	}
	current := svc.Identity().Key()
	pendingPath := filepath.Join(dir, PendingKeyFile)
	pending, err := identity.LoadKey(pendingPath)
	if err != nil || !bytes.Equal(pending.Public(), current.Public()) {
		t.Fatalf("pending key = %v, %v; want the current key", pending, err)
	}

	// While the file cannot be replaced, no new rotation starts and the
	// pending file is untouched.
	if res, err := r.Rotate(context.Background(), RotateActor{ID: "a"}); err == nil || res.Seq != 0 {
		t.Fatalf("second rotation: res = %+v, err = %v; want a refusal", res, err)
	}
	if chain, _ := ChainSource(st)(context.Background()); len(chain) != 1 {
		t.Fatalf("chain has %d links after a refused rotation", len(chain))
	}
	if again, err := identity.LoadKey(pendingPath); err != nil || !bytes.Equal(again.Public(), current.Public()) {
		t.Fatalf("pending key after the refusal = %v, %v", again, err)
	}

	// Once the file can be replaced, the next rotation completes the pending
	// one first and then proceeds.
	unblock()
	if res, err := r.Rotate(context.Background(), RotateActor{ID: "a"}); err != nil || res.Seq != 2 {
		t.Fatalf("third rotation: res = %+v, err = %v", res, err)
	}
	if _, err := os.Stat(pendingPath); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("pending file left: %v", err)
	}
	last := svc.Identity().Public()
	svc.Close()
	svc2, err := NewService(context.Background(), ServiceOptions{DataDir: dir, Store: st, Logger: textLogger(&logs)})
	if err != nil {
		t.Fatalf("the Hub does not start after the rotations: %v", err)
	}
	defer svc2.Close()
	if !bytes.Equal(svc2.Identity().Public(), last) {
		t.Error("the restarted Hub has another identity")
	}
}

// A failed rotation after a committed one with a pending file must keep that
// file: it still holds the only copy of the current key.
func TestAFailedRotationKeepsACommittedPendingKey(t *testing.T) {
	dir := t.TempDir()
	st := openStore(t, dir)
	var logs syncBuf
	svc, err := NewService(context.Background(), ServiceOptions{DataDir: dir, Store: st, Logger: textLogger(&logs)})
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	unblock := blockIdentityFile(t, dir)
	if _, err := svc.Rotator().Rotate(context.Background(), RotateActor{ID: "a"}); err == nil {
		t.Fatal("want a follow-up error")
	}
	current := svc.Identity().Public()
	unblock() // the file can be replaced again, but the store now refuses

	failing := &failingStore{RotationStore: st}
	r2, err := NewRotator(RotatorOptions{Identity: svc.Identity(), DataDir: dir, Store: failing, Logger: textLogger(&logs)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r2.Rotate(context.Background(), RotateActor{ID: "a"}); err == nil {
		t.Fatal("the rotation must fail")
	}
	svc.Close()
	// identity.key is the current key (completed before the failing attempt)
	// and the Hub starts.
	svc2, err := NewService(context.Background(), ServiceOptions{DataDir: dir, Store: st, Logger: textLogger(&logs)})
	if err != nil {
		t.Fatalf("restart after the failed rotation: %v", err)
	}
	defer svc2.Close()
	if !bytes.Equal(svc2.Identity().Public(), current) {
		t.Error("the restarted Hub lost the current key")
	}
}

// failingStore fails the commit of every rotation.
type failingStore struct{ RotationStore }

func (f *failingStore) RotateHubIdentity(context.Context, store.RotateParams) error {
	return errors.New("disk full")
}

// flushStore wraps the real store and lets a test decide whether the commit
// of a rotation can be proven durable. The commit itself is real.
type flushStore struct {
	*store.Store
	failFlush atomic.Bool // MakeDurable (and the check inside RotateHubIdentity) fail
	// onCommit runs after a rotation committed and before its durability
	// result is returned; tests cancel the request context there.
	onCommit func()
	// flushErr is the context error that each MakeDurable call saw.
	mu       sync.Mutex
	flushCtx []error
}

func (f *flushStore) RotateHubIdentity(ctx context.Context, p store.RotateParams) error {
	if err := f.Store.RotateHubIdentity(ctx, p); err != nil {
		return err
	}
	if f.onCommit != nil {
		f.onCommit()
	}
	if ctx.Err() != nil {
		return fmt.Errorf("%w: %v", store.ErrCommitNotDurable, ctx.Err())
	}
	if f.failFlush.Load() {
		return fmt.Errorf("%w: simulated", store.ErrCommitNotDurable)
	}
	return nil
}

func (f *flushStore) MakeDurable(ctx context.Context) error {
	f.mu.Lock()
	f.flushCtx = append(f.flushCtx, ctx.Err())
	f.mu.Unlock()
	if f.failFlush.Load() {
		return fmt.Errorf("%w: simulated", store.ErrCommitNotDurable)
	}
	return f.Store.MakeDurable(ctx)
}

// unflushedRotation sets up a Hub whose next rotation commits but cannot be
// proven durable, and returns what a power loss would leave behind: the
// database file as it was on disk before the rotation.
type unflushedRotation struct {
	dir    string
	st     *store.Store
	fs     *flushStore
	svc    *Service
	logs   *syncBuf
	oldPub []byte
	disk   []byte // minion.db before the rotation (what survives a power loss)
}

func newUnflushed(t *testing.T) *unflushedRotation {
	t.Helper()
	dir := t.TempDir()
	st := openStore(t, dir)
	if err := st.MakeDurable(context.Background()); err != nil {
		t.Fatal(err)
	}
	disk, err := os.ReadFile(filepath.Join(dir, store.DBFile))
	if err != nil {
		t.Fatal(err)
	}
	u := &unflushedRotation{dir: dir, st: st, fs: &flushStore{Store: st}, logs: &syncBuf{}, disk: disk}
	svc, err := NewService(context.Background(), ServiceOptions{DataDir: dir, Store: u.fs, Logger: textLogger(u.logs)})
	if err != nil {
		t.Fatal(err)
	}
	u.svc = svc
	t.Cleanup(svc.Close)
	// NewService recorded the identity; the pre-rotation disk must hold it.
	if err := st.MakeDurable(context.Background()); err != nil {
		t.Fatal(err)
	}
	if u.disk, err = os.ReadFile(filepath.Join(dir, store.DBFile)); err != nil {
		t.Fatal(err)
	}
	u.oldPub = bytes.Clone(svc.Identity().Public())
	return u
}

// powerLoss closes the Hub and puts the database back as it was on disk before
// the rotation, without its WAL, as a power loss would.
func (u *unflushedRotation) powerLoss(t *testing.T) *store.Store {
	t.Helper()
	u.svc.Close()
	_ = u.st.Close()
	for _, suffix := range []string{"-wal", "-shm"} {
		if err := os.Remove(filepath.Join(u.dir, store.DBFile+suffix)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(u.dir, store.DBFile), u.disk, 0o600); err != nil {
		t.Fatal(err)
	}
	return openStore(t, u.dir)
}

func (u *unflushedRotation) identityKeyPub(t *testing.T) []byte {
	t.Helper()
	id, err := ReadIdentity(u.dir)
	if err != nil {
		t.Fatal(err)
	}
	return id.Public()
}

// A commit that cannot be proven durable must not be published: the Hub keeps
// signing with the old key, identity.key is untouched, and a power loss that
// brings back the old database leaves a Hub that starts with the old key.
func TestAnUnconfirmedCommitIsNotPublishedAndASimulatedPowerLossLeavesAWorkingHub(t *testing.T) {
	u := newUnflushed(t)
	u.fs.failFlush.Store(true)

	res, err := u.svc.Rotator().Rotate(context.Background(), RotateActor{ID: "a"})
	if !errors.Is(err, ErrRotationNotDurable) || res.Seq != 0 {
		t.Fatalf("res = %+v, err = %v; want ErrRotationNotDurable and no result", res, err)
	}
	if !bytes.Equal(u.svc.Identity().Public(), u.oldPub) {
		t.Fatal("the Hub signs with a key whose rotation is not durable")
	}
	if !bytes.Equal(u.identityKeyPub(t), u.oldPub) {
		t.Fatal("identity.key was replaced before the commit was durable")
	}
	if _, err := os.Stat(filepath.Join(u.dir, PendingKeyFile)); err != nil {
		t.Fatalf("pending key: %v", err)
	}

	// A retry that still cannot prove durability changes nothing either.
	if _, err := u.svc.Rotator().Rotate(context.Background(), RotateActor{ID: "a"}); !errors.Is(err, ErrRotationNotDurable) {
		t.Fatalf("retry err = %v", err)
	}
	if !bytes.Equal(u.svc.Identity().Public(), u.oldPub) || !bytes.Equal(u.identityKeyPub(t), u.oldPub) {
		t.Fatal("a retry published or replaced the key without proof")
	}
	if _, err := os.Stat(filepath.Join(u.dir, PendingKeyFile)); err != nil {
		t.Fatalf("the retry lost the pending key: %v", err)
	}

	// The power loss: the old database comes back.
	st2 := u.powerLoss(t)
	svc2, err := NewService(context.Background(), ServiceOptions{DataDir: u.dir, Store: st2, Logger: textLogger(u.logs)})
	if err != nil {
		t.Fatalf("the Hub does not start after the lost commit: %v", err)
	}
	defer svc2.Close()
	if !bytes.Equal(svc2.Identity().Public(), u.oldPub) {
		t.Error("the restarted Hub has another identity")
	}
	if chain, _ := ChainSource(st2)(context.Background()); len(chain) != 0 {
		t.Errorf("the lost rotation left %d chain links", len(chain))
	}
	if _, err := os.Stat(filepath.Join(u.dir, PendingKeyFile)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the abandoned pending key stayed: %v", err)
	}
}

// While a commit is unconfirmed the database holds the new link, but a Worker
// pinned to the old key must still connect: the Hub shows the chain only up to
// the key it uses.
func TestAWorkerStillConnectsWhileACommitIsUnconfirmedAndFollowsOnceConfirmed(t *testing.T) {
	var fs *flushStore
	h := newTestHub(t, func(o *ServiceOptions) {
		fs = &flushStore{Store: o.Store.(*store.Store)}
		o.Store = fs
	})
	key := newKey(t)
	h.enrol(key, "owner-1", "pi")
	oldPin := h.pin()
	fs.failFlush.Store(true)
	if _, err := h.svc.Rotator().Rotate(context.Background(), RotateActor{ID: "a"}); !errors.Is(err, ErrRotationNotDurable) {
		t.Fatalf("err = %v", err)
	}
	if rows, _ := h.st.Transitions(context.Background()); len(rows) != 1 {
		t.Fatalf("the database has %d links, want the unconfirmed one", len(rows))
	}
	cfg := h.workerConfig(key)
	cfg.Pin = oldPin
	res, err := h.dial(cfg)
	if err != nil {
		t.Fatalf("a Worker pinned to the old key cannot connect while the commit is unconfirmed: %v", err)
	}
	if !bytes.Equal(res.Pin.Pub, oldPin.Pub) || res.Pin.Seq != 0 {
		t.Errorf("the Worker was moved to %x seq %d before the commit was durable", res.Pin.Pub, res.Pin.Seq)
	}
	_ = res.Channel.Close()

	// Durability is confirmed by the retry, which completes the same rotation.
	fs.failFlush.Store(false)
	done, err := h.svc.Rotator().Rotate(context.Background(), RotateActor{ID: "a"})
	if err != nil || done.Seq != 1 {
		t.Fatalf("retry: res = %+v, err = %v; want the first rotation completed", done, err)
	}
	if rows, _ := h.st.Transitions(context.Background()); len(rows) != 1 {
		t.Fatalf("the retry rotated again: %d links", len(rows))
	}
	cfg.Pin = oldPin
	res, err = h.dial(cfg)
	if err != nil {
		t.Fatalf("a Worker pinned to the old key cannot follow the confirmed rotation: %v", err)
	}
	if !bytes.Equal(res.Pin.Pub, h.svc.Identity().Public()) || res.Pin.Seq != 1 {
		t.Errorf("the Worker pin = %x seq %d, want the new key at seq 1", res.Pin.Pub, res.Pin.Seq)
	}
	_ = res.Channel.Close()
	onDisk, err := ReadIdentity(h.dataDir())
	if err != nil || !bytes.Equal(onDisk.Public(), h.svc.Identity().Public()) {
		t.Fatalf("identity.key after the confirmed rotation = %v, %v", onDisk, err)
	}
}

// The request is canceled between COMMIT and the checkpoint: the confirmation
// runs detached from it, so the rotation is published only after it succeeded.
func TestACancellationAfterTheCommitDoesNotPublishBeforeTheDurabilityIsConfirmed(t *testing.T) {
	u := newUnflushed(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	u.fs.onCommit = cancel
	res, err := u.svc.Rotator().Rotate(ctx, RotateActor{ID: "a"})
	if err != nil || res.Seq != 1 {
		t.Fatalf("res = %+v, err = %v", res, err)
	}
	u.fs.mu.Lock()
	calls := slices.Clone(u.fs.flushCtx)
	u.fs.mu.Unlock()
	if len(calls) == 0 {
		t.Fatal("the rotation was published without confirming durability again")
	}
	for _, e := range calls {
		if e != nil {
			t.Errorf("the durability check ran on a canceled context: %v", e)
		}
	}
	if bytes.Equal(u.svc.Identity().Public(), u.oldPub) || !bytes.Equal(u.identityKeyPub(t), u.svc.Identity().Public()) {
		t.Error("the confirmed rotation is not in force")
	}
}

// A restart finds the record of a rotation in the database and the new key in
// identity.key.next: identity.key is replaced only once the record is durable.
func TestRecoveryReplacesTheKeyFileOnlyOnceTheRecordIsDurable(t *testing.T) {
	u := newUnflushed(t)
	u.fs.failFlush.Store(true)
	if _, err := u.svc.Rotator().Rotate(context.Background(), RotateActor{ID: "a"}); !errors.Is(err, ErrRotationNotDurable) {
		t.Fatalf("err = %v", err)
	}
	u.svc.Close() // a crash right after the commit: record visible, key pending

	if _, err := NewService(context.Background(), ServiceOptions{DataDir: u.dir, Store: u.fs, Logger: textLogger(u.logs)}); err == nil ||
		!strings.Contains(err.Error(), "durable") {
		t.Fatalf("start with an unflushable record: err = %v; want a refusal", err)
	}
	if !bytes.Equal(u.identityKeyPub(t), u.oldPub) {
		t.Fatal("identity.key was replaced for a record that is not durable")
	}
	u.fs.failFlush.Store(false)
	svc2, err := NewService(context.Background(), ServiceOptions{DataDir: u.dir, Store: u.fs, Logger: textLogger(u.logs)})
	if err != nil {
		t.Fatal(err)
	}
	defer svc2.Close()
	if bytes.Equal(svc2.Identity().Public(), u.oldPub) || !bytes.Equal(u.identityKeyPub(t), svc2.Identity().Public()) {
		t.Error("the recovered Hub does not use the new key")
	}
}

func TestChainUpToCutsTheLinksPastTheKeyInUse(t *testing.T) {
	a, b, c := newKey(t), newKey(t), newKey(t)
	t1, err := identity.NewTransition(a, b.Public(), 1, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	t2, err := identity.NewTransition(b, c.Public(), 2, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	chain := []identity.Transition{t1, t2}
	for _, tc := range []struct {
		name string
		pub  []byte
		want int
	}{{"first key", a.Public(), 0}, {"middle key", b.Public(), 1}, {"last key", c.Public(), 2}, {"unknown key", newKey(t).Public(), 2}} {
		if got := chainUpTo(chain, tc.pub); len(got) != tc.want {
			t.Errorf("%s: %d links, want %d", tc.name, len(got), tc.want)
		}
	}
	if got := chainUpTo(nil, a.Public()); len(got) != 0 {
		t.Errorf("empty chain: %d links", len(got))
	}
}
