//go:build e2e

package e2e

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/hub/hubid"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/proto"
	wagent "github.com/patrick-1984/ai-minion/internal/worker/agent"
)

// Story 9.4 composition cases for the protocol window and the restored
// backup, on the real Hub endpoint and the production connector with the
// Worker state on disk.
//
// Fixture choice for "protocol N-1 minor accepted": this build speaks 2.0 (the
// wire protocol was bumped to 2.0 with the mandatory request origin; there is
// no 1.x fallback), and minor 0 has no previous minor, so a Worker one minor
// behind cannot exist in this build. The same window is exercised from the
// other side, with the real Hub configured as protocol 2.1
// (ServiceOptions.Current, the Hub's own knob) and a Worker whose range is
// exactly 2.0: the Hub accepts the previous minor of its major and negotiates
// it (proto.Supported). The major mismatch uses a Worker whose range is exactly
// 3.0, and a Worker that still speaks 1.x is refused as well.

// enrolledCompWorker creates the Worker key, stores the pin of the Hub and has
// the Hub enroll the key, as pairing or an invitation would.
func enrolledCompWorker(t *testing.T, h *connHub) (*compWorkerState, store.WorkerRow) {
	t.Helper()
	w := newCompWorkerState(t)
	key, err := w.enr.LoadOrCreateKey()
	if err != nil {
		t.Fatal(err)
	}
	if err := w.enr.Commit(context.Background(), wagent.Enrollment{Pin: h.pin()}); err != nil {
		t.Fatal(err)
	}
	return w, h.enrol(key.Public(), "pi")
}

func TestHubAcceptsTheWorkersPreviousMinorAndNegotiatesIt(t *testing.T) {
	h := newConnHubWith(t, proto.Version{Major: 3, Minor: 1}, nil)
	w, row := enrolledCompWorker(t, h)
	only20 := proto.Range{Min: proto.Version{Major: 3, Minor: 0}, Max: proto.Version{Major: 3, Minor: 0}}
	conn, _, _ := w.connect(h, only20)
	eventually(t, "the 3.0 Worker to be online at the 3.1 Hub", func() bool { return h.service().Registry().IsOnline(row.ID) })
	on, _ := h.service().Registry().Get(row.ID)
	if on.Version != (proto.Version{Major: 3, Minor: 0}) {
		t.Fatalf("negotiated %s, want 3.0 (the previous minor)", on.Version)
	}
	eventually(t, "the connector to be connected", func() bool { return conn.Status().State == wagent.ConnConnected })
}

func TestHubAtTheBuildsVersionNegotiatesTheNewestSharedVersionWithANewerMinorWorker(t *testing.T) {
	h := newConnHub(t)
	w, row := enrolledCompWorker(t, h)
	w.connect(h, proto.Range{Min: proto.Version{Major: 3, Minor: 0}, Max: proto.Version{Major: 3, Minor: 1}})
	eventually(t, "the 3.0..3.1 Worker to be online", func() bool { return h.service().Registry().IsOnline(row.ID) })
	if on, _ := h.service().Registry().Get(row.ID); on.Version != proto.Current {
		t.Fatalf("negotiated %s, want %s", on.Version, proto.Current)
	}
}

// v0.5 was never released: a Worker that speaks only 1.x is refused, with no
// fallback, and keeps its enrollment.
func TestAOneDotXWorkerIsRefusedByTheTwoDotZeroHub(t *testing.T) {
	h := newConnHub(t)
	w, row := enrolledCompWorker(t, h)
	only1 := proto.Range{Min: proto.Version{Major: 1}, Max: proto.Version{Major: 1, Minor: 9}}
	conn, _, _ := w.connect(h, only1)
	eventually(t, "the 1.x mismatch to be reported", func() bool {
		return strings.Contains(conn.Status().LastError, string(proto.CodeProtoIncompatible))
	})
	if h.service().Registry().IsOnline(row.ID) {
		t.Fatal("a 1.x Worker connected to a 2.0 Hub")
	}
	if _, ok, _ := w.enr.Load(context.Background()); !ok {
		t.Fatal("a protocol mismatch wiped the enrollment")
	}
}

func TestAMajorMismatchIsRetriedKeepsTheEnrollmentAndConnectsWhenTheHubCatchesUp(t *testing.T) {
	h := newConnHub(t)
	w, row := enrolledCompWorker(t, h)
	only4 := proto.Range{Min: proto.Version{Major: 4}, Max: proto.Version{Major: 4}}
	conn, logs, _ := w.connect(h, only4)

	eventually(t, "the mismatch to be reported", func() bool {
		return strings.Contains(conn.Status().LastError, string(proto.CodeProtoIncompatible))
	})
	eventually(t, "the connector to retry the mismatch", func() bool { return conn.Status().Attempts >= 3 })
	if h.service().Registry().IsOnline(row.ID) {
		t.Fatal("a major mismatch connected")
	}
	if _, ok, _ := w.enr.Load(context.Background()); !ok {
		t.Fatal("a protocol mismatch wiped the enrollment")
	}
	if strings.Contains(logs.String(), "removed by the hub") {
		t.Fatalf("the mismatch was treated as a removal: %s", logs.String())
	}

	// The Hub is upgraded to the Worker's major: the same running connector
	// connects on a retry, with no human action.
	h.stop()
	h.mu.Lock()
	addr := h.addr
	h.mu.Unlock()
	h.current = proto.Version{Major: 4}
	h.start(addr)
	eventually(t, "the retrying Worker to connect at protocol 4.0", func() bool { return h.service().Registry().IsOnline(row.ID) })
	if on, _ := h.service().Registry().Get(row.ID); on.Version != (proto.Version{Major: 4}) {
		t.Fatalf("negotiated %s", on.Version)
	}
}

// A restored older Hub backup knows the Hub's identity but not the Worker that
// enrolled after the backup: the Worker is told not_enrolled, keeps its state
// and key, and connects again once the Hub knows it (a newer backup or a new
// enrollment). Nothing on the Worker is wiped.
func TestARestoredOlderHubBackupAnswersNotEnrolledAndTheWorkerKeepsItsState(t *testing.T) {
	h := newConnHub(t)
	ctx := context.Background()
	// The archive format does not know the two-tier identity files yet (design
	// section 13): the "older backup" is a consistent snapshot of the database
	// (VACUUM INTO) and the sealed root and operational key files.
	snapshot := t.TempDir()
	if _, err := h.st.DB().ExecContext(ctx, "VACUUM INTO ?", filepath.Join(snapshot, store.DBFile)); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{hubid.RootFile, hubid.OpFile, "data.key"} {
		raw, err := os.ReadFile(filepath.Join(h.dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(snapshot, name), raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	w, row := enrolledCompWorker(t, h) // enrolls after the backup
	conn, logs, _ := w.connect(h, proto.Range{})
	eventually(t, "the Worker to be online at the original Hub", func() bool { return h.service().Registry().IsOnline(row.ID) })
	keyBefore, _ := w.enr.LoadKey()

	// The Hub is restored from the older archive into a fresh directory and
	// started on the same address.
	h.stop()
	h.mu.Lock()
	addr := h.addr
	h.mu.Unlock()
	if err := h.st.Close(); err != nil {
		t.Fatal(err)
	}
	restored := snapshot
	st, err := store.Open(restored)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	h.dir, h.st = restored, st
	h.start(addr)

	eventually(t, "not_enrolled from the restored Hub", func() bool {
		return strings.Contains(conn.Status().LastError, string(proto.CodeNotEnrolled))
	})
	attempts := conn.Status().Attempts
	eventually(t, "more retries, none destructive", func() bool { return conn.Status().Attempts >= attempts+3 })
	got, ok, err := w.enr.Load(ctx)
	if err != nil || !ok || got.WorkerID != row.ID || got.Owner != "owner-1" {
		t.Fatalf("enrollment after not_enrolled = %+v, %v, %v", got, ok, err)
	}
	if k, err := w.enr.LoadKey(); err != nil || string(k.Public()) != string(keyBefore.Public()) {
		t.Fatalf("the Worker key changed: %v", err)
	}
	if strings.Contains(logs.String(), "removed by the hub") {
		t.Fatalf("not_enrolled was treated as a removal: %s", logs.String())
	}
	if h.service().Registry().IsOnline(row.ID) {
		t.Fatal("online at a Hub that does not know it")
	}

	// The Hub learns the Worker again (the newer state is restored by hand).
	h.enrol(keyBefore.Public(), "pi")
	eventually(t, "the Worker to reconnect once the Hub knows it", func() bool {
		return conn.Status().State == wagent.ConnConnected
	})
}
