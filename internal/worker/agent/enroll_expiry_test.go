package agent

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/transport"
)

// realClockWM is a fake WallMono that starts at the real time (so that the
// certificate window check passes) and that the test can move.
func realClockWM() *fakeWM {
	wm := newFakeWM()
	wm.set(time.Now(), 10*time.Hour)
	return wm
}

func inviteParamsFor(t *testing.T, h *inviteHub, invite identity.InviteKey, wm transport.WallMono) InviteParams {
	t.Helper()
	k, err := identity.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	return InviteParams{HubURL: h.url(), Key: k, Name: "pi", Invite: invite, Clock: wm}
}

func TestInviteSecretIsNotSentAfterTheCertificateDeadline(t *testing.T) {
	for _, tc := range []struct {
		name string
		jump func(wm *fakeWM, e time.Time)
	}{
		{"wall clock at not_after", func(wm *fakeWM, e time.Time) { wm.set(e, wm.Mono()) }},
		{"monotonic clock at the deadline", func(wm *fakeWM, e time.Time) { wm.set(wm.Wall(), wm.Mono()+365*24*time.Hour) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, invite := newInviteHub(t)
			wm := realClockWM()
			d := inviteDialer{dialTimeout: 5 * time.Second, handshakeTimeout: 10 * time.Second}
			d.afterHandshake = func() { tc.jump(wm, time.UnixMilli(h.hub.Cert.NotAfterMs)) }
			_, err := d.Enroll(context.Background(), inviteParamsFor(t, h, invite, wm))
			if !errors.Is(err, errEnrollExpired) {
				t.Fatalf("err = %v, want errEnrollExpired", err)
			}
			if got := h.secretsReceived(); len(got) != 0 {
				t.Fatalf("the secret was sent after the deadline: %v", got)
			}
		})
	}
}

func TestInviteWelcomeAfterTheDeadlineIsNotAccepted(t *testing.T) {
	h, invite := newInviteHub(t)
	wm := realClockWM()
	d := inviteDialer{dialTimeout: 5 * time.Second, handshakeTimeout: 10 * time.Second}
	d.afterReply = func() { wm.set(time.UnixMilli(h.hub.Cert.NotAfterMs), wm.Mono()) }
	out, err := d.Enroll(context.Background(), inviteParamsFor(t, h, invite, wm))
	if !errors.Is(err, errEnrollExpired) || out != nil {
		t.Fatalf("out %v err %v, want errEnrollExpired", out, err)
	}
}

func TestInviteChannelBlockedAtTheDeadlineIsClosed(t *testing.T) {
	h, invite := newInviteHub(t)
	h.holdAnswer = true // the Hub takes the secret and never answers
	now := time.Now()
	h.hub.Cert = h.hub.IssueAt(t, 1, now.Add(-time.Hour).UnixMilli(), now.Add(400*time.Millisecond).UnixMilli())
	d := inviteDialer{dialTimeout: 5 * time.Second, handshakeTimeout: 10 * time.Second}
	start := time.Now()
	_, err := d.Enroll(context.Background(), inviteParamsFor(t, h, invite, nil))
	if !errors.Is(err, errEnrollExpired) {
		t.Fatalf("err = %v, want errEnrollExpired", err)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("the blocked read lasted %s: the channel was not closed at the deadline", took)
	}
}

func TestInviteEnrollerDoesNotCommitAWelcomeThatCameInAfterTheDeadline(t *testing.T) {
	f := newPairFixture(t)
	wm := newFakeWM()
	exp := wm.expiryAfter(time.Hour)
	wm.set(time.UnixMilli(exp.WallMs).UTC(), wm.Mono()) // the dialer returned after not_after
	d := stubInviteDialer(func(context.Context, InviteParams) (*InviteOutcome, error) {
		return &InviteOutcome{Pin: identity.Pin{Pub: make([]byte, 32)}, Welcome: HubWelcome{WorkerID: "w-1", Name: "pi"}, Expiry: &exp}, nil
	})
	e, err := NewInviteEnroller(InviteEnrollerConfig{State: f.enr, Dialer: d, HubURL: "https://hub.example", Name: "pi", WallMono: wm, Attempts: 1})
	if err != nil {
		t.Fatal(err)
	}
	invite, err := identity.NewInviteKey(identity.Fingerprint(mustFingerprint(t)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Enroll(context.Background(), invite); !errors.Is(err, errEnrollExpired) {
		t.Fatalf("err = %v, want errEnrollExpired", err)
	}
	if _, ok, _ := f.enr.Load(context.Background()); ok {
		t.Fatal("an enrollment was committed after not_after")
	}
}

type stubInviteDialer func(context.Context, InviteParams) (*InviteOutcome, error)

func (f stubInviteDialer) Enroll(ctx context.Context, p InviteParams) (*InviteOutcome, error) {
	return f(ctx, p)
}

func mustFingerprint(t *testing.T) identity.Fingerprint {
	t.Helper()
	k, err := identity.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	return k.Fingerprint()
}

func TestPairingWelcomeIsNotCommittedAfterTheDeadline(t *testing.T) {
	wm := newFakeWM()
	exp := wm.expiryAfter(time.Hour)
	hubPub := make([]byte, 32)
	ok := pairOkStep(hubPub, "alice")
	f := newPairFixture(t, func(ctx context.Context, p PairParams) (PairResult, error) {
		res, err := ok(ctx, p)
		res.Expiry = &exp
		// The welcome came in before not_after; the commit happens after it.
		wm.set(time.UnixMilli(exp.WallMs).UTC(), wm.Mono())
		return res, err
	})
	r, _ := f.run(f.pairer(func(c *PairerConfig) { c.WallMono = wm }))
	r.wait(t)
	if !errors.Is(r.err, errEnrollExpired) {
		t.Fatalf("err = %v, want errEnrollExpired", r.err)
	}
	if _, ok, _ := f.enr.Load(context.Background()); ok {
		t.Fatal("an enrollment was committed after not_after")
	}
	if got := f.out.String(); strings.Contains(got, "paired to") {
		t.Fatalf("success was reported:\n%s", got)
	}
}

func TestPairingWelcomeReadAfterTheDeadlineIsNotAccepted(t *testing.T) {
	// The production dialer: the deadline passes while the welcome is read.
	h := newStubPairHub(t)
	wm := realClockWM()
	fired := make(chan struct{}, 8)
	d := wssPairDialer{dialTimeout: 5 * time.Second, afterReply: func() {
		wm.set(time.UnixMilli(h.hub.Cert.NotAfterMs), wm.Mono())
		fired <- struct{}{}
	}}
	hp := newHubPairer(t, h.url(), func(c *PairerConfig) { c.Dialer = d; c.WallMono = wm })
	r := hp.start(t)
	code := hp.code(t, 1)
	eventually(t, func() bool { return h.lastID() == code[3:7] })
	h.claim(code)
	select {
	case <-fired:
	case <-time.After(10 * time.Second):
		t.Fatal("the welcome was never read")
	}
	select {
	case <-r.done:
		t.Fatalf("pairing ended: enrollment %+v err %v", r.enr, r.err)
	case <-time.After(300 * time.Millisecond):
	}
	if _, ok, _ := hp.enr.Load(context.Background()); ok {
		t.Fatal("an enrollment was committed from a welcome that arrived after not_after")
	}
}
