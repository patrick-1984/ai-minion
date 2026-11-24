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
	if _, err := s.ObserveHarnessTitle(ctx, grant, e, "bad\x1bname"); err == nil {
		t.Fatal("an unsanitized name was stored")
	}
	nameTitle(t, s, grant, e, "release prep")
	if title, pid := read(); title != "release prep" || pid != os.Getpid() {
		t.Fatalf("name = %q, %d", title, pid)
	}
	// An unconfirmed name clears the known one (the session no longer has
	// it) and records only its digest and the searched offset.
	digest := ops.UsageID("test", "unconfirmed")
	e.At = e.At.Add(time.Millisecond)
	if obs, err := s.ObserveHarnessTitle(ctx, grant, e, "AI summary"); err != nil || !obs.Verify {
		t.Fatalf("observe = %+v, %v", obs, err)
	}
	if err := s.MarkHarnessTitleChecked(ctx, grant, e, "AI summary", 0); err == nil {
		t.Fatal("a plain name was stored as a digest")
	}
	if err := s.MarkHarnessTitleChecked(ctx, grant, e, digest, 42); err != nil {
		t.Fatal(err)
	}
	if title, _ := read(); title != "" {
		t.Fatalf("name after an unconfirmed one = %q", title)
	}
	e.At = e.At.Add(time.Millisecond)
	if obs, err := s.ObserveHarnessTitle(ctx, grant, e, "AI summary"); err != nil || obs != (HarnessTitleObservation{Verify: true, Checked: digest, Scanned: 42}) {
		t.Fatalf("search state = %+v, %v", obs, err)
	}
	// The confirmed name again: shown at once, no search.
	e.At = e.At.Add(time.Millisecond)
	if obs, err := s.ObserveHarnessTitle(ctx, grant, e, "release prep"); err != nil || obs.Verify {
		t.Fatalf("repeat = %+v, %v", obs, err)
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
	nameTitle(t, s, grant, other, "gone")
	if title, _ := read(); title != "" {
		t.Fatalf("ended process name %q", title)
	}

	// An empty name forgets it.
	e.At = e.At.Add(time.Millisecond)
	if _, err := s.ObserveHarnessTitle(ctx, grant, e, ""); err != nil {
		t.Fatal(err)
	}
	if title := storedTitle(t, s, e); title != "" {
		t.Fatalf("cleared name = %q", title)
	}

	if _, err := s.db.Exec("UPDATE hub_enrollment SET usage_policy=0 WHERE id=1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ObserveHarnessTitle(ctx, grant, e, "after"); err == nil {
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

// nameTitle observes title at e.At and confirms it, as the exporter does when
// the transcript holds the user's /rename.
func nameTitle(t *testing.T, s *Store, grant string, e HarnessEvent, title string) {
	t.Helper()
	if _, err := s.ObserveHarnessTitle(t.Context(), grant, e, title); err != nil {
		t.Fatal(err)
	}
	if err := s.ConfirmHarnessTitle(t.Context(), grant, e, title); err != nil {
		t.Fatal(err)
	}
}

// storedTitle is the name the row of e's native session shows.
func storedTitle(t *testing.T, s *Store, e HarnessEvent) string {
	t.Helper()
	var title string
	err := s.db.QueryRow(`SELECT title FROM harness_titles WHERE session=? AND pane=? AND created=? AND harness=? AND native=?`,
		e.Session, e.Pane, e.Created, e.Harness, e.NativeID).Scan(&title)
	if err != nil {
		t.Fatal(err)
	}
	return title
}

func titleEventAt(base time.Time, native string, d time.Duration) HarnessEvent {
	return HarnessEvent{Session: "work", Pane: "%7", Created: base.Unix(), Harness: "claude", ProcessPID: os.Getpid(), ProcessBirth: agentdetect.ProcessBirth(os.Getpid()),
		NativeID: ops.UsageID("test", native), At: base.Add(d), Kind: "heartbeat"}
}

// A96 review round 2: every observation advances the fence, clears and
// repeats included; a confirmation is shown only while its observation is the
// latest; a stale observation changes nothing; a different name clears the
// shown one until it is confirmed; the search state of a newer observation is
// only ever advanced.
func TestHarnessTitleObservationFence(t *testing.T) {
	ctx := t.Context()
	s := openStore(t, t.TempDir())
	grant := titleGrant(t, s)
	base := time.Now().Add(-time.Minute)
	e := titleEventAt(base, "one", 0)
	if err := s.RecordHarnessEvent(ctx, grant, e); err != nil {
		t.Fatal(err)
	}
	at := func(d time.Duration) HarnessEvent { return titleEventAt(base, "one", d) }

	// An older callback observed a name and is still searching when a newer
	// one without a name (or with the same, still unconfirmed, name) is
	// recorded: the older confirmation does not show the name.
	if obs, err := s.ObserveHarnessTitle(ctx, grant, at(1), "release prep"); err != nil || !obs.Verify {
		t.Fatalf("observe = %+v, %v", obs, err)
	}
	if _, err := s.ObserveHarnessTitle(ctx, grant, at(2), ""); err != nil {
		t.Fatal(err)
	}
	if err := s.ConfirmHarnessTitle(ctx, grant, at(1), "release prep"); err != nil {
		t.Fatal(err)
	}
	if got := storedTitle(t, s, e); got != "" {
		t.Fatalf("a stale confirmation showed %q", got)
	}
	// The confirmation is remembered: the name observed again is shown at
	// once.
	if obs, err := s.ObserveHarnessTitle(ctx, grant, at(3), "release prep"); err != nil || obs.Verify {
		t.Fatalf("repeat = %+v, %v", obs, err)
	}
	if got := storedTitle(t, s, e); got != "release prep" {
		t.Fatalf("repeat shows %q", got)
	}
	// A stale observation changes nothing.
	if obs, err := s.ObserveHarnessTitle(ctx, grant, at(2), "other"); err != nil || obs.Verify {
		t.Fatalf("stale observation = %+v, %v", obs, err)
	}
	if got := storedTitle(t, s, e); got != "release prep" {
		t.Fatalf("a stale observation changed the name to %q", got)
	}
	// A different name clears the shown one until it is confirmed (an
	// unreadable transcript never confirms it).
	if obs, err := s.ObserveHarnessTitle(ctx, grant, at(4), "new name"); err != nil || !obs.Verify {
		t.Fatalf("new name = %+v, %v", obs, err)
	}
	if got := storedTitle(t, s, e); got != "" {
		t.Fatalf("an unconfirmed new name left %q shown", got)
	}
	if err := s.ConfirmHarnessTitle(ctx, grant, at(4), "new name"); err != nil {
		t.Fatal(err)
	}
	if got := storedTitle(t, s, e); got != "new name" {
		t.Fatalf("a current confirmation shows %q", got)
	}
	// The search state: an older search does not replace a newer one's, but
	// the same search may advance it.
	d1, d2 := ops.UsageID("test", "d1"), ops.UsageID("test", "d2")
	if _, err := s.ObserveHarnessTitle(ctx, grant, at(5), "x"); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkHarnessTitleChecked(ctx, grant, at(5), d1, 10); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ObserveHarnessTitle(ctx, grant, at(6), "x"); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkHarnessTitleChecked(ctx, grant, at(5), d2, 99); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkHarnessTitleChecked(ctx, grant, at(5), d1, 20); err != nil {
		t.Fatal(err)
	}
	if obs, _ := s.ObserveHarnessTitle(ctx, grant, at(7), "x"); obs.Checked != d1 || obs.Scanned != 20 {
		t.Fatalf("search state = %+v", obs)
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
	old := titleEventAt(base, "old", 0)
	if old.ProcessBirth == "" {
		t.Skip("process lifetime unavailable on this platform")
	}
	if err := s.RecordHarnessEvent(ctx, grant, old); err != nil {
		t.Fatal(err)
	}
	nameTitle(t, s, grant, old, "old name")
	cleared := titleEventAt(base, "new", time.Second)
	cleared.Kind = "session_start"
	if err := s.RecordHarnessEvent(ctx, grant, cleared); err != nil {
		t.Fatal(err)
	}
	late := titleEventAt(base, "old", 2*time.Second)
	if err := s.RecordHarnessEvent(ctx, grant, late); err != nil {
		t.Fatal(err)
	}
	title, _, err := s.HarnessTitle(ctx, old.Session, old.Pane, old.Harness, old.Created, time.Now())
	if err != nil || title != "" {
		t.Fatalf("a delayed hook of the cleared conversation brought back %q (%v)", title, err)
	}
	nameTitle(t, s, grant, titleEventAt(base, "new", 3*time.Second), "new name")
	if title, _, _ := s.HarnessTitle(ctx, old.Session, old.Pane, old.Harness, old.Created, time.Now()); title != "new name" {
		t.Fatalf("name of the current conversation = %q", title)
	}
}

// A96 review round 2: /clear ended the old conversation (session_end) and
// the start of the new one was missed: the ended conversation never shows its
// name, neither while the new one reports nor after a delayed callback of the
// ended one. A resumed conversation (a start after its end) shows it again.
func TestHarnessTitleOfAnEndedConversation(t *testing.T) {
	ctx := t.Context()
	s := openStore(t, t.TempDir())
	grant := titleGrant(t, s)
	base := time.Now().Add(-time.Minute)
	start := titleEventAt(base, "old", 0)
	start.Kind = "session_start"
	if start.ProcessBirth == "" {
		t.Skip("process lifetime unavailable on this platform")
	}
	if err := s.RecordHarnessEvent(ctx, grant, start); err != nil {
		t.Fatal(err)
	}
	nameTitle(t, s, grant, titleEventAt(base, "old", time.Millisecond), "old name")
	read := func() string {
		t.Helper()
		title, _, err := s.HarnessTitle(ctx, start.Session, start.Pane, start.Harness, start.Created, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		return title
	}
	if got := read(); got != "old name" {
		t.Fatalf("before /clear = %q", got)
	}
	end := titleEventAt(base, "old", time.Second)
	end.Kind = "session_end"
	if err := s.RecordHarnessEvent(ctx, grant, end); err != nil {
		t.Fatal(err)
	}
	// The new conversation's start is missed; its status line reports.
	if err := s.RecordHarnessEvent(ctx, grant, titleEventAt(base, "new", 2*time.Second)); err != nil {
		t.Fatal(err)
	}
	if got := read(); got != "" {
		t.Fatalf("the ended conversation's name is shown: %q", got)
	}
	// A delayed callback of the ended conversation, recorded last.
	late := titleEventAt(base, "old", 3*time.Second)
	if err := s.RecordHarnessEvent(ctx, grant, late); err != nil {
		t.Fatal(err)
	}
	nameTitle(t, s, grant, late, "old name")
	if got := read(); got != "" {
		t.Fatalf("a delayed callback of the ended conversation brought back %q", got)
	}
	// The new conversation is named.
	nameTitle(t, s, grant, titleEventAt(base, "new", 4*time.Second), "new name")
	if err := s.RecordHarnessEvent(ctx, grant, titleEventAt(base, "new", 4*time.Second)); err != nil {
		t.Fatal(err)
	}
	if got := read(); got != "new name" {
		t.Fatalf("the new conversation shows %q", got)
	}
	// The old one resumed in the same process: its name is back.
	resumed := titleEventAt(base, "old", 5*time.Second)
	resumed.Kind = "session_start"
	if err := s.RecordHarnessEvent(ctx, grant, resumed); err != nil {
		t.Fatal(err)
	}
	if got := read(); got != "old name" {
		t.Fatalf("the resumed conversation shows %q", got)
	}
}

// A96 review round 4: two accounts can share one sanitized display name. A
// reassignment between them keeps the display name but changes the owner id:
// the stored names are dropped and the old monitoring grant no longer writes.
func TestHarnessTitlePurgedOnOwnerIDChangeWithSameDisplayName(t *testing.T) {
	ctx := t.Context()
	s := openStore(t, t.TempDir())
	en := Enrollment{HubPub: pubKey(1), WorkerID: "work", Owner: "Pat", OwnerID: "01900000-0000-7000-8000-000000000001"}
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
	e := titleEventAt(time.Now().Add(-time.Minute), "one", 0)
	if e.ProcessBirth == "" {
		t.Skip("process lifetime unavailable on this platform")
	}
	if err := s.RecordHarnessEvent(ctx, grant, e); err != nil {
		t.Fatal(err)
	}
	nameTitle(t, s, grant, e, "release prep")
	count := func() int {
		t.Helper()
		var n int
		if err := s.db.QueryRow("SELECT count(*) FROM harness_titles").Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	// The same owner welcomed again keeps the name.
	same := WelcomeInfo{WorkerID: en.WorkerID, Name: "box", Owner: en.Owner, OwnerID: en.OwnerID}
	if err := s.UpdateWelcome(ctx, same); err != nil {
		t.Fatal(err)
	}
	if n := count(); n != 1 {
		t.Fatalf("a repeated welcome of the same owner left %d names", n)
	}
	// Another account with the same display name.
	other := same
	other.OwnerID = "01900000-0000-7000-8000-000000000002"
	if err := s.UpdateWelcome(ctx, other); err != nil {
		t.Fatal(err)
	}
	if n := count(); n != 0 {
		t.Fatalf("reassignment to another account with the same display name left %d names", n)
	}
	e.At = e.At.Add(time.Millisecond)
	if _, err := s.ObserveHarnessTitle(ctx, grant, e, "release prep"); err == nil {
		t.Fatal("the previous owner's monitoring grant still writes names")
	}
	if now, err := s.UsageGrantID(ctx, en.WorkerID); err != nil || now == grant {
		t.Fatalf("grant not bound to the owner id: %q, %v", now, err)
	}
}
