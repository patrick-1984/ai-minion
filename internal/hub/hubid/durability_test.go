package hubid

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestUnconfirmedCommitKeepsThePreviousPairUntilConfirmed(t *testing.T) {
	e := newEnv(t)
	s := e.established()
	old := e.cert()
	oldOp := e.opPub(OpFile)
	gen, closes := s.Generation(), e.closes
	fail := true
	e.confirmFailsAfterCommit(&fail)
	_, err := s.Reissue(bg, ui, pw1, IssueOptions{})
	wantCode(t, err, ErrNotDurable)
	// The Hub keeps its previous pair; N stays; nothing was swapped or closed.
	snap, _ := s.Current()
	if snap.Cert.Serial != old.Serial || s.Generation() != gen || e.closes != closes || !e.has(OpNextFile) || string(e.opPub(OpFile)) != string(oldOp) {
		t.Fatalf("used before confirmed: serial %d/%d gen %d/%d closes %d/%d N %v", snap.Cert.Serial, old.Serial, s.Generation(), gen, e.closes, closes, e.has(OpNextFile))
	}
	e.opKeyPreserved()
	// Every other change is refused with conflict.
	_, err = s.ChangePassphrase(bg, ui, pw1, pw2, pw2)
	wantCode(t, err, ErrConflict)
	_, err = s.RotateMaster(bg, ui, pw1, pw2, pw2, IssueOptions{})
	wantCode(t, err, ErrConflict)
	_, err = s.Reissue(bg, ui, pw1, IssueOptions{})
	wantCode(t, err, ErrNotDurable)
	// The same command confirms and completes the same issuance: no new serial.
	fail = false
	res, err := s.Reissue(bg, ui, pw1, IssueOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Serial != e.cert().Serial || res.Serial <= old.Serial || s.Generation() != gen+1 || e.closes != closes+1 || e.has(OpNextFile) {
		t.Fatalf("completion: %+v", res)
	}
	if string(e.opPub(OpFile)) != string(e.cert().OpPub) || len(e.audits("hub_identity.reissue")) != 2 {
		t.Fatal("files or audit after the completion")
	}
	e.allSignersDestroyed()
}

func TestStartWithAFailingConfirmationRefusesAndKeepsFiles(t *testing.T) {
	e := newEnv(t)
	s := e.established()
	e.crashAt(PointCommitted)
	_, err := s.Reissue(bg, ui, pw1, IssueOptions{})
	wantCode(t, err, ErrCrash)
	e.makeDurable = func(context.Context) error { return errors.New("checkpoint failed") }
	wantCode(t, e.startErr(), ErrNotDurable)
	if !e.has(OpNextFile) || !e.has(OpFile) {
		t.Fatal("files were touched")
	}
	e.makeDurable = nil
	if _, err := e.svc().Current(); err != nil {
		t.Fatal(err)
	}
}

func TestUnconfirmedPassphraseChangeAndRotation(t *testing.T) {
	e := newEnv(t)
	s := e.established()
	fail := true
	rearm := e.confirmFailsAfterCommit(&fail)
	_, err := s.ChangePassphrase(bg, ui, pw1, pw2, pw2)
	wantCode(t, err, ErrNotDurable)
	if !e.has(RootNextFile) {
		t.Fatal("RN must stay")
	}
	e.sealedPreserved()
	fail = false
	if _, err := s.ChangePassphrase(bg, ui, pw1, pw2, pw2); err != nil {
		t.Fatal(err) // the repetition completes it; the passphrase is typed again but not used
	}
	if e.has(RootNextFile) {
		t.Fatal("RN not renamed")
	}
	if _, ok := e.rootOpens(pw2); !ok {
		t.Fatal("new passphrase")
	}
	// Rotation.
	fail = true
	rearm()
	_, err = s.RotateMaster(bg, ui, pw2, pw3, pw3, IssueOptions{})
	wantCode(t, err, ErrNotDurable)
	if !e.has(RootNextFile) || !e.has(OpNextFile) {
		t.Fatal("RN and N must stay")
	}
	snap, _ := s.Current()
	if string(snap.RootPub) == string(e.row().Pub) {
		t.Fatal("the new root is in use before it was confirmed")
	}
	fail = false
	if _, err := s.RotateMaster(bg, ui, pw2, pw3, pw3, IssueOptions{}); err != nil {
		t.Fatal(err)
	}
	snap, _ = s.Current()
	if string(snap.RootPub) != string(e.row().Pub) || e.has(RootNextFile) || e.has(OpNextFile) {
		t.Fatal("rotation not completed")
	}
}

func TestFailedRenameAfterADurableCommit(t *testing.T) {
	e := newEnv(t)
	s := e.established()
	broken := true
	e.rename = func(_, to string) error {
		if broken {
			return errors.New("EIO")
		}
		return nil
	}
	res, err := s.Reissue(bg, ui, pw1, IssueOptions{})
	wantCode(t, err, ErrFilesPending)
	// D5: it keeps serving the new in-memory pair.
	snap, _ := s.Current()
	if snap.Cert.Serial != res.Serial || snap.Cert.Serial != e.cert().Serial {
		t.Fatal("the new pair is not served")
	}
	e.opKeyPreserved()
	// Further identity changes are refused with conflict and no other N is written.
	nBefore := mustRead(t, e, OpNextFile)
	_, err = s.Reissue(bg, ui, pw1, IssueOptions{})
	wantCode(t, err, ErrConflict)
	_, err = s.ChangePassphrase(bg, ui, pw1, pw2, pw2)
	wantCode(t, err, ErrConflict)
	if string(mustRead(t, e, OpNextFile)) != string(nBefore) {
		t.Fatal("N was rewritten")
	}
	// A retry that succeeds lifts the block.
	broken = false
	if _, err := s.ChangePassphrase(bg, ui, pw1, pw2, pw2); err != nil {
		t.Fatal(err)
	}
	if e.has(OpNextFile) || string(e.opPub(OpFile)) != string(snap.Cert.OpPub) {
		t.Fatal("the pending rename was not completed first")
	}
}

func TestDirectorySyncFailures(t *testing.T) {
	// D6 before the commit: nothing changes, staged files are removed.
	e := newEnv(t)
	s := e.established()
	old, oldOp := e.cert(), e.opPub(OpFile)
	e.syncDir = func(string) error { return errors.New("EIO") }
	if _, err := s.Reissue(bg, ui, pw1, IssueOptions{}); err == nil {
		t.Fatal("no error")
	}
	e.syncDir = nil
	if e.has(OpNextFile) || e.cert().Serial != old.Serial || string(e.opPub(OpFile)) != string(oldOp) || s.Generation() != 2 {
		t.Fatal("a failed sync before the commit changed state")
	}
	// Rotation: RN and N are removed too.
	e.syncDir = func(string) error { return errors.New("EIO") }
	if _, err := s.RotateMaster(bg, ui, pw1, pw2, pw2, IssueOptions{}); err == nil {
		t.Fatal("no error")
	}
	e.syncDir = nil
	if e.has(RootNextFile) || e.has(OpNextFile) {
		t.Fatal("staged files left")
	}
	if _, ok := e.rootOpens(pw1); !ok {
		t.Fatal("old root damaged")
	}
	// D6 after a rename: keeps serving, refuses with conflict, retries on the next request.
	calls := 0
	e.syncDir = func(string) error {
		calls++
		if calls == 2 { // 1 = staging, 2 = after the rename
			return errors.New("EIO")
		}
		return nil
	}
	_, err := s.Reissue(bg, ui, pw1, IssueOptions{})
	wantCode(t, err, ErrFilesPending)
	if snap, _ := s.Current(); snap.Cert.Serial != e.cert().Serial {
		t.Fatal("not serving the new pair")
	}
	e.syncDir = func(string) error { return errors.New("EIO") }
	_, err = s.Reissue(bg, ui, pw1, IssueOptions{})
	wantCode(t, err, ErrConflict)
	e.syncDir = nil
	if _, err := s.Reissue(bg, ui, pw1, IssueOptions{}); err != nil {
		t.Fatal(err)
	}
}

func TestStalledHandshakeFailsAtRegistration(t *testing.T) {
	e := newEnv(t)
	s := e.established()
	snap, _ := s.Current() // a handshake takes its pair and generation once
	if !s.Admit(snap.Generation) {
		t.Fatal("not admitted")
	}
	if _, err := s.Reissue(bg, ui, pw1, IssueOptions{}); err != nil {
		t.Fatal(err)
	}
	if s.Admit(snap.Generation) {
		t.Fatal("a handshake that took the old pair registered")
	}
	// A passphrase change does not move the generation.
	g := s.Generation()
	if _, err := s.ChangePassphrase(bg, ui, pw1, pw2, pw2); err != nil {
		t.Fatal(err)
	}
	if !s.Admit(g) {
		t.Fatal("a passphrase change invalidated the generation")
	}
}

func TestExpiryWarningsAndEvents(t *testing.T) {
	e := newEnv(t)
	s := e.established() // 180 days
	na := e.cert().NotAfterMs
	at := func(d time.Duration) { e.clock = time.UnixMilli(na).Add(d) }
	level := func() WarnLevel {
		st, err := s.Status(bg)
		if err != nil {
			t.Fatal(err)
		}
		return st.Warning
	}
	at(-30*24*time.Hour - time.Minute)
	if level() != WarnNone {
		t.Fatal("warning before 30 days")
	}
	if _, err := s.Tick(bg); err != nil || len(e.audits("hub_identity.expiry_warning")) != 0 {
		t.Fatal("event before 30 days")
	}
	at(-30 * 24 * time.Hour)
	if level() != WarnWarning {
		t.Fatal("no warning at 30 days")
	}
	_, _ = s.Tick(bg)
	_, _ = s.Tick(bg)
	s = e.svc() // a restart does not repeat the event of the same UTC day
	_, _ = s.Tick(bg)
	rows := e.audits("hub_identity.expiry_warning")
	if len(rows) != 1 || rows[0].Actor != SystemActor {
		t.Fatalf("warning rows %d", len(rows))
	}
	e.clock = e.clock.Add(24 * time.Hour)
	_, _ = s.Tick(bg)
	if len(e.audits("hub_identity.expiry_warning")) != 2 {
		t.Fatal("no event for the next UTC day")
	}
	at(-7 * 24 * time.Hour)
	if level() != WarnError {
		t.Fatal("no error level at 7 days")
	}
	// Expiry: sessions closed once, event once, not served, restart needs no passphrase.
	closes := e.closes
	at(0)
	if level() != WarnExpired {
		t.Fatal("not expired at not_after")
	}
	_, err := s.Current()
	wantCode(t, err, ErrNotServing)
	_, _ = s.Tick(bg)
	_, _ = s.Tick(bg)
	if e.closes != closes+1 || len(e.audits("hub_identity.expired")) != 1 {
		t.Fatalf("closes %d expired rows %d", e.closes-closes, len(e.audits("hub_identity.expired")))
	}
	at(time.Hour)
	s2 := e.svc()
	if _, err := s2.Current(); err == nil {
		t.Fatal("expired Hub serves")
	}
	st, _ := s2.Status(bg)
	if !st.Expired || st.Serving {
		t.Fatalf("status %+v", st)
	}
	// Nothing renewed it by itself; re-issue fixes it.
	if _, err := s2.Reissue(bg, ui, pw1, IssueOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s2.Current(); err != nil {
		t.Fatal(err)
	}
}

func TestDeadServiceRefusesEverything(t *testing.T) {
	e := newEnv(t)
	s := e.established()
	e.crashAt(PointStaged)
	_, _ = s.Reissue(bg, ui, pw1, IssueOptions{})
	_, err := s.Reissue(bg, ui, pw1, IssueOptions{})
	wantCode(t, err, ErrFailClosed)
}

// One test with the production sealer: a real age/scrypt file at work factor 18.
func TestRealSealerRoundTrip(t *testing.T) {
	if testing.Short() {
		t.Skip("real scrypt derivations")
	}
	e := newEnv(t)
	e.fk = nil
	e.opts = func(o *Options) { o.Sealer = AgeSealer{} }
	s := e.svc()
	if _, err := s.SetPassphrase(bg, ui, pw1, pw1, IssueOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Reissue(bg, ui, pw2, IssueOptions{}); !errors.Is(err, ErrPassphraseInvalid) {
		t.Fatalf("wrong passphrase: %v", err)
	}
	if _, err := s.Reissue(bg, cli, pw1, IssueOptions{}); err != nil {
		t.Fatal(err)
	}
	e.allSignersDestroyed()
}
