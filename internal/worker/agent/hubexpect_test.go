package agent

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/transport"
	"github.com/patrick-1984/ai-minion/internal/worker/store"
)

func mustMatcher(t *testing.T, in string) identity.FingerprintMatcher {
	t.Helper()
	m, err := identity.ParseFingerprintInput(in)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestAskHubFingerprint(t *testing.T) {
	k, err := identity.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	fp := k.Fingerprint()
	words := strings.Split(fp.Words(), "-")
	prefixes := make([]string, len(words))
	for i, w := range words {
		prefixes[i] = w[:min(4, len(w))]
	}
	for name, in := range map[string]string{
		"words":                             fp.Words() + "\n",
		"prefixes":                          strings.Join(prefixes, " ") + "\n",
		"full":                              string(fp) + "\n",
		"invalid then valid, empty repeats": "nope\n\n" + fp.Words() + "\n",
	} {
		var out bytes.Buffer
		m, err := AskHubFingerprint(strings.NewReader(in), &out, "https://hub.example")
		if err != nil || !m.Matches(k.Public()) {
			t.Fatalf("%s: %v %v\n%s", name, m, err, out.String())
		}
		if !strings.Contains(out.String(), "NEW HUB - ENTER ITS FINGERPRINT") || !strings.Contains(out.String(), "Expected Hub fingerprint: "+fp.Words()) {
			t.Fatalf("%s: output\n%s", name, out.String())
		}
	}
	var out bytes.Buffer
	m, err := AskHubFingerprint(strings.NewReader("\n\nSKIP\n"), &out, "https://hub.example")
	if err != nil || !m.IsZero() || !strings.Contains(out.String(), `"not verified"`) {
		t.Fatalf("skip: %v %v", m, err)
	}
	if strings.Count(out.String(), "Hub fingerprint: ") != 3 {
		t.Fatalf("an empty line must repeat the question:\n%s", out.String())
	}
	for _, in := range []string{"", "\n\n", "nope"} {
		if _, err := AskHubFingerprint(strings.NewReader(in), io.Discard, "https://hub.example"); !errors.Is(err, ErrPromptAborted) {
			t.Fatalf("end of input %q: %v", in, err)
		}
	}
	// The answer is read without read-ahead: the next line stays.
	r := strings.NewReader(fp.Words() + "\ny\n")
	if _, err := AskHubFingerprint(r, io.Discard, "https://hub.example"); err != nil {
		t.Fatal(err)
	}
	if rest, _ := io.ReadAll(r); string(rest) != "y\n" {
		t.Fatalf("rest = %q", rest)
	}
}

func TestParseHubExpectation(t *testing.T) {
	if m, err := ParseHubExpectation("  "); err != nil || !m.IsZero() {
		t.Fatal("empty must be no expectation")
	}
	if _, err := ParseHubExpectation("abc"); err == nil || !strings.Contains(err.Error(), "hub_fingerprint") {
		t.Fatalf("malformed: %v", err)
	}
}

type checkFixture struct {
	enr  *Enrollments
	hub  *identity.Key
	logs *logBuf
	log  *slog.Logger
}

func newCheckFixture(t *testing.T, check, anchor string) *checkFixture {
	t.Helper()
	e, _, _ := newEnrollments(t)
	hub, err := identity.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Commit(context.Background(), Enrollment{Pin: identity.Pin{Pub: hub.Public()}, HubCheck: check, HubCheckAnchor: anchor}); err != nil {
		t.Fatal(err)
	}
	logs := &logBuf{}
	return &checkFixture{enr: e, hub: hub, logs: logs, log: slog.New(slog.NewTextHandler(logs, nil))}
}

func (f *checkFixture) load(t *testing.T) Enrollment {
	t.Helper()
	enr, ok, err := f.enr.Load(context.Background())
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	return enr
}

func TestCheckEnrolledHubThreeCases(t *testing.T) {
	ctx := context.Background()
	// 1. The setting names the pin: fingerprint.
	f := newCheckFixture(t, "", "")
	var out bytes.Buffer
	if err := CheckEnrolledHub(ctx, f.enr, mustMatcher(t, f.hub.Fingerprint().Words()), &out, f.log); err != nil {
		t.Fatal(err)
	}
	if enr := f.load(t); enr.Check() != store.HubCheckFingerprint || enr.HubCheckAnchor != string(f.hub.Fingerprint()) || out.Len() != 0 {
		t.Fatalf("match: %+v %q", enr, out.String())
	}
	if !strings.Contains(f.logs.String(), "the paired Hub matches hub_fingerprint") {
		t.Fatal(f.logs.String())
	}

	// 2. The setting names the checked key, the pin moved by a rotation.
	old, _ := identity.GenerateKey()
	f = newCheckFixture(t, store.HubCheckFingerprint, string(old.Fingerprint()))
	if err := CheckEnrolledHub(ctx, f.enr, mustMatcher(t, string(old.Fingerprint())), &out, f.log); err != nil {
		t.Fatal(err)
	}
	if enr := f.load(t); enr.Check() != store.HubCheckFingerprint || out.Len() != 0 ||
		!strings.Contains(f.logs.String(), "Update the setting to "+string(f.hub.Fingerprint())) {
		t.Fatalf("rotated: %+v %s", enr, f.logs.String())
	}
	if line := hubCheckLine(f.load(t)); !strings.Contains(line, "reached by verified rotation from "+old.Fingerprint().Words()) {
		t.Fatalf("status line %q", line)
	}

	// 3. Anything else: mismatch, box M6, and it stays after a restart
	// without the setting.
	for _, check := range []string{"", store.HubCheckInvite, store.HubCheckFingerprint} {
		f = newCheckFixture(t, check, "")
		out.Reset()
		other, _ := identity.GenerateKey()
		gen0, _ := f.enr.Watch()
		if err := CheckEnrolledHub(ctx, f.enr, mustMatcher(t, string(other.Fingerprint())), &out, f.log); err != nil {
			t.Fatal(err)
		}
		if enr := f.load(t); enr.Check() != store.HubCheckMismatch {
			t.Fatalf("mismatch from %q: %+v", check, enr)
		}
		if gen1, _ := f.enr.Watch(); gen1 == gen0 {
			t.Fatal("entering mismatch must move the generation")
		}
		if !strings.Contains(out.String(), MismatchTitle) || strings.Contains(out.String(), "DISCONNECTED") ||
			!strings.Contains(out.String(), "The setting hub_fingerprint says:") ||
			!strings.Contains(f.logs.String(), "the paired Hub does not match hub_fingerprint; not connecting") {
			t.Fatalf("box/log:\n%s\n%s", out.String(), f.logs.String())
		}
		if err := CheckEnrolledHub(ctx, f.enr, identity.FingerprintMatcher{}, &out, f.log); err != nil {
			t.Fatal(err)
		}
		if f.load(t).Check() != store.HubCheckMismatch {
			t.Fatal("the quarantine must survive a start without the setting")
		}
	}
}

func TestVerifyHubAndForget(t *testing.T) {
	ctx := context.Background()
	f := newCheckFixture(t, "", "")
	other, _ := identity.GenerateKey()

	res, err := VerifyHub(ctx, f.enr, mustMatcher(t, string(other.Fingerprint())))
	if err != nil || res.Outcome != VerifyMismatch || res.Paired != f.hub.Fingerprint() || f.load(t).Check() != store.HubCheckMismatch {
		t.Fatalf("mismatch: %+v %v", res, err)
	}
	// Words do not end a quarantine, only the full pinned key does.
	res, err = VerifyHub(ctx, f.enr, mustMatcher(t, f.hub.Fingerprint().Words()))
	if err != nil || res.Outcome != VerifyNeedFull || !res.Quarantined || f.load(t).Check() != store.HubCheckMismatch {
		t.Fatalf("words: %+v %v", res, err)
	}
	res, err = VerifyHub(ctx, f.enr, mustMatcher(t, string(f.hub.Fingerprint())))
	if err != nil || res.Outcome != VerifyMatch || res.Quarantined || f.load(t).Check() != store.HubCheckFingerprint {
		t.Fatalf("match: %+v %v", res, err)
	}
	// Outside a quarantine words are enough.
	if res, err = VerifyHub(ctx, f.enr, mustMatcher(t, f.hub.Fingerprint().Words())); err != nil || res.Outcome != VerifyMatch {
		t.Fatalf("words, not quarantined: %+v %v", res, err)
	}

	if err := ForgetHub(ctx, f.enr, other.Fingerprint()); !errors.Is(err, ErrPinnedDiffers) {
		t.Fatalf("forget another key: %v", err)
	}
	if err := ForgetHub(ctx, f.enr, f.hub.Fingerprint()); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := f.enr.Load(ctx); ok {
		t.Fatal("forget must remove the pairing")
	}
	if got, err := ForgottenHubs(f.enr.stateDir); err != nil || len(got) != 1 || got[0] != string(f.hub.Fingerprint()) {
		t.Fatalf("the removal must be recorded: %v %v", got, err)
	}
	if _, err := VerifyHub(ctx, f.enr, mustMatcher(t, string(other.Fingerprint()))); !errors.Is(err, ErrNotPaired) {
		t.Fatalf("not paired: %v", err)
	}
	if err := ForgetHub(ctx, f.enr, other.Fingerprint()); !errors.Is(err, ErrNotPaired) {
		t.Fatalf("forget, not paired: %v", err)
	}
}

func TestConnectorStaysIdleOnAStoredMismatch(t *testing.T) {
	f := newConnFixture(t)
	if err := f.enr.SetHubCheck(context.Background(), identity.Pin{Pub: f.hub.Public()}, store.HubCheckMismatch, ""); err != nil {
		t.Fatal(err)
	}
	f.drainChanged()
	f.run()
	f.waitIdle(ReasonQuarantined)
	if f.dial.count() != 0 {
		t.Fatal("the connector dialled a quarantined Hub")
	}
	// A plain SetHubCheck (a start that read the enrollment earlier) cannot
	// leave the quarantine.
	if err := f.enr.SetHubCheck(context.Background(), identity.Pin{Pub: f.hub.Public()}, store.HubCheckFingerprint, string(f.hub.Fingerprint())); !errors.Is(err, ErrQuarantined) {
		t.Fatalf("SetHubCheck left the quarantine: %v", err)
	}
	// verify-hub with the full pinned key leaves it and the connector dials.
	sess := newFakeSession()
	f.dial.script(f.welcomeStep(sess, okWelcome()))
	if res, err := VerifyHub(context.Background(), f.enr, mustMatcher(t, string(f.hub.Fingerprint()))); err != nil || res.Outcome != VerifyMatch {
		t.Fatal(res, err)
	}
	f.waitState(ConnConnected)
}

func TestConnectorIdentityChangedAlarmOncePerStreak(t *testing.T) {
	var out bytes.Buffer
	f := newConnFixture(t, func(c *ConnectorConfig) { c.Out = &out })
	other, _ := identity.GenerateKey()
	refuse := func(cause error) dialStep {
		return func(ConnectParams) (*Attempt, error) {
			return nil, &transport.HubKeyError{Presented: other.Public(), Err: cause}
		}
	}
	sess := newFakeSession()
	f.dial.script(refuse(transport.ErrPinMismatch), refuse(transport.ErrPinMismatch), f.welcomeStep(sess, okWelcome()))
	f.run()
	eventually(t, func() bool { return f.conn.Status().Alarm == AlarmHubIdentityChanged })
	f.step(time.Minute)
	eventually(t, func() bool { return f.dial.count() >= 2 })
	f.step(time.Minute)
	f.waitState(ConnConnected)
	if n := strings.Count(out.String(), "HUB IDENTITY CHANGED - NOT CONNECTING"); n != 1 {
		t.Fatalf("box M5 shown %d times:\n%s", n, out.String())
	}
	if !strings.Contains(out.String(), other.Fingerprint().Words()) || !strings.Contains(out.String(), "It is another key") {
		t.Fatalf("box:\n%s", out.String())
	}
	if f.conn.Status().Alarm != "" {
		t.Fatal("a connection must clear the alarm")
	}
	enr, _, _ := f.enr.Load(context.Background())
	if !bytes.Equal(enr.Pin.Pub, f.hub.Public()) {
		t.Fatal("the pin must be unchanged")
	}
	if strings.Count(f.logs.String(), "hub identity changed; not connecting") != 1 {
		t.Fatalf("log:\n%s", f.logs.String())
	}
}

func TestConnectorInvalidChainReason(t *testing.T) {
	var out bytes.Buffer
	f := newConnFixture(t, func(c *ConnectorConfig) { c.Out = &out })
	other, _ := identity.GenerateKey()
	f.dial.script(func(ConnectParams) (*Attempt, error) {
		return nil, &transport.HubKeyError{Presented: other.Public(), Err: transport.ErrChainInvalid}
	})
	f.run()
	eventually(t, func() bool { return strings.Contains(out.String(), "The Hub sent a record of a") })
}

// Finding 1 of the round-1 review: a quarantine stored by verify-hub is not
// left by a restart whose unchanged setting still names the pinned key.
func TestQuarantineSurvivesARestartWithTheMatchingSetting(t *testing.T) {
	ctx := context.Background()
	f := newCheckFixture(t, "", "")
	setting := mustMatcher(t, string(f.hub.Fingerprint()))
	if err := CheckEnrolledHub(ctx, f.enr, setting, io.Discard, f.log); err != nil {
		t.Fatal(err)
	}
	other, _ := identity.GenerateKey()
	if res, err := VerifyHub(ctx, f.enr, mustMatcher(t, string(other.Fingerprint()))); err != nil || res.Outcome != VerifyMismatch {
		t.Fatal(res, err)
	}
	var out bytes.Buffer
	if err := CheckEnrolledHub(ctx, f.enr, setting, &out, f.log); err != nil {
		t.Fatal(err)
	}
	if f.load(t).Check() != store.HubCheckMismatch {
		t.Fatal("a restart with the matching setting left the quarantine")
	}
	if !strings.Contains(out.String(), "WORKER QUARANTINED") || !strings.Contains(out.String(), "--forget --pinned "+string(f.hub.Fingerprint())) ||
		!strings.Contains(f.logs.String(), "quarantined") {
		t.Fatalf("note/log:\n%s\n%s", out.String(), f.logs.String())
	}
}

// Finding 7: a setting that names any key of the verified lineage (A -> B ->
// C, setting B, pin C) is an authenticated predecessor: no quarantine.
func TestCheckEnrolledHubAcceptsAKeyOfTheVerifiedLineage(t *testing.T) {
	ctx := context.Background()
	e, _, _ := newEnrollments(t)
	a, _ := identity.GenerateKey()
	b, _ := identity.GenerateKey()
	c, _ := identity.GenerateKey()
	if err := e.Commit(ctx, Enrollment{Pin: identity.Pin{Pub: a.Public()}, HubCheck: store.HubCheckFingerprint, HubCheckAnchor: string(a.Fingerprint())}); err != nil {
		t.Fatal(err)
	}
	gen, _ := e.Watch()
	// A -> B, then B -> C, each by a verified chain (one link each), and
	// also A -> B -> C in one commit on a second Worker.
	if err := e.CommitRatchetIfLive(ctx, gen, identity.Pin{Pub: b.Public(), Seq: 1}, transport.Ratchet{Root: b.Public()}, nil); err != nil {
		t.Fatal(err)
	}
	if err := e.CommitRatchetIfLive(ctx, gen, identity.Pin{Pub: c.Public(), Seq: 2}, transport.Ratchet{Root: c.Public()}, nil); err != nil {
		t.Fatal(err)
	}
	e2, _, _ := newEnrollments(t)
	if err := e2.Commit(ctx, Enrollment{Pin: identity.Pin{Pub: a.Public()}, HubCheck: store.HubCheckFingerprint, HubCheckAnchor: string(a.Fingerprint())}); err != nil {
		t.Fatal(err)
	}
	gen2, _ := e2.Watch()
	if err := e2.CommitRatchetIfLive(ctx, gen2, identity.Pin{Pub: c.Public(), Seq: 2}, transport.Ratchet{Root: c.Public()}, nil, b.Public()); err != nil {
		t.Fatal(err)
	}
	logs := &logBuf{}
	log := slog.New(slog.NewTextHandler(logs, nil))
	for _, en := range []*Enrollments{e, e2} {
		for _, k := range []*identity.Key{a, b} {
			var out bytes.Buffer
			if err := CheckEnrolledHub(ctx, en, mustMatcher(t, string(k.Fingerprint())), &out, log); err != nil {
				t.Fatal(err)
			}
			enr, _, _ := en.Load(ctx)
			if enr.Check() != store.HubCheckFingerprint || out.Len() != 0 {
				t.Fatalf("setting %s: %+v\n%s", k.Fingerprint().Words(), enr, out.String())
			}
		}
	}
	// A key outside the lineage is still a mismatch.
	other, _ := identity.GenerateKey()
	if err := CheckEnrolledHub(ctx, e, mustMatcher(t, string(other.Fingerprint())), io.Discard, log); err != nil {
		t.Fatal(err)
	}
	if enr, _, _ := e.Load(ctx); enr.Check() != store.HubCheckMismatch {
		t.Fatalf("other key: %+v", enr)
	}
}

// Finding 2: --forget is recorded; an explicit new enrollment ends the record.
func TestForgetIsRecordedUntilAnExplicitEnrollment(t *testing.T) {
	ctx := context.Background()
	f := newCheckFixture(t, "", "")
	if err := ForgetHub(ctx, f.enr, f.hub.Fingerprint()); err != nil {
		t.Fatal(err)
	}
	if got, _ := ForgottenHubs(f.enr.stateDir); len(got) != 1 {
		t.Fatalf("record = %v", got)
	}
	if err := f.enr.Commit(ctx, Enrollment{Pin: identity.Pin{Pub: f.hub.Public()}}); err != nil {
		t.Fatal(err)
	}
	if got, _ := ForgottenHubs(f.enr.stateDir); len(got) != 0 {
		t.Fatalf("a new enrollment must end the record: %v", got)
	}
}

// Finding 10: the hello facts carry this Worker's hub_check, so the Hub can
// show "not verified" on the approval page.
func TestConnectorSendsItsHubCheckInTheHelloFacts(t *testing.T) {
	f := newConnFixture(t)
	got := make(chan string, 1)
	sess := newFakeSession()
	welcome := f.welcomeStep(sess, okWelcome())
	f.dial.script(func(p ConnectParams) (*Attempt, error) {
		got <- p.Facts.HubCheck
		return welcome(p)
	})
	f.run()
	select {
	case c := <-got:
		if c != store.HubCheckNone {
			t.Fatalf("hub_check fact = %q", c)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no dial")
	}
}
