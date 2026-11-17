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
		"words":    fp.Words() + "\n",
		"prefixes": strings.Join(prefixes, " ") + "\n",
		"full":     string(fp) + "\n",
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
		if !strings.Contains(out.String(), "HUB FINGERPRINT DOES NOT MATCH - WORKER DISCONNECTED") ||
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
	res, err = VerifyHub(ctx, f.enr, mustMatcher(t, f.hub.Fingerprint().Words()))
	if err != nil || res.Outcome != VerifyMatch || f.load(t).Check() != store.HubCheckFingerprint {
		t.Fatalf("match: %+v %v", res, err)
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
	f.waitIdle("the Hub does not match the fingerprint you gave; see minion worker verify-hub")
	if f.dial.count() != 0 {
		t.Fatal("the connector dialled a quarantined Hub")
	}
	// A match leaves the quarantine and the connector dials.
	sess := newFakeSession()
	f.dial.script(f.welcomeStep(sess, okWelcome()))
	if err := f.enr.SetHubCheck(context.Background(), identity.Pin{Pub: f.hub.Public()}, store.HubCheckFingerprint, string(f.hub.Fingerprint())); err != nil {
		t.Fatal(err)
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
