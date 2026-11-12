package hubid

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/identity"
)

var bg = context.Background()

func TestPristineStartsInB0AndServesNothing(t *testing.T) {
	e := newEnv(t)
	s := e.svc()
	if s.State() != StatePristine {
		t.Fatalf("state = %v", s.State())
	}
	_, err := s.Current()
	wantCode(t, err, ErrNotServing)
	st, err := s.Status(bg)
	if err != nil || st.State != StatePristine || st.PassphraseSet || st.Serving {
		t.Fatalf("status = %+v, %v", st, err)
	}
	// Reissue, rotate and change need a sealed root.
	_, err = s.Reissue(bg, ui, pw1, IssueOptions{})
	wantCode(t, err, ErrConflict)
	_, err = s.ChangePassphrase(bg, ui, pw1, pw2, pw2)
	wantCode(t, err, ErrConflict)
	_, err = s.RotateMaster(bg, ui, pw1, pw2, pw2, IssueOptions{})
	wantCode(t, err, ErrConflict)
}

func TestSetPassphraseFromB0(t *testing.T) {
	e := newEnv(t)
	s := e.svc()
	res, err := s.SetPassphrase(bg, ui, pw1, pw1, IssueOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if s.State() != StateEstablished || res.Kind != "passphrase_set" || res.Generation != 2 {
		t.Fatalf("state %v result %+v", s.State(), res)
	}
	row := e.row()
	if !row.RootSealed || len(row.RootFileSHA256) != 32 || len(row.CurrentCert) != identity.HubCertSize || row.MigratedFrom06 {
		t.Fatalf("row = %+v", row)
	}
	c := e.cert()
	if c.Verify(row.Pub) != nil || c.NotAfterMs-c.NotBeforeMs != 180*dayMs || int64(c.Serial) != row.LastIssued {
		t.Fatalf("cert %+v row serial %d", c, row.LastIssued)
	}
	if got, ok := e.rootOpens(pw1); !ok || string(got) != string(row.Pub) {
		t.Fatal("the sealed root does not open with the passphrase")
	}
	if !e.has(RootFile) || !e.has(OpFile) || e.has(OpNextFile) || e.has(RootNextFile) || e.has(PlainFile) {
		t.Fatal("unexpected files")
	}
	if string(e.opPub(OpFile)) != string(c.OpPub) {
		t.Fatal("op.key is not the certified key")
	}
	if e.closes != 1 {
		t.Fatalf("sessions closed %d times", e.closes)
	}
	snap, err := s.Current()
	if err != nil || snap.Generation != 2 || snap.Cert.Serial != c.Serial || !s.Admit(2) || s.Admit(1) {
		t.Fatalf("snapshot %+v err %v", snap, err)
	}
	if n := len(e.audits("hub_identity.passphrase_set")); n != 1 {
		t.Fatalf("passphrase_set rows = %d", n)
	}
	if n := len(e.audits("hub_identity.reissue")); n != 1 {
		t.Fatalf("reissue rows = %d", n)
	}
	// B2 and B3: set-passphrase is a conflict and touches nothing.
	sha := e.row().RootFileSHA256
	before := e.row()
	_, err = s.SetPassphrase(bg, ui, pw2, pw2, IssueOptions{})
	wantCode(t, err, ErrConflict)
	after := e.row()
	if string(after.RootFileSHA256) != string(sha) || after.Web != before.Web || after.CLI != before.CLI {
		t.Fatal("a refused set-passphrase changed state")
	}
	e.allSignersDestroyed()
	// A restart needs no passphrase and serves the same pair.
	s2 := e.svc()
	snap2, err := s2.Current()
	if err != nil || snap2.Cert.Serial != c.Serial || snap2.Generation != 1 {
		t.Fatalf("restart snapshot %+v err %v", snap2, err)
	}
}

func TestSetPassphraseFromB1KeepsTheIdentity(t *testing.T) {
	e := newEnv(t)
	k := e.plain()
	s := e.svc()
	if s.State() != StatePlain {
		t.Fatalf("state = %v", s.State())
	}
	if _, err := s.Current(); !errors.Is(err, ErrNotServing) {
		t.Fatalf("a B1 Hub serves: %v", err)
	}
	st, _ := s.Status(bg)
	if st.State != StatePlain || st.PassphraseSet || !st.MigratedFrom06 || st.RootFingerprint != k.Fingerprint().String() {
		t.Fatalf("status %+v", st)
	}
	if _, err := s.SetPassphrase(bg, ui, pw1, pw1, IssueOptions{ValidityDays: 90}); err != nil {
		t.Fatal(err)
	}
	row := e.row()
	if string(row.Pub) != string(k.Public()) || row.Fingerprint != k.Fingerprint().String() || !row.MigratedFrom06 {
		t.Fatal("the identity changed")
	}
	if e.has(PlainFile) || !e.has(RootFile) || !e.has(OpFile) {
		t.Fatal("files after sealing")
	}
	if got, ok := e.rootOpens(pw1); !ok || string(got) != string(k.Public()) {
		t.Fatal("the sealed root is not the old identity")
	}
	if c := e.cert(); c.NotAfterMs-c.NotBeforeMs != 90*dayMs || c.Verify(k.Public()) != nil {
		t.Fatal("first certificate")
	}
	e.allSignersDestroyed()
}

func TestPassphraseRules(t *testing.T) {
	e := newEnv(t)
	s := e.svc()
	short := "0123456789abc" // 13
	_, err := s.SetPassphrase(bg, ui, short, short, IssueOptions{})
	wantCode(t, err, ErrInvalidArgument)
	_, err = s.SetPassphrase(bg, ui, pw1, pw2, IssueOptions{})
	wantCode(t, err, ErrInvalidArgument)
	_, err = s.SetPassphrase(bg, Actor{ID: "x", Channel: "web"}, pw1, pw1, IssueOptions{})
	wantCode(t, err, ErrInvalidArgument)
	e.opts = nil
	if _, err := s.SetPassphrase(bg, ui, "0123456789abcd", "0123456789abcd", IssueOptions{}); err != nil { // 14 accepted
		t.Fatal(err)
	}
	_, err = s.ChangePassphrase(bg, ui, "0123456789abcd", "0123456789abcd", "0123456789abcd")
	wantCode(t, err, ErrInvalidArgument)
	_, err = s.RotateMaster(bg, ui, "0123456789abcd", "0123456789abcd", "0123456789abcd", IssueOptions{})
	wantCode(t, err, ErrInvalidArgument)
	// The veto hook (for example: equal to the login password).
	e2 := newEnv(t)
	e2.opts = func(o *Options) { o.RejectPassphrase = func(Actor, string) bool { return true } }
	_, err = e2.svc().SetPassphrase(bg, ui, pw1, pw1, IssueOptions{})
	wantCode(t, err, ErrInvalidArgument)
	if msg := err.Error(); strings.Contains(msg, pw1) {
		t.Fatal("error leaks the passphrase")
	}
}

func TestReissueIsRotationOfTheOperationalKey(t *testing.T) {
	e := newEnv(t)
	s := e.established()
	c1, row1 := e.cert(), e.row()
	oldOp := e.opPub(OpFile)
	e.clock = e.clock.Add(time.Hour)
	res, err := s.Reissue(bg, cli, pw1, IssueOptions{ValidityDays: 30})
	if err != nil {
		t.Fatal(err)
	}
	c2, row2 := e.cert(), e.row()
	if string(c2.OpPub) == string(c1.OpPub) || string(e.opPub(OpFile)) != string(c2.OpPub) || string(oldOp) == string(e.opPub(OpFile)) {
		t.Fatal("re-issue did not create a new operational key")
	}
	if c2.Serial <= c1.Serial || int64(c2.Serial) != row2.LastIssued || string(row2.Pub) != string(row1.Pub) || string(row2.RootFileSHA256) != string(row1.RootFileSHA256) {
		t.Fatal("serial or root state")
	}
	if c2.NotAfterMs-c2.NotBeforeMs != 30*dayMs || c2.Verify(row1.Pub) != nil {
		t.Fatal("validity applies to this issuance")
	}
	if res.Generation != 3 || s.Generation() != 3 || s.Admit(2) || e.closes != 2 || e.has(OpNextFile) {
		t.Fatalf("generation %d closes %d", res.Generation, e.closes)
	}
	rows := e.audits("hub_identity.reissue")
	if len(rows) != 2 || rows[0].Actor != "local:1000" || !strings.Contains(rows[0].Target, "channel=cli") {
		t.Fatalf("audit %+v", rows)
	}
	// The default validity is not sticky.
	e.clock = e.clock.Add(time.Hour)
	if _, err := s.Reissue(bg, ui, pw1, IssueOptions{}); err != nil {
		t.Fatal(err)
	}
	if c := e.cert(); c.NotAfterMs-c.NotBeforeMs != 180*dayMs {
		t.Fatal("validity was remembered")
	}
	e.allSignersDestroyed()
}

func TestReissueInB2IssuesTheFirstCertificate(t *testing.T) {
	e := newEnv(t)
	s := e.svc()
	e.crashAt(PointBSDurable)
	_, err := s.SetPassphrase(bg, ui, pw1, pw1, IssueOptions{})
	wantCode(t, err, ErrCrash)
	s = e.svc()
	if s.State() != StateSealedNoCert {
		t.Fatalf("state = %v", s.State())
	}
	if _, err := s.Current(); err == nil {
		t.Fatal("B2 serves")
	}
	if _, err := s.SetPassphrase(bg, ui, pw2, pw2, IssueOptions{}); !errors.Is(err, ErrConflict) {
		t.Fatalf("set-passphrase in B2: %v", err)
	}
	if _, err := s.Reissue(bg, ui, pw1, IssueOptions{}); err != nil {
		t.Fatal(err)
	}
	if s.State() != StateEstablished {
		t.Fatal("not established")
	}
}

func TestSerialsAreMonotonicAndFloorsWork(t *testing.T) {
	e := newEnv(t)
	s := e.established()
	last := e.cert().Serial
	// Same millisecond: the serial still rises.
	for i := 0; i < 3; i++ {
		if _, err := s.Reissue(bg, ui, pw1, IssueOptions{}); err != nil {
			t.Fatal(err)
		}
		if c := e.cert(); c.Serial <= last {
			t.Fatalf("serial %d after %d", c.Serial, last)
		} else {
			last = c.Serial
		}
	}
	// Clock rollback with a durable database never repeats a serial.
	e.clock = e.clock.Add(-48 * time.Hour)
	if _, err := s.Reissue(bg, ui, pw1, IssueOptions{}); err != nil {
		t.Fatal(err)
	}
	if c := e.cert(); c.Serial != last+1 || e.row().LastIssued != int64(last+1) {
		t.Fatalf("serial after rollback %d, last %d", c.Serial, last)
	}
	last++
	// A floor.
	floor := int64(last) + 1000
	if _, err := s.Reissue(bg, ui, pw1, IssueOptions{SerialFloor: floor}); err != nil {
		t.Fatal(err)
	}
	if c := e.cert(); int64(c.Serial) != floor+1 {
		t.Fatalf("serial %d, want %d", c.Serial, floor+1)
	}
	// A floor beyond now + 366 d is refused without the allowance and counts nothing.
	far := e.clock.UnixMilli() + 400*dayMs
	before := e.row()
	_, err := s.Reissue(bg, ui, pw1, IssueOptions{SerialFloor: far})
	wantCode(t, err, ErrInvalidArgument)
	if e.row().Web != before.Web {
		t.Fatal("a refused floor counted")
	}
	if _, err := s.Reissue(bg, ui, pw1, IssueOptions{SerialFloor: far, AllowFarFloor: true}); err != nil {
		t.Fatal(err)
	}
	if c := e.cert(); int64(c.Serial) != far+1 {
		t.Fatalf("serial %d", c.Serial)
	}
	// The serial space end is an error, not a wrap.
	if _, err := allocateSerial(int64(identity.MaxHubCertSerial), 1, 0); err == nil {
		t.Fatal("serial space exhausted accepted")
	}
	if _, err := allocateSerial(1, 1, int64(identity.MaxHubCertSerial)); err == nil {
		t.Fatal("floor at the end accepted")
	}
	if n, _ := allocateSerial(5, 3, 2); n != 6 {
		t.Fatalf("allocate = %d", n)
	}
	if n, _ := allocateSerial(5, 100, 2); n != 100 {
		t.Fatalf("allocate = %d", n)
	}
}

func TestRotateMasterFollowsTheChain(t *testing.T) {
	e := newEnv(t)
	s := e.established()
	oldRoot := e.row().Pub
	oldCert := e.cert()
	if _, err := e.st.DB().Exec(`INSERT INTO invitation_keys (id, secret_hash, mode, kind, ttl, creator_id, created_at, max_uses)
		VALUES ('i1', zeroblob(32), 'one_time', 'private', '1h', 'u', 1, 1)`); err != nil {
		t.Fatal(err)
	}
	e.clock = e.clock.Add(time.Minute)
	res, err := s.RotateMaster(bg, ui, pw1, pw2, pw2, IssueOptions{})
	if err != nil {
		t.Fatal(err)
	}
	row := e.row()
	if string(row.Pub) == string(oldRoot) || row.MigratedFrom06 || res.Generation != 3 || s.Generation() != 3 {
		t.Fatalf("row %+v result %+v", row, res)
	}
	chain, err := e.st.Transitions(bg)
	if err != nil || len(chain) != 1 {
		t.Fatalf("chain %v %v", chain, err)
	}
	tr := identity.Transition{Seq: chain[0].Seq, OldPub: chain[0].OldPub, NewPub: chain[0].NewPub, CreatedAt: time.UnixMilli(chain[0].CreatedAt).UTC(), SigByOld: chain[0].SigByOld}
	if err := tr.Verify(); err != nil {
		t.Fatalf("transition: %v", err)
	}
	pin, err := identity.VerifyChain(identity.Pin{Pub: oldRoot}, []identity.Transition{tr})
	if err != nil || string(pin.Pub) != string(row.Pub) {
		t.Fatalf("chain %v", err)
	}
	c := e.cert()
	if c.Verify(row.Pub) != nil || c.Serial <= oldCert.Serial || c.Verify(oldRoot) == nil {
		t.Fatal("certificate is not under the new root")
	}
	if got, ok := e.rootOpens(pw2); !ok || string(got) != string(row.Pub) {
		t.Fatal("new passphrase does not open the new root")
	}
	if _, ok := e.rootOpens(pw1); ok {
		t.Fatal("old passphrase opens the current root")
	}
	var revoked int
	if err := e.st.DB().QueryRow(`SELECT revoked FROM invitation_keys WHERE id = 'i1'`).Scan(&revoked); err != nil || revoked != 1 {
		t.Fatalf("invitation revoked = %d %v", revoked, err)
	}
	if e.has(RootNextFile) || e.has(OpNextFile) || len(e.audits("hub_identity.rotate")) != 1 || e.closes != 2 {
		t.Fatal("files, audit or sessions")
	}
	e.allSignersDestroyed()
	// The new passphrase now serves operations; the old one is wrong.
	if _, err := s.Reissue(bg, ui, pw1, IssueOptions{}); !errors.Is(err, ErrPassphraseInvalid) {
		t.Fatalf("old passphrase: %v", err)
	}
	if _, err := s.Reissue(bg, ui, pw2, IssueOptions{}); err != nil {
		t.Fatal(err)
	}
}

func TestChangePassphraseKeepsEverythingElse(t *testing.T) {
	e := newEnv(t)
	s := e.established()
	cert, row := e.cert(), e.row()
	gen, closes := s.Generation(), e.closes
	res, err := s.ChangePassphrase(bg, cli, pw1, pw2, pw2)
	if err != nil {
		t.Fatal(err)
	}
	after := e.row()
	if s.Generation() != gen || e.closes != closes || res.Generation != gen || string(after.CurrentCert) != string(cert.Bytes()) || string(after.Pub) != string(row.Pub) {
		t.Fatal("a passphrase change touched the certificate, generation or sessions")
	}
	if string(after.RootFileSHA256) == string(row.RootFileSHA256) || e.has(RootNextFile) {
		t.Fatal("sealed root not replaced")
	}
	if _, ok := e.rootOpens(pw1); ok {
		t.Fatal("old passphrase still opens")
	}
	if got, ok := e.rootOpens(pw2); !ok || string(got) != string(row.Pub) {
		t.Fatal("new passphrase does not open")
	}
	if len(e.audits("hub_identity.passphrase_change")) != 1 {
		t.Fatal("audit")
	}
	e.allSignersDestroyed()
}

func TestWrongPassphraseIsOneGenericAnswer(t *testing.T) {
	e := newEnv(t)
	s := e.established()
	var msgs []string
	var wrong error
	_, wrong = s.Reissue(bg, ui, pw2, IssueOptions{})
	msgs = append(msgs, wrong.Error())
	wantCode(t, wrong, ErrPassphraseInvalid)
	if e.fk.dummy != 0 {
		t.Fatal("a wrong passphrase ran the dummy derivation")
	}
	// A swapped (hash mismatch) file: early failure, one dummy derivation, same answer.
	good := mustRead(t, e, RootFile)
	mustWrite(t, e, RootFile, append([]byte("FKS1"), make([]byte, 64)...))
	_, err := s.Reissue(bg, ui, pw1, IssueOptions{})
	wantCode(t, err, ErrPassphraseInvalid)
	msgs = append(msgs, err.Error())
	if e.fk.dummy != 1 {
		t.Fatalf("dummy derivations = %d", e.fk.dummy)
	}
	// A damaged file whose hash is recorded: Open fails before any derivation.
	bad := []byte("not a sealed file")
	mustWrite(t, e, RootFile, bad)
	if _, err := e.st.DB().Exec(`UPDATE hub_identity SET root_file_sha256 = ?`, sha(bad)); err != nil {
		t.Fatal(err)
	}
	_, err = s.Reissue(bg, ui, pw1, IssueOptions{})
	wantCode(t, err, ErrPassphraseInvalid)
	msgs = append(msgs, err.Error())
	if e.fk.dummy != 2 {
		t.Fatalf("dummy derivations = %d", e.fk.dummy)
	}
	mustWrite(t, e, RootFile, good)
	if _, err := e.st.DB().Exec(`UPDATE hub_identity SET root_file_sha256 = ?`, sha(good)); err != nil {
		t.Fatal(err)
	}
	for _, m := range msgs {
		if m != msgs[0] || strings.Contains(m, pw1) || strings.Contains(m, pw2) {
			t.Fatalf("answers differ or leak: %q", msgs)
		}
	}
	if e.row().Web.Failures != 3 {
		t.Fatalf("failures = %d", e.row().Web.Failures)
	}
	e.allSignersDestroyed()
}

func TestCanaryPassphraseNeverAppearsAnywhere(t *testing.T) {
	e := newEnv(t)
	canary, bad := "CANARY-passphrase-one-9183", "CANARY-wrong-pass-phrase-7731"
	s := e.svc()
	if _, err := s.SetPassphrase(bg, ui, canary, canary, IssueOptions{}); err != nil {
		t.Fatal(err)
	}
	_, _ = s.Reissue(bg, ui, bad, IssueOptions{})
	e.point = func(n string) error {
		if n == PointAfterUnseal {
			panic("injected")
		}
		return nil
	}
	func() {
		defer func() { _ = recover() }()
		_, _ = s.Reissue(bg, ui, canary, IssueOptions{})
	}()
	e.point = nil
	if _, err := s.ChangePassphrase(bg, ui, canary, "CANARY-new-pass-phrase-4412", "CANARY-new-pass-phrase-4412"); err != nil {
		t.Fatal(err)
	}
	var all strings.Builder
	all.WriteString(e.log.String())
	rows, _ := e.st.ListAudit(bg, store.AuditQuery{Limit: 1000})
	for _, r := range rows {
		all.WriteString(r.Actor + r.Action + r.Target + r.Outcome)
	}
	for _, f := range []string{PlainFile, RootFile, OpFile} {
		if e.has(f) {
			all.WriteString(string(mustRead(t, e, f)))
		}
	}
	if strings.Contains(all.String(), "CANARY") {
		t.Fatal("a passphrase appears in the log, audit rows or files")
	}
	// Fake sealer keeps the hash, not the text; nothing else stores it.
	e.allSignersDestroyed()
}

func TestPanicAfterUnsealDestroysSignersAndReleasesTheMutex(t *testing.T) {
	e := newEnv(t)
	s := e.established()
	e.point = func(n string) error {
		if n == PointAfterUnseal {
			panic("boom")
		}
		return nil
	}
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("no panic")
			}
		}()
		_, _ = s.RotateMaster(bg, ui, pw1, pw2, pw2, IssueOptions{})
	}()
	e.allSignersDestroyed()
	e.point = nil
	if _, err := s.Reissue(bg, ui, pw1, IssueOptions{}); err != nil {
		t.Fatalf("the mutex stayed locked or the state is damaged: %v", err)
	}
}

func TestSessionsAreClosedAfterASwapEvenOnPanicOrError(t *testing.T) {
	e := newEnv(t)
	s := e.established()
	closes := e.closes
	e.point = func(n string) error {
		if n == PointSwapped {
			panic("after swap")
		}
		return nil
	}
	func() {
		defer func() { _ = recover() }()
		_, _ = s.Reissue(bg, ui, pw1, IssueOptions{})
	}()
	if e.closes != closes+1 {
		t.Fatal("D7: sessions not closed after a panic following the swap")
	}
	e.point = nil
	// The swap happened, so the pending rename is completed by the next call.
	if _, err := s.Reissue(bg, ui, pw1, IssueOptions{}); err != nil {
		t.Fatal(err)
	}
	if string(e.opPub(OpFile)) != string(e.cert().OpPub) || e.has(OpNextFile) {
		t.Fatal("files after recovery")
	}
	// An error (not a panic) after the swap also closes.
	closes = e.closes
	e.rename = func(_, _ string) error { return errors.New("disk full") }
	_, err := s.Reissue(bg, ui, pw1, IssueOptions{})
	wantCode(t, err, ErrFilesPending)
	if e.closes != closes+1 {
		t.Fatal("D7: sessions not closed after a rename error")
	}
}

func TestOnlyOneIdentityOperationAtATime(t *testing.T) {
	e := newEnv(t)
	s := e.established()
	enter, release := make(chan struct{}), make(chan struct{})
	e.point = func(n string) error {
		if n == PointAfterUnseal {
			close(enter)
			<-release
		}
		return nil
	}
	done := make(chan error, 1)
	go func() { _, err := s.Reissue(bg, ui, pw1, IssueOptions{}); done <- err }()
	<-enter
	e.point = nil
	_, err := s.Reissue(bg, cli, pw1, IssueOptions{})
	wantCode(t, err, ErrConflict)
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func mustRead(t *testing.T, e *env, name string) []byte {
	t.Helper()
	b, err := osReadFile(e.dir, name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func mustWrite(t *testing.T, e *env, name string, b []byte) {
	t.Helper()
	if err := osWriteFile(e.dir, name, b); err != nil {
		t.Fatal(err)
	}
}
