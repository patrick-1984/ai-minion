package store

import (
	"os"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/worker/agentdetect"
)

// A96: the harness session name is kept per pane generation and native
// session, read back only for the session that last reported in the pane while
// its process lives, and dropped with the monitoring grant.
func TestHarnessTitleBindingAndRevocation(t *testing.T) {
	ctx := t.Context()
	s := openStore(t, t.TempDir())
	en := Enrollment{HubPub: pubKey(1), WorkerID: "work", Owner: "owner"}
	if err := s.SaveEnrollment(ctx, en); err != nil {
		t.Fatal(err)
	}
	if err := s.AcceptUsageRegistration(ctx, en.HubPub, en.WorkerID, en.Owner); err != nil {
		t.Fatal(err)
	}
	grant, err := s.UsageGrantID(ctx, en.WorkerID)
	if err != nil {
		t.Fatal(err)
	}
	base := time.Now().Add(-time.Minute)
	e := HarnessEvent{Session: "work", Pane: "%7", Created: base.Unix(), Harness: "claude", ProcessPID: os.Getpid(), ProcessBirth: agentdetect.ProcessBirth(os.Getpid()),
		NativeID: ops.UsageID("test", "one"), At: base, Kind: "heartbeat"}
	if e.ProcessBirth == "" {
		t.Skip("process lifetime unavailable on this platform")
	}
	read := func() (string, int) {
		t.Helper()
		title, pid, err := s.HarnessTitle(ctx, e.Session, e.Pane, e.Harness, e.Created, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		return title, pid
	}
	if err := s.RecordHarnessEvent(ctx, grant, e); err != nil {
		t.Fatal(err)
	}
	if title, _ := read(); title != "" {
		t.Fatalf("name before any rename: %q", title)
	}
	if err := s.RecordHarnessTitle(ctx, grant, e, "bad\x1bname"); err == nil {
		t.Fatal("an unsanitized name was stored")
	}
	if err := s.RecordHarnessTitle(ctx, grant, e, "release prep"); err != nil {
		t.Fatal(err)
	}
	if title, pid := read(); title != "release prep" || pid != os.Getpid() {
		t.Fatalf("name = %q, %d", title, pid)
	}
	if got, err := s.StoredHarnessTitle(ctx, e.Session, e.Pane, e.Harness, e.Created, e.NativeID); err != nil || got != (HarnessTitleCheck{Title: "release prep"}) {
		t.Fatalf("stored = %+v, %v", got, err)
	}
	// An unconfirmed name clears the known one (the session no longer has
	// it) and records only its digest and the searched offset.
	digest := ops.UsageID("test", "unconfirmed")
	e.At = e.At.Add(time.Millisecond)
	if err := s.MarkHarnessTitleChecked(ctx, grant, e, "AI summary", 0); err == nil {
		t.Fatal("a plain name was stored as a digest")
	}
	if err := s.MarkHarnessTitleChecked(ctx, grant, e, digest, 42); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.StoredHarnessTitle(ctx, e.Session, e.Pane, e.Harness, e.Created, e.NativeID); got != (HarnessTitleCheck{Checked: digest, Scanned: 42}) {
		t.Fatalf("after check = %+v", got)
	}
	if title, _ := read(); title != "" {
		t.Fatalf("name after an unconfirmed one = %q", title)
	}
	e.At = e.At.Add(time.Millisecond)
	if err := s.RecordHarnessTitle(ctx, grant, e, "release prep"); err != nil {
		t.Fatal(err)
	}
	if title, _, _ := s.HarnessTitle(ctx, e.Session, "%8", e.Harness, e.Created, time.Now()); title != "" {
		t.Fatal("another pane got the name")
	}
	if title, _, _ := s.HarnessTitle(ctx, e.Session, e.Pane, e.Harness, e.Created+1, time.Now()); title != "" {
		t.Fatal("a recreated session got the name")
	}

	// A new native session in the same pane has no name until it is given one.
	other := e
	other.NativeID = ops.UsageID("test", "two")
	other.At = base.Add(time.Second)
	if err := s.RecordHarnessEvent(ctx, grant, other); err != nil {
		t.Fatal(err)
	}
	if title, _ := read(); title != "" {
		t.Fatalf("the next session inherited the name %q", title)
	}
	// An ended process does not keep its name.
	other.At = base.Add(2 * time.Second)
	other.ProcessBirth = "obsolete"
	if err := s.RecordHarnessEvent(ctx, grant, other); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordHarnessTitle(ctx, grant, other, "gone"); err != nil {
		t.Fatal(err)
	}
	if title, _ := read(); title != "" {
		t.Fatalf("ended process name %q", title)
	}

	// An empty name forgets it.
	e.At = e.At.Add(time.Millisecond)
	if err := s.RecordHarnessTitle(ctx, grant, e, ""); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.StoredHarnessTitle(ctx, e.Session, e.Pane, e.Harness, e.Created, e.NativeID); got.Title != "" {
		t.Fatalf("cleared name = %q", got.Title)
	}
	// A digest alone (no name yet) reads as no name.
	e.At = e.At.Add(time.Millisecond)
	if err := s.MarkHarnessTitleChecked(ctx, grant, e, digest, 0); err != nil {
		t.Fatal(err)
	}
	if title, _ := read(); title != "" {
		t.Fatalf("checked-only row read as %q", title)
	}

	if _, err := s.db.Exec("UPDATE hub_enrollment SET usage_policy=0 WHERE id=1"); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordHarnessTitle(ctx, grant, e, "after"); err == nil {
		t.Fatal("revoked grant wrote a name")
	}
	var count int
	if err := s.db.QueryRow("SELECT count(*) FROM harness_titles").Scan(&count); err != nil || count != 0 {
		t.Fatalf("revocation left %d names: %v", count, err)
	}
}

func titleGrant(t *testing.T, s *Store) string {
	t.Helper()
	ctx := t.Context()
	en := Enrollment{HubPub: pubKey(1), WorkerID: "work", Owner: "owner"}
	if err := s.SaveEnrollment(ctx, en); err != nil {
		t.Fatal(err)
	}
	if err := s.AcceptUsageRegistration(ctx, en.HubPub, en.WorkerID, en.Owner); err != nil {
		t.Fatal(err)
	}
	grant, err := s.UsageGrantID(ctx, en.WorkerID)
	if err != nil {
		t.Fatal(err)
	}
	return grant
}

// A96 review: callbacks that overlap finish in any order; the write of the
// newer observation wins, a clear included, whatever finishes last.
func TestHarnessTitleWritesFollowObservationOrder(t *testing.T) {
	ctx := t.Context()
	s := openStore(t, t.TempDir())
	grant := titleGrant(t, s)
	base := time.Now().Add(-time.Minute)
	e := HarnessEvent{Session: "work", Pane: "%7", Created: base.Unix(), Harness: "claude", ProcessPID: os.Getpid(), ProcessBirth: agentdetect.ProcessBirth(os.Getpid()),
		NativeID: ops.UsageID("test", "one"), At: base, Kind: "heartbeat"}
	if err := s.RecordHarnessEvent(ctx, grant, e); err != nil {
		t.Fatal(err)
	}
	stored := func() HarnessTitleCheck {
		t.Helper()
		c, err := s.StoredHarnessTitle(ctx, e.Session, e.Pane, e.Harness, e.Created, e.NativeID)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	older, newer := e, e
	older.At, newer.At = base.Add(time.Second), base.Add(2*time.Second)
	if err := s.RecordHarnessTitle(ctx, grant, newer, "new name"); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordHarnessTitle(ctx, grant, older, "old name"); err != nil {
		t.Fatal(err)
	}
	if got := stored(); got.Title != "new name" {
		t.Fatalf("an older write replaced a newer one: %+v", got)
	}
	if err := s.MarkHarnessTitleChecked(ctx, grant, older, ops.UsageID("test", "d"), 1); err != nil {
		t.Fatal(err)
	}
	if got := stored(); got.Title != "new name" {
		t.Fatalf("an older check cleared a newer name: %+v", got)
	}
	// A newer clear is not undone by an older name.
	cleared := e
	cleared.At = base.Add(3 * time.Second)
	if err := s.RecordHarnessTitle(ctx, grant, cleared, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordHarnessTitle(ctx, grant, newer, "new name"); err != nil {
		t.Fatal(err)
	}
	if got := stored(); got.Title != "" {
		t.Fatalf("an older name undid a newer clear: %+v", got)
	}
}

// A96 review: after /clear the conversation before it is fenced off by the
// session start of the new one, as for the native state: a delayed hook of the
// old conversation, recorded last, does not bring back its name.
func TestHarnessTitleFencedBySessionStart(t *testing.T) {
	ctx := t.Context()
	s := openStore(t, t.TempDir())
	grant := titleGrant(t, s)
	base := time.Now().Add(-time.Minute)
	old := HarnessEvent{Session: "work", Pane: "%7", Created: base.Unix(), Harness: "claude", ProcessPID: os.Getpid(), ProcessBirth: agentdetect.ProcessBirth(os.Getpid()),
		NativeID: ops.UsageID("test", "old"), At: base, Kind: "heartbeat"}
	if old.ProcessBirth == "" {
		t.Skip("process lifetime unavailable on this platform")
	}
	if err := s.RecordHarnessEvent(ctx, grant, old); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordHarnessTitle(ctx, grant, old, "old name"); err != nil {
		t.Fatal(err)
	}
	cleared := old
	cleared.NativeID, cleared.Kind, cleared.At = ops.UsageID("test", "new"), "session_start", base.Add(time.Second)
	if err := s.RecordHarnessEvent(ctx, grant, cleared); err != nil {
		t.Fatal(err)
	}
	late := old
	late.At = base.Add(2 * time.Second)
	if err := s.RecordHarnessEvent(ctx, grant, late); err != nil {
		t.Fatal(err)
	}
	title, _, err := s.HarnessTitle(ctx, old.Session, old.Pane, old.Harness, old.Created, time.Now())
	if err != nil || title != "" {
		t.Fatalf("a delayed hook of the cleared conversation brought back %q (%v)", title, err)
	}
	named := cleared
	named.At = base.Add(3 * time.Second)
	if err := s.RecordHarnessTitle(ctx, grant, named, "new name"); err != nil {
		t.Fatal(err)
	}
	if title, _, _ := s.HarnessTitle(ctx, old.Session, old.Pane, old.Harness, old.Created, time.Now()); title != "new name" {
		t.Fatalf("name of the current conversation = %q", title)
	}
}
