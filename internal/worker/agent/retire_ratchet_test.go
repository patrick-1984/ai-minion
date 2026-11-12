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

	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/transport"
)

func loadRetired(t *testing.T, rs *RetiredStore) RetiredIdentity {
	t.Helper()
	l, err := rs.List()
	if err != nil || len(l) != 1 {
		t.Fatalf("pending = %d, %v", len(l), err)
	}
	return l[0]
}

func retiredWithRatchet(t *testing.T, h *retireHub, max uint64, hash []byte) (RetiredIdentity, *identity.Key, *RetiredStore) {
	t.Helper()
	rec, old := retiredFor(t, h, h.url())
	rec.MaxSerial, rec.CertSHA256 = max, hash
	rs := NewRetiredStore(t.TempDir())
	if err := rs.Add(rec); err != nil {
		t.Fatal(err)
	}
	return rec, old, rs
}

func TestRetiredMetadataIsVersionTwoAndCarriesTheRatchet(t *testing.T) {
	h := newRetireHub(t)
	hash := bytes.Repeat([]byte{4}, 32)
	rec, _, rs := retiredWithRatchet(t, h, 17, hash)
	_, metaPath := rs.paths(rec.Fingerprint())
	raw, err := os.ReadFile(metaPath)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil || m["version"] != float64(2) || m["max_serial"] != float64(17) || m["cert_sha256"] == "" {
		t.Fatalf("metadata = %s (%v)", raw, err)
	}
	got := loadRetired(t, rs)
	if got.MaxSerial != 17 || !bytes.Equal(got.CertSHA256, hash) || !got.RatchetSeeded {
		t.Fatalf("loaded = %+v", got)
	}
}

func TestIdentityReplacementCarriesTheRatchetAndFailedAddAbortsBeforeTheWipe(t *testing.T) {
	f := newPairFixture(t)
	if _, err := f.enr.LoadOrCreateKey(); err != nil {
		t.Fatal(err)
	}
	hash := bytes.Repeat([]byte{9}, 32)
	enr := Enrollment{Pin: identity.Pin{Pub: bytes.Repeat([]byte{3}, 32)}, Owner: "alice", HubURL: "https://old-hub.example",
		Ratchet: transport.Ratchet{Root: bytes.Repeat([]byte{3}, 32), MaxSerial: 33, CertSHA256: hash}}
	if err := f.enr.Commit(context.Background(), enr); err != nil {
		t.Fatal(err)
	}
	// An Add that fails (the retired directory cannot be created) aborts the
	// replacement before the wipe: the enrollment and the ratchet stay.
	blocker := filepath.Join(f.enr.stateDir, retiredDirName)
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := RenewWorkerIdentity(context.Background(), f.enr); err == nil {
		t.Fatal("the replacement succeeded although the old identity could not be kept")
	}
	got, ok, _ := f.enr.Load(context.Background())
	if !ok || got.Ratchet.MaxSerial != 33 {
		t.Fatalf("the enrollment was wiped despite the failed Add: %+v ok %v", got, ok)
	}
	if err := os.Remove(blocker); err != nil {
		t.Fatal(err)
	}
	if _, err := RenewWorkerIdentity(context.Background(), f.enr); err != nil {
		t.Fatal(err)
	}
	pending := loadRetired(t, f.enr.RetiredStore())
	if pending.MaxSerial != 33 || !bytes.Equal(pending.CertSHA256, hash) {
		t.Fatalf("the ratchet was not carried: %+v", pending)
	}
	if _, ok, _ := f.enr.Load(context.Background()); ok {
		t.Fatal("the enrollment was not wiped")
	}
}

func TestAStaleCertificateCannotAnswerNotEnrolledAndTheEntryStaysPending(t *testing.T) {
	for _, tc := range []struct {
		name       string
		hubSerial  uint64
		max        uint64
		sameSerial bool
		wantReason string
	}{
		{"serial below the carried max_serial", 5, 10, false, "cert_serial_too_low"},
		{"equal serial with another hash", 10, 10, true, "cert_serial_conflict"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newRetireHub(t)
			h.hub.Reissue(t, tc.hubSerial)
			_, _, rs := retiredWithRatchet(t, h, tc.max, bytes.Repeat([]byte{1}, 32))
			// the old key is unknown to the Hub: an honest answer is not_enrolled
			remaining, err := newRetirer(t, rs, h.url()).RunOnce(context.Background())
			if err != nil || remaining != 1 {
				t.Fatalf("RunOnce = %d, %v: the entry must stay pending", remaining, err)
			}
			got := loadRetired(t, rs)
			if got.MaxSerial != tc.max {
				t.Fatalf("a refused certificate advanced the ratchet to %d", got.MaxSerial)
			}
			if h.connects.Load() == 0 {
				t.Fatal("the Hub was never reached")
			}
		})
	}
}

func TestAValidHigherSerialAdvancesTheRatchetDurablyBeforeTheWorkerActs(t *testing.T) {
	h := newRetireHub(t)
	h.hub.Reissue(t, 8)
	rec, old, rs := retiredWithRatchet(t, h, 3, bytes.Repeat([]byte{1}, 32))
	h.enrol(old.Public())

	// A failed advance ends the connection and keeps the entry: the retirement
	// call was not even sent.
	rs.write = func(string, []byte) error { return errors.New("disk full") }
	remaining, err := newRetirer(t, rs, h.url()).RunOnce(context.Background())
	if err != nil || remaining != 1 {
		t.Fatalf("RunOnce = %d, %v", remaining, err)
	}
	h.mu.Lock()
	retired := h.retired
	h.mu.Unlock()
	if retired != 0 {
		t.Fatal("the Worker acted although the ratchet could not be stored")
	}
	if got := loadRetired(t, rs); got.MaxSerial != 3 {
		t.Fatalf("ratchet = %d after a failed write", got.MaxSerial)
	}

	// The advance is on disk when the action then fails (a crash between the
	// advance and the action keeps the advance).
	rs.write = identity.WritePrivate
	h.setUnsupported(true)
	if remaining, _ := newRetirer(t, rs, h.url()).RunOnce(context.Background()); remaining != 1 {
		t.Fatal("an unsupported Hub must keep the entry")
	}
	got := loadRetired(t, rs)
	if got.MaxSerial != 8 || !bytes.Equal(got.CertSHA256, hashOf(h.hub.Cert)) {
		t.Fatalf("advanced ratchet = %d %x", got.MaxSerial, got.CertSHA256)
	}
	_ = rec
	h.setUnsupported(false)
	if remaining, _ := newRetirer(t, rs, h.url()).RunOnce(context.Background()); remaining != 0 {
		t.Fatal("the retirement must complete with the validated certificate")
	}
}

func writeV1Retired(t *testing.T, rs *RetiredStore, rec RetiredIdentity) {
	t.Helper()
	if err := os.MkdirAll(rs.dir, 0o700); err != nil {
		t.Fatal(err)
	}
	keyPath, metaPath := rs.paths(rec.Fingerprint())
	if err := identity.SaveKey(keyPath, rec.Key); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]any{
		"version": 1, "hub_url": rec.HubURL, "hub_pub": identity.B64(rec.Pin.Pub), "pin_seq": rec.Pin.Seq,
		"worker_id": rec.WorkerID, "retired_at": rec.RetiredAt,
	})
	if err := os.WriteFile(metaPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestAVersionOneEntryIsSeededByItsFirstValidatedConnectionAndRetiredByALaterOne(t *testing.T) {
	h := newRetireHub(t)
	h.hub.Reissue(t, 6)
	rec, _ := retiredFor(t, h, h.url())
	rs := NewRetiredStore(t.TempDir())
	writeV1Retired(t, rs, rec)
	if got := loadRetired(t, rs); got.RatchetSeeded || got.MaxSerial != 0 {
		t.Fatalf("v1 load = %+v", got)
	}
	// The Hub does not know the old key: not_enrolled. The first validated
	// connection must not retire the entry.
	remaining, err := newRetirer(t, rs, h.url()).RunOnce(context.Background())
	if err != nil || remaining != 1 {
		t.Fatalf("first connection: remaining %d err %v, want the entry pending", remaining, err)
	}
	seeded := loadRetired(t, rs)
	if !seeded.RatchetSeeded || seeded.MaxSerial != 6 {
		t.Fatalf("not seeded: %+v", seeded)
	}
	_, metaPath := rs.paths(rec.Fingerprint())
	raw, _ := os.ReadFile(metaPath)
	if !strings.Contains(string(raw), `"version":2`) {
		t.Fatalf("the migration did not rewrite version 2: %s", raw)
	}
	// A later validated connection (serial >= the seed) may retire it.
	if remaining, err := newRetirer(t, rs, h.url()).RunOnce(context.Background()); err != nil || remaining != 0 {
		t.Fatalf("later connection: remaining %d err %v", remaining, err)
	}
}

func TestAVersionOneEntryCanCompleteOnTheFirstConnectionThroughASignedRevoke(t *testing.T) {
	h := newRetireHub(t)
	rec, old := retiredFor(t, h, h.url())
	h.mu.Lock()
	h.tomb[string(old.Public())] = true
	h.mu.Unlock()
	rs := NewRetiredStore(t.TempDir())
	writeV1Retired(t, rs, rec)
	if remaining, err := newRetirer(t, rs, h.url()).RunOnce(context.Background()); err != nil || remaining != 0 {
		t.Fatalf("remaining %d err %v: a signed revoke completes a seeded v1 entry", remaining, err)
	}
}

func TestARootTransitionDuringRetirementResetsTheScope(t *testing.T) {
	keys, chain := hubChain(t, 1)
	hub := newChainHub(t, keys[1], chain)
	old, err := identity.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	rs := NewRetiredStore(t.TempDir())
	if err := rs.Add(RetiredIdentity{Key: old, HubURL: hub.srv.URL, Pin: identity.Pin{Pub: keys[0].Public()},
		MaxSerial: 500, CertSHA256: bytes.Repeat([]byte{7}, 32), RatchetSeeded: true}); err != nil {
		t.Fatal(err)
	}
	// The chain hub welcomes but serves no retirement call: the entry stays,
	// with the pin and the ratchet of the new root.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, _ = newRetirer(t, rs, hub.srv.URL).RunOnce(ctx)
	got := loadRetired(t, rs)
	if !bytes.Equal(got.Pin.Pub, keys[1].Public()) || got.Pin.Seq != 1 || got.MaxSerial != 1 {
		t.Fatalf("after the transition: pin %s seq %d max_serial %d", key2fp(got.Pin.Pub), got.Pin.Seq, got.MaxSerial)
	}
}
