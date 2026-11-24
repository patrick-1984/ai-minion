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
	if got, checked, err := s.StoredHarnessTitle(ctx, e.Session, e.Pane, e.Harness, e.Created, e.NativeID); err != nil || got != "release prep" || checked != "" {
		t.Fatalf("stored = %q, %q, %v", got, checked, err)
	}
	// An unconfirmed name keeps the known one and records only its digest.
	digest := ops.UsageID("test", "unconfirmed")
	if err := s.MarkHarnessTitleChecked(ctx, grant, e, "AI summary"); err == nil {
		t.Fatal("a plain name was stored as a digest")
	}
	if err := s.MarkHarnessTitleChecked(ctx, grant, e, digest); err != nil {
		t.Fatal(err)
	}
	if got, checked, _ := s.StoredHarnessTitle(ctx, e.Session, e.Pane, e.Harness, e.Created, e.NativeID); got != "release prep" || checked != digest {
		t.Fatalf("after check = %q, %q", got, checked)
	}
	if title, _ := read(); title != "release prep" {
		t.Fatalf("name after check = %q", title)
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
	if err := s.RecordHarnessTitle(ctx, grant, e, ""); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := s.StoredHarnessTitle(ctx, e.Session, e.Pane, e.Harness, e.Created, e.NativeID); got != "" {
		t.Fatalf("cleared name = %q", got)
	}
	// A digest alone (no name yet) reads as no name.
	if err := s.MarkHarnessTitleChecked(ctx, grant, e, digest); err != nil {
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
