package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/transport"
	"github.com/patrick-1984/ai-minion/internal/worker/store"
)

// hubRoot is a fixed key for the box tests; its words are what the Worker
// shows as the Hub fingerprint.
var hubRoot = func() []byte {
	k, err := identity.GenerateKey()
	if err != nil {
		panic(err)
	}
	return k.Public()
}()

// pairHelloStep is a Hub since v0.12.0: the hello (root) arrives while the
// Worker waits; then it fails with err, or waits for the end of the context.
func pairHelloStep(admit bool, err error) pairStep {
	return func(ctx context.Context, p PairParams) (PairResult, error) {
		p.OnWaiting()
		p.OnHubHello(hubRoot, admit)
		if err != nil {
			return PairResult{}, err
		}
		<-ctx.Done()
		return PairResult{}, context.Cause(ctx)
	}
}

// peerReasonErr is an unauthenticated error frame with a reason token, as the
// transport hands it on.
func peerReasonErr(reason string) error {
	e := proto.NewError(proto.CodePairingCodeInvalid, "reported by a peer that is not authenticated")
	e.Details = json.RawMessage(`{"reason":"` + reason + `"}`)
	return e
}

// A39: without an expected fingerprint the box shows the words of the Hub that
// answered, confirmed by the code, and never the old "NOT CHECKED ... stop,
// and start again" text.
func TestTheCodeBoxShowsTheHubWordsConfirmedByTheCode(t *testing.T) {
	f := newPairFixture(t, pairHelloStep(false, nil))
	f.run(f.pairer())
	eventually(t, func() bool { return codeRE.MatchString(f.out.String()) })
	out := f.out.String()
	words := fingerprintOfPub(hubRoot).Words()
	mustContain(t, out, "Hub fingerprint:  "+words, string(fingerprintOfPub(hubRoot)),
		"confirmed by the pairing code: the Hub", "Workers > Add worker", "Enter the code on the Hub")
	mustLack(t, out, "NOT CHECKED", "stop, and start again", "whichever Hub", "Pairing requests")
	for _, l := range strings.Split(out, "\n") {
		if len(l) > 80 {
			t.Errorf("box line longer than 80 columns: %q", l)
		}
	}
}

// A41: when the Hub may admit the request, the box says both ways.
func TestTheCodeBoxOffersThePairingRequestWhenAdmitIsAllowed(t *testing.T) {
	f := newPairFixture(t, pairHelloStep(true, nil))
	f.run(f.pairer())
	eventually(t, func() bool { return codeRE.MatchString(f.out.String()) })
	mustContain(t, f.out.String(), "Enter the code on the Hub (Workers > Add worker), or admit the",
		"request in Workers > Pairing requests after checking these words", "Worker fingerprint:")
}

// An older Hub sends its hello only after the claim: the box comes without the
// Hub's words (the owner allowed leaving the line out) and without admission.
func TestTheCodeBoxOfAnOlderHubHasNoHubLine(t *testing.T) {
	f := newPairFixture(t, pairWaitStep(nil))
	f.run(f.pairer())
	eventually(t, func() bool { return codeRE.MatchString(f.out.String()) })
	mustLack(t, f.out.String(), "Hub fingerprint:", "NOT CHECKED", "Pairing requests")
}

func TestAnExpectedFingerprintKeepsItsLines(t *testing.T) {
	m, err := ParseHubExpectation(string(fingerprintOfPub(hubRoot)))
	if err != nil {
		t.Fatal(err)
	}
	f := newPairFixture(t, pairHelloStep(true, nil))
	f.run(f.pairer(func(c *PairerConfig) { c.ExpectHub = m }))
	eventually(t, func() bool { return codeRE.MatchString(f.out.String()) })
	mustContain(t, f.out.String(), "(expected; any other Hub is refused)")
	mustLack(t, f.out.String(), "confirmed by the pairing code")
}

func TestAnAdmittedPairingSaysSoInThePairedBox(t *testing.T) {
	f := newPairFixture(t, func(_ context.Context, p PairParams) (PairResult, error) {
		p.OnWaiting()
		p.OnHubHello(hubRoot, true)
		return PairResult{
			Pin:      identity.Pin{Pub: hubRoot},
			Welcome:  HubWelcome{Type: proto.TypeWelcome, WorkerID: "w-1", Name: p.Name, Owner: "admin", Kind: "private"},
			Version:  proto.Current,
			Admitted: true,
		}, nil
	})
	r, _ := f.run(f.pairer())
	r.wait(t)
	if r.err != nil {
		t.Fatal(r.err)
	}
	if !f.dial.call(0).AllowAdmit {
		t.Error("the pairer did not allow an admission")
	}
	mustContain(t, f.out.String(), "MINION WORKER PAIRED", "Admitted on the Hub without the code")
	if r.enr.HubCheck != store.HubCheckNone {
		t.Errorf("hub_check = %q, want none (the Worker did not check the Hub itself)", r.enr.HubCheck)
	}
	mustContain(t, f.logs.String(), "paired_by=admitted")
}

func TestNoAdmitTurnsTheRequestOff(t *testing.T) {
	f := newPairFixture(t, pairWaitStep(nil))
	f.run(f.pairer(func(c *PairerConfig) { c.NoAdmit = true }))
	eventually(t, func() bool { return f.dial.nCalls() >= 1 })
	if f.dial.call(0).AllowAdmit {
		t.Error("AllowAdmit with NoAdmit")
	}
}

func TestARejectedRequestShowsANewCodeWithTheReason(t *testing.T) {
	f := newPairFixture(t, pairHelloStep(true, peerReasonErr("rejected")), pairHelloStep(true, nil))
	f.run(f.pairer())
	eventually(t, func() bool { return len(codeRE.FindAllString(f.out.String(), -1)) >= 2 })
	codes := codeRE.FindAllString(f.out.String(), -1)
	if codes[0] == codes[1] {
		t.Fatal("the code was kept after a rejection")
	}
	second := f.out.String()[strings.Index(f.out.String(), codes[1])-900:]
	mustContain(t, second, "The pairing request was rejected on the Hub")
}

func TestAdmitNeedsAnExpectationOrATrustedCertificate(t *testing.T) {
	m, err := ParseHubExpectation(string(fingerprintOfPub(hubRoot)))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		expect identity.FingerprintMatcher
		seen   transport.TLSSeen
		want   bool
	}{
		{"nothing", identity.FingerprintMatcher{}, transport.TLSSeen{KeyHash: [32]byte{1}}, false},
		{"plain", identity.FingerprintMatcher{}, transport.TLSSeen{None: true, Trusted: true}, false},
		{"trusted certificate", identity.FingerprintMatcher{}, transport.TLSSeen{KeyHash: [32]byte{1}, Trusted: true}, true},
		{"expected fingerprint", m, transport.TLSSeen{None: true}, true},
	} {
		if got := admitTrust(tc.expect, tc.seen); got != tc.want {
			t.Errorf("%s: %v", tc.name, got)
		}
	}
}

// ---- A40: cancel and start again ------------------------------------------

// The Hub ended the wait because the code expired (its reason token): the
// Worker shows the new code at once, in one box with the reason, as the Hub's
// "the Worker shows a new one" promises; it never leaves a lone "expired".
func TestTheHubsExpiredReasonShowsTheNewCodeAtOnce(t *testing.T) {
	f := newPairFixture(t, pairHelloStep(false, peerReasonErr("expired")), pairHelloStep(false, nil))
	f.run(f.pairer())
	eventually(t, func() bool { return len(codeRE.FindAllString(f.out.String(), -1)) >= 2 })
	out := f.out.String()
	codes := codeRE.FindAllString(out, -1)
	box := out[strings.LastIndex(out, "MINION WORKER PAIRING"):]
	mustContain(t, box, "The pairing code expired.", codes[1])
}

// The notice of a new code stays with it when the first attempt with that code
// never reached the Hub: it is printed in the box of that code later.
func TestTheNoticeIsNotLostWhenTheNextAttemptFailsBeforeTheHub(t *testing.T) {
	f := newPairFixture(t,
		pairHelloStep(false, peerReasonErr("expired")),
		pairFailStep(errors.New("dial the hub: connection refused")),
		pairHelloStep(false, nil))
	f.run(f.pairer())
	eventually(t, func() bool { return f.dial.nCalls() >= 2 })
	for i := 0; i < 50 && len(codeRE.FindAllString(f.out.String(), -1)) < 2; i++ {
		f.clk.Advance(time.Minute) // the backoff after the failed dial
		time.Sleep(5 * time.Millisecond)
	}
	out := f.out.String()
	if n := len(codeRE.FindAllString(out, -1)); n != 2 {
		t.Fatalf("codes shown = %d:\n%s", n, out)
	}
	box := out[strings.LastIndex(out, "MINION WORKER PAIRING"):]
	mustContain(t, box, "The pairing code expired.")
}

func TestACancelledPairingStartedAgainShowsACodeAtOnce(t *testing.T) {
	f := newPairFixture(t, pairHelloStep(false, nil))
	r, cancel := f.run(f.pairer())
	eventually(t, func() bool { return codeRE.MatchString(f.out.String()) })
	cancel() // Ctrl+C
	r.wait(t)

	// Within the lifetime of the first code: a new run shows a code at once.
	f.out = &logBuf{}
	r2, cancel2 := f.run(f.pairer())
	eventually(t, func() bool { return codeRE.MatchString(f.out.String()) })
	mustLack(t, f.out.String(), "expired")
	cancel2()
	r2.wait(t)

	// After it: the same.
	f.clk.Advance(PairCodeTTL + time.Minute)
	f.out = &logBuf{}
	f.run(f.pairer())
	eventually(t, func() bool { return codeRE.MatchString(f.out.String()) })
	mustLack(t, f.out.String(), "expired")
}

// A run interrupted after the Hub accepted the code left a pair_resume record.
// Started again within its lifetime the same code is shown at once; after it,
// a new code is shown in the same box as the note that the old one expired.
func TestAnInterruptedPairingWithARecordShowsACodeAtOnce(t *testing.T) {
	code := identity.NewPairingCode()
	f := newPairFixture(t, pairHelloStep(false, nil))
	if _, err := f.enr.LoadOrCreateKey(); err != nil {
		t.Fatal(err)
	}
	if err := f.st.SavePairResume(context.Background(), store.PairResume{
		Code: code.String(), PairingID: code.PairingID(), ExpiresAt: f.clk.Now().Add(5 * time.Minute), RootPub: hubRoot,
		Serial: 1, CertSHA256: make([]byte, 32),
	}); err != nil {
		t.Fatal(err)
	}
	r, cancel := f.run(f.pairer())
	eventually(t, func() bool { return strings.Contains(f.out.String(), code.String()) })
	mustContain(t, f.out.String(), "Resuming an interrupted pairing with the same code.")
	cancel()
	r.wait(t)

	f.clk.Advance(6 * time.Minute) // the record expired meanwhile
	f.out = &logBuf{}
	f.run(f.pairer())
	eventually(t, func() bool { return codeRE.MatchString(f.out.String()) })
	out := f.out.String()
	if strings.Contains(out, code.String()) {
		t.Fatal("the expired code was shown again")
	}
	box := out[strings.LastIndex(out, "MINION WORKER PAIRING"):]
	mustContain(t, box, "The code of the interrupted pairing expired. Use this new code.", codeRE.FindString(out))
}

func mustContain(t *testing.T, s string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(s, w) {
			t.Errorf("missing %q in:\n%s", w, s)
		}
	}
}

func mustLack(t *testing.T, s string, unwanted ...string) {
	t.Helper()
	for _, w := range unwanted {
		if strings.Contains(s, w) {
			t.Errorf("unexpected %q in:\n%s", w, s)
		}
	}
}
