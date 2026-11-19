//go:build unix

package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/worker/localapi"
	"github.com/patrick-1984/ai-minion/internal/worker/store"
)

// cancelPairing sends worker_pair_cancel as "minion worker pair cancel" does.
func (w *pairingWorker) cancelPairing(t *testing.T, attempt string) (PairCancelResult, error) {
	t.Helper()
	raw, err := w.sock.Call(context.Background(), localapi.QualifiedOp(OpLocalPairCancel, []string{FeaturePairCancel}),
		LocalPairCancelArgs{Attempt: attempt})
	if err != nil {
		return PairCancelResult{}, err
	}
	var res PairCancelResult
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatal(err)
	}
	return res, nil
}

// wantStateChanged checks the refusal that the CLI maps to exit 27.
func wantStateChanged(t *testing.T, err error) {
	t.Helper()
	var pe *proto.Error
	if !errors.As(err, &pe) || pe.Code != proto.CodeConflict {
		t.Fatalf("err = %v, want conflict/state_changed", err)
	}
	var det struct {
		Reason string `json:"reason"`
	}
	_ = json.Unmarshal(pe.Details, &det)
	if det.Reason != "state_changed" {
		t.Fatalf("reason = %q (%v)", det.Reason, err)
	}
}

func (w *pairingWorker) stop(t *testing.T) {
	t.Helper()
	if err := w.rt.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// openStateStore opens the stopped Worker's database.
func openStateStore(t *testing.T, dir string) *store.Store {
	t.Helper()
	st, err := store.OpenContext(context.Background(), dir, store.NoMigrate())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func writeConfig(t *testing.T, dir string, kv map[string]string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	f, err := config.LoadFile(filepath.Join(dir, WorkerConfigFile))
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range kv {
		f.Set(k, v)
	}
	if err := f.Save(); err != nil {
		t.Fatal(err)
	}
}

func readConfig(t *testing.T, dir string, key string) (string, bool) {
	t.Helper()
	f, err := config.LoadFile(filepath.Join(dir, WorkerConfigFile))
	if err != nil {
		t.Fatal(err)
	}
	v, ok, err := f.Lookup(key)
	if err != nil {
		t.Fatal(err)
	}
	return v, ok
}

func TestCancelPairingKeepsKeyAndShare(t *testing.T) {
	dir := t.TempDir()
	writeConfig(t, dir, map[string]string{config.KeyShareExisting: "all"})
	w := startPairingWorkerIn(t, newStubPairHub(t), dir, config.PairingDisplayStatus)
	p := w.pendingCode(t, "")
	key, err := os.ReadFile(filepath.Join(dir, WorkerKeyFile))
	if err != nil {
		t.Fatal(err)
	}

	res, err := w.cancelPairing(t, p.Attempt)
	if err != nil || !res.Cancelled || res.Attempt != p.Attempt {
		t.Fatalf("cancel = %+v, %v", res, err)
	}
	if got := w.statusPairing(t); got.State != PairingNone || got.Code != "" {
		t.Fatalf("status after cancel = %+v", got)
	}
	if after, err := os.ReadFile(filepath.Join(dir, WorkerKeyFile)); err != nil || !bytes.Equal(after, key) {
		t.Fatalf("the Worker key changed or went (%v)", err)
	}
	if v, ok := readConfig(t, dir, config.KeyShareExisting); !ok || v != "all" {
		t.Fatalf("share_existing = %q, %v", v, ok)
	}
	// The code cannot pair any more: the Hub that gets it waits in vain.
	n := w.hub.idCount()
	time.Sleep(300 * time.Millisecond)
	if w.hub.idCount() != n || w.hub.welcomed.Load() != 0 {
		t.Fatalf("the Worker kept pairing after the cancel (ids %d -> %d)", n, w.hub.idCount())
	}
	if !strings.Contains(w.logs.String(), "cancelled by the owner") {
		t.Fatalf("log:\n%s", w.logs.String())
	}
}

func TestCancelClearsOnlyBackgroundJoinServer(t *testing.T) {
	const server = "https://hub.example:8443"
	fp := "SHA256:" + strings.Repeat("A", 43)
	other := "SHA256:" + strings.Repeat("B", 43)
	created := JoinPin{Version: JoinPinVersion, Source: JoinPinSourceBackground, Server: server, ServerCreated: true,
		HubFingerprint: fp, HubFingerprintCreated: true, At: time.Now()}
	for _, tc := range []struct {
		name          string
		file          map[string]string
		pin           *JoinPin
		cleared, kept []string
	}{
		{name: "created and unchanged", pin: &created,
			file:    map[string]string{config.KeyServer: server, config.KeyHubFingerprint: fp, config.KeyPairingDisplay: "status"},
			cleared: []string{config.KeyHubFingerprint, config.KeyPairingDisplay, config.KeyServer}},
		{name: "provisioned fingerprint", pin: func() *JoinPin { p := created; p.HubFingerprintCreated = false; return &p }(),
			file:    map[string]string{config.KeyServer: server, config.KeyHubFingerprint: fp, config.KeyPairingDisplay: "status"},
			cleared: []string{config.KeyPairingDisplay, config.KeyServer}, kept: []string{config.KeyHubFingerprint}},
		{name: "edited after the join", pin: &created,
			file:    map[string]string{config.KeyServer: "https://other.example", config.KeyHubFingerprint: other, config.KeyPairingDisplay: "status"},
			cleared: []string{config.KeyPairingDisplay}, kept: []string{config.KeyHubFingerprint, config.KeyServer}},
		{name: "no join record",
			file: map[string]string{config.KeyServer: server, config.KeyHubFingerprint: fp, config.KeyPairingDisplay: "status"},
			kept: []string{config.KeyHubFingerprint, config.KeyServer}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeConfig(t, dir, tc.file)
			if tc.pin != nil {
				if err := WriteJoinPin(dir, *tc.pin); err != nil {
					t.Fatal(err)
				}
			}
			cleared, kept, err := clearBackgroundJoinConfig(dir)
			if err != nil || strings.Join(cleared, ",") != strings.Join(tc.cleared, ",") || strings.Join(kept, ",") != strings.Join(tc.kept, ",") {
				t.Fatalf("cleared %v kept %v (%v), want %v and %v", cleared, kept, err, tc.cleared, tc.kept)
			}
			for k, v := range tc.file {
				got, ok := readConfig(t, dir, k)
				removed := strings.Contains(","+strings.Join(tc.cleared, ",")+",", ","+k+",")
				if removed == ok || (ok && got != v) {
					t.Fatalf("%s = %q, %v after the cancel", k, got, ok)
				}
			}
			// The record goes once something it describes was removed.
			if _, ok, _ := ReadJoinPin(dir); ok {
				t.Fatal("the join record is still there")
			}
		})
	}

	// Through the running Worker: the values the background join saved go.
	h := newStubPairHub(t)
	dir := t.TempDir()
	pin := created
	pin.Server, pin.HubFingerprintCreated, pin.HubFingerprint = h.url(), false, ""
	writeConfig(t, dir, map[string]string{config.KeyServer: h.url(), config.KeyPairingDisplay: "status"})
	if err := WriteJoinPin(dir, pin); err != nil {
		t.Fatal(err)
	}
	w := startPairingWorkerIn(t, h, dir, config.PairingDisplayStatus)
	res, err := w.cancelPairing(t, w.pendingCode(t, "").Attempt)
	if err != nil || strings.Join(res.Cleared, ",") != "pairing_display,server" || len(res.Kept) != 0 {
		t.Fatalf("cancel = %+v, %v", res, err)
	}
	if _, ok := readConfig(t, dir, config.KeyServer); ok {
		t.Fatal("the server the background join saved is still there")
	}
	if _, ok, _ := ReadJoinPin(dir); ok {
		t.Fatal("the join record outlived the values it described")
	}
}

// Completion first: the cancel is refused (27) and the enrollment stays.
func TestCancelRacesCompletion(t *testing.T) {
	t.Run("completion first", func(t *testing.T) {
		w := startPairingWorker(t, config.PairingDisplayStatus)
		p := w.pendingCode(t, "")
		eventually(t, func() bool { return w.hub.lastID() == p.Code[3:7] })
		w.hub.claim(p.Code)
		eventually(t, func() bool { return w.statusPairing(t).State == PairingNone && w.hub.welcomed.Load() == 1 })
		_, err := w.cancelPairing(t, p.Attempt)
		wantStateChanged(t, err)
		w.stop(t)
		enr, ok, err := NewEnrollments(openStateStore(t, w.dir), w.dir).Load(context.Background())
		if err != nil || !ok || enr.WorkerID == "" {
			t.Fatalf("enrollment after the refused cancel = %+v, %v, %v", enr, ok, err)
		}
		if _, ok, _ := ReadPairCancel(w.dir); ok {
			t.Fatal("a refused cancel saved a cancel record")
		}
	})

	// Cancel first: the Hub's welcome arrives, the commit waits for the
	// enrollment lock that the cancel holds, and stores nothing afterwards.
	t.Run("cancel first", func(t *testing.T) {
		h := newStubPairHub(t)
		hp := newHubPairer(t, h.url())
		locked := make(chan func(), 1)
		h.afterAuth = func() {
			release, err := hp.enr.lockEnroll(context.Background())
			if err != nil {
				t.Error(err)
			}
			locked <- release
		}
		ctx, cancel := context.WithCancelCause(context.Background())
		defer cancel(nil)
		done := make(chan error, 1)
		go func() { _, err := hp.Run(ctx); done <- err }()
		h.claim(hp.code(t, 1))
		release := <-locked
		eventually(t, func() bool { return h.welcomed.Load() == 1 })
		time.Sleep(100 * time.Millisecond) // the commit waits for the lock
		cancel(errPairingCancelled)
		release()
		select {
		case err := <-done:
			if !errors.Is(err, errPairingCancelled) {
				t.Fatalf("run = %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("the run did not end")
		}
		if _, ok, _ := hp.enr.Load(context.Background()); ok {
			t.Fatal("a cancelled pairing was committed")
		}
	})
}

func TestCancelStaleAttempt(t *testing.T) {
	w := startPairingWorker(t, config.PairingDisplayStatus)
	first := w.pendingCode(t, "")
	eventually(t, func() bool { return w.hub.lastID() == first.Code[3:7] })
	w.hub.claim(first.Code[:8] + otherChar(first.Code[8]) + first.Code[9:])
	second := w.pendingCode(t, first.Code)

	_, err := w.cancelPairing(t, first.Attempt)
	wantStateChanged(t, err)
	_, err = w.cancelPairing(t, "0123456789abcdef")
	wantStateChanged(t, err)
	if got := w.statusPairing(t); got.State != PairingPending || got.Attempt != second.Attempt {
		t.Fatalf("a stale cancel changed the pairing: %+v", got)
	}
	if _, ok, _ := ReadPairCancel(w.dir); ok {
		t.Fatal("a stale cancel saved a cancel record")
	}
	if _, err := w.cancelPairing(t, second.Attempt); err != nil {
		t.Fatalf("cancel of the current attempt = %v", err)
	}
}

// After the cancel no resume record remains, and the restarted Worker does
// not pair again.
func TestCancelLeavesNoResumeAndNoPairingOnRestart(t *testing.T) {
	h := newStubPairHub(t)
	authed := make(chan struct{}, 1)
	h.afterAuth = func() { authed <- struct{}{} }
	h.dropBeforeWelcome.Store(1) // the Hub authenticated, the welcome never comes
	dir := t.TempDir()
	w := startPairingWorkerIn(t, h, dir, config.PairingDisplayStatus)
	p := w.pendingCode(t, "")
	eventually(t, func() bool { return h.lastID() == p.Code[3:7] })
	h.claim(p.Code)
	<-authed
	// The Worker resumes with the same code: the record exists.
	eventually(t, func() bool {
		_, err := openStateStore(t, dir).LoadPairResume(context.Background())
		return err == nil
	})

	res, err := w.cancelPairing(t, p.Attempt)
	if err != nil || !res.Cancelled || !res.ResumeDeleted {
		t.Fatalf("cancel = %+v, %v", res, err)
	}
	w.stop(t)
	if _, err := openStateStore(t, dir).LoadPairResume(context.Background()); !errors.Is(err, store.ErrNoPairResume) {
		t.Fatalf("resume record after the cancel: %v", err)
	}
	if rec, ok, err := ReadPairCancel(dir); err != nil || !ok || rec.Server != h.url() {
		t.Fatalf("cancel record = %+v, %v, %v", rec, ok, err)
	}

	ids := h.idCount()
	w2 := startPairingWorkerIn(t, h, dir, config.PairingDisplayStatus)
	eventually(t, func() bool { return strings.Contains(w2.logs.String(), "the pairing with this Hub was cancelled") })
	time.Sleep(300 * time.Millisecond)
	if got := w2.statusPairing(t); got.State != PairingNone || h.idCount() != ids {
		t.Fatalf("the restarted Worker paired again: %+v, ids %d -> %d", got, ids, h.idCount())
	}

	// A reset (or a new background join) asks for pairing again.
	if err := RemovePairCancel(dir); err != nil {
		t.Fatal(err)
	}
	w2.stop(t)
	w3 := startPairingWorkerIn(t, h, dir, config.PairingDisplayStatus)
	if p3 := w3.pendingCode(t, ""); p3.Code == p.Code {
		t.Fatal("the cancelled code came back")
	}
}
