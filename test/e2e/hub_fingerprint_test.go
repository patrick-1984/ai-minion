//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/hubid"
	"github.com/patrick-1984/ai-minion/internal/hub/hubid/hubidtest"
	"github.com/patrick-1984/ai-minion/internal/hub/workers"
	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/proto"
	wagent "github.com/patrick-1984/ai-minion/internal/worker/agent"
	wstore "github.com/patrick-1984/ai-minion/internal/worker/store"
)

// v0.11.0 trust ceremony, end to end: the real Hub, the production pair and
// connect dialers, the real Pairer and connector, the Worker state on disk.

func hubFP(t *testing.T, h *connHub) identity.Fingerprint {
	t.Helper()
	f, err := identity.FingerprintOf(h.service().Identity().Public())
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func matcher(t *testing.T, in string) identity.FingerprintMatcher {
	t.Helper()
	m, err := identity.ParseFingerprintInput(in)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func prefixes(words string) string {
	var out []string
	for _, w := range strings.Split(words, "-") {
		out = append(out, w[:min(4, len(w))])
	}
	return strings.Join(out, " ")
}

func expectPairer(t *testing.T, h *connHub, w *compWorkerState, out io.Writer, code identity.PairingCode, expect identity.FingerprintMatcher) *wagent.Pairer {
	t.Helper()
	p, err := wagent.NewPairer(wagent.PairerConfig{
		HubURL: h.url(), Name: "pi", Ops: func() []string { return []string{"sessions_list"} },
		Enrollment: w.enr, Dialer: wagent.NewPairDialer(), Backoff: fastBackoff(),
		NewCode: func() identity.PairingCode { return code }, EpochPoll: 20 * time.Millisecond,
		ExpectHub: expect, Out: out,
	})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// pairAt pairs a fresh Worker state at h with the expectation and returns the
// state and the console output. claim says whether the code is claimed at h.
func pairAt(t *testing.T, h *connHub, ps func() *workers.Pairings, expect identity.FingerprintMatcher) (*compWorkerState, *pairRun, *syncLog) {
	t.Helper()
	w := newCompWorkerState(t)
	code := identity.NewPairingCode()
	out := &syncLog{}
	run, cancel := runPairer(expectPairer(t, h, w, out, code, expect))
	t.Cleanup(cancel)
	// Since v0.12.0 the Hub registers a pending pairing only after the
	// Worker's first CPace message: a Worker that refuses the Hub's root
	// leaves before that and simply ends.
	eventually(t, "the Hub to hold the Worker as pending, or the Worker to refuse", func() bool {
		select {
		case <-run.done:
			return true
		default:
		}
		return ps().IsPending(code.PairingID())
	})
	if _, err := ps().Claim(context.Background(), workers.ClaimRequest{UserID: "alice", Code: code.String(), IP: claimIP()}); err != nil &&
		expect.IsZero() {
		t.Fatal(err)
	}
	run.wait(t)
	return w, run, out
}

// 1. Pairing with the right expectation (words, prefixes, 52 characters)
// pairs and stores hub_check fingerprint; a wrong one is refused with box M4
// and nothing is stored.
func TestPairWithTheExpectedHubFingerprint(t *testing.T) {
	h, ps := newPairingConnHub(t)
	fp := hubFP(t, h)
	for name, in := range map[string]string{"words": fp.Words(), "prefixes": prefixes(fp.Words()), "full": string(fp)} {
		w, run, _ := pairAt(t, h, ps, matcher(t, in))
		if run.err != nil {
			t.Fatalf("%s: %v", name, run.err)
		}
		enr, ok, _ := w.enr.Load(context.Background())
		if !ok || enr.Check() != wstore.HubCheckFingerprint || enr.HubCheckAnchor != string(fp) {
			t.Fatalf("%s: enrollment %+v", name, enr)
		}
	}

	other, _ := identity.GenerateKey()
	w, run, out := pairAt(t, h, ps, matcher(t, other.Fingerprint().Words()))
	if !errors.Is(run.err, wagent.ErrHubFingerprintMismatch) {
		t.Fatalf("wrong expectation: %v", run.err)
	}
	if _, ok, _ := w.enr.Load(context.Background()); ok {
		t.Fatal("a refused pairing stored an enrollment")
	}
	if !strings.Contains(out.String(), "HUB FINGERPRINT DOES NOT MATCH - NOT PAIRED") || !strings.Contains(out.String(), fp.Words()) {
		t.Fatalf("box M4:\n%s", out.String())
	}
}

// 2. A malicious middle: a second Hub identity answers and knows the code.
// With an expectation the pairing fails; without one the Worker pairs with the
// middle and says "not verified"; verify-hub with the real words reports the
// mismatch, the connector stays idle, also after a restart; --forget --pinned
// removes the pairing.
func TestMaliciousMiddleIsRefusedOrDetected(t *testing.T) {
	real := newConnHub(t)
	middle, ps := newPairingConnHub(t)
	realFP, middleFP := hubFP(t, real), hubFP(t, middle)

	_, run, _ := pairAt(t, middle, ps, matcher(t, realFP.Words()))
	if !errors.Is(run.err, wagent.ErrHubFingerprintMismatch) {
		t.Fatalf("with an expectation: %v", run.err)
	}

	w, run, _ := pairAt(t, middle, ps, identity.FingerprintMatcher{})
	if run.err != nil {
		t.Fatalf("without an expectation: %v", run.err)
	}
	ctx := context.Background()
	if enr, _, _ := w.enr.Load(ctx); enr.Check() != wstore.HubCheckNone {
		t.Fatalf("enrollment %+v", enr)
	}
	rep := wagent.BuildStatus(ctx, wagent.StatusInputs{HubURL: middle.url(), Enrollment: w.enr})
	if !strings.HasPrefix(rep.HubCheckText, "not verified") {
		t.Fatalf("status says %q", rep.HubCheckText)
	}

	res, err := wagent.VerifyHub(ctx, w.enr, matcher(t, realFP.Words()))
	if err != nil || res.Outcome != wagent.VerifyMismatch || res.Paired != middleFP {
		t.Fatalf("verify-hub: %+v %v", res, err)
	}
	conn, _, stop := w.connect(middle, proto.Range{})
	eventually(t, "the quarantined Worker to stay idle", func() bool {
		st := conn.Status()
		return st.State == wagent.ConnIdle && strings.Contains(st.Reason, "does not match the fingerprint you gave")
	})
	stop()
	w.restart()
	conn, _, stop = w.connect(middle, proto.Range{})
	eventually(t, "the quarantine to survive a restart", func() bool {
		return conn.Status().State == wagent.ConnIdle && strings.Contains(conn.Status().Reason, "does not match")
	})
	stop()
	if err := wagent.ForgetHub(ctx, w.enr, realFP); !errors.Is(err, wagent.ErrPinnedDiffers) {
		t.Fatalf("forget with the wrong key: %v", err)
	}
	if err := wagent.ForgetHub(ctx, w.enr, middleFP); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := w.enr.Load(ctx); ok {
		t.Fatal("--forget left the pairing")
	}
}

// 3. The Hub identity changes after pairing: box M5 once, no connection, the
// pin is unchanged, and the Worker reconnects when the first Hub is back.
func TestHubIdentitySwapAfterPairing(t *testing.T) {
	h := newConnHub(t)
	w := newPersistentWorker(t, h)
	row := h.enrol(w.key.Public(), "pi")
	pin := h.pin()
	impostor := newConnHub(t)
	impostor.enrol(w.key.Public(), "pi")

	h.mu.Lock()
	addr := h.addr
	h.mu.Unlock()
	h.stop()
	impostor.stop()
	impostor.start(addr)

	var out syncLog
	conn, err := wagent.NewConnector(wagent.ConnectorConfig{
		HubURL: h.url(), Name: "pi", Ops: func() []string { return []string{"sessions_list"} },
		Key: w.enr.LoadKey, Enrollment: w.enr, Policy: wagent.NewPolicy(w.st), Dialer: wagent.NewHubDialer(),
		Backoff: fastBackoff(), IdlePoll: 20 * time.Millisecond, Out: &out,
		Logger: slog.New(slog.NewTextHandler(w.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); conn.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })

	eventually(t, "the identity alarm", func() bool { return conn.Status().Alarm == wagent.AlarmHubIdentityChanged })
	eventually(t, "a few more refused attempts", func() bool {
		return strings.Count(w.logs.String(), "cannot connect to the hub") >= 1 && conn.Status().Attempts >= 3
	})
	if n := strings.Count(out.String(), "HUB IDENTITY CHANGED - NOT CONNECTING"); n != 1 {
		t.Fatalf("box M5 shown %d times:\n%s", n, out.String())
	}
	if enr, _, _ := w.enr.Load(context.Background()); !bytes.Equal(enr.Pin.Pub, pin.Pub) {
		t.Fatal("the pin changed")
	}

	impostor.stop()
	h.start(addr)
	eventually(t, "the Worker to reconnect to its Hub", func() bool { return h.service().Registry().IsOnline(row.ID) })
	eventually(t, "the alarm to clear", func() bool { return conn.Status().Alarm == "" })
}

// 4. Rotation: a Worker checked with the fingerprint follows rotate-master and
// keeps the check; a start with the old setting warns and connects; a start
// with the new setting is silent and moves the anchor.
func TestCheckedWorkerFollowsARotation(t *testing.T) {
	h := newConnHub(t)
	w := newPersistentWorker(t, h)
	row := h.enrol(w.key.Public(), "pi")
	ctx := context.Background()
	oldFP := hubFP(t, h)
	log := slog.New(slog.NewTextHandler(w.logs, nil))
	if err := wagent.CheckEnrolledHub(ctx, w.enr, matcher(t, oldFP.Words()), io.Discard, log); err != nil {
		t.Fatal(err)
	}

	actor := hubid.Actor{ID: "admin-1", Channel: hubid.ChannelUI}
	const pass2 = "second passphrase for the Hub"
	if _, err := h.id.RotateMaster(ctx, actor, hubidtest.Passphrase, pass2, pass2, hubid.IssueOptions{}); err != nil {
		t.Fatal(err)
	}
	newFP := hubFP(t, h)
	w.start()
	eventually(t, "the Worker to follow the rotation", func() bool {
		enr, ok, _ := w.enr.Load(ctx)
		return ok && enr.Pin.Seq == 1 && h.service().Registry().IsOnline(row.ID)
	})
	w.stop()
	enr, _, _ := w.enr.Load(ctx)
	if enr.Check() != wstore.HubCheckFingerprint || enr.HubCheckAnchor != string(oldFP) {
		t.Fatalf("after the rotation: %+v", enr)
	}

	var box bytes.Buffer
	if err := wagent.CheckEnrolledHub(ctx, w.enr, matcher(t, string(oldFP)), &box, log); err != nil {
		t.Fatal(err)
	}
	if enr, _, _ = w.enr.Load(ctx); enr.Check() != wstore.HubCheckFingerprint || box.Len() != 0 ||
		!strings.Contains(w.logs.String(), "Update the setting to "+string(newFP)) {
		t.Fatalf("old setting: %+v\n%s", enr, w.logs.String())
	}
	w.start()
	eventually(t, "the Worker to connect with the old setting", func() bool { return h.service().Registry().IsOnline(row.ID) })
	w.stop()

	if err := wagent.CheckEnrolledHub(ctx, w.enr, matcher(t, newFP.Words()), &box, log); err != nil {
		t.Fatal(err)
	}
	if enr, _, _ = w.enr.Load(ctx); enr.Check() != wstore.HubCheckFingerprint || enr.HubCheckAnchor != string(newFP) || box.Len() != 0 {
		t.Fatalf("new setting: %+v", enr)
	}
}
