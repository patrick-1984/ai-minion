package agent

import (
	"bytes"
	"context"
	"io"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/worker/store"
)

// RC2 release-gate reproducers (Worker side).

// --- W5: an expired welcome must not leave its restriction applied ---------

// On a reconnect with the same certificate neither the pin nor the ratchet
// moves, so no ratchet commit runs before the welcome. The deadline passes
// while the welcome write waits for the state lock: nothing is stored and the
// in-memory policy is unchanged.
func TestAnExpiredWelcomeWithoutRatchetChangeLeavesThePolicyUnchanged(t *testing.T) {
	wm := newFakeWM()
	exp := wm.expiryAfter(time.Hour)
	f := newConnFixture(t, func(cfg *ConnectorConfig) { cfg.WallMono = wm })
	f.dial.script(func(p ConnectParams) (*Attempt, error) {
		release := holdStateLock(t, f.dir)
		go func() {
			time.Sleep(150 * time.Millisecond)
			wm.set(time.UnixMilli(exp.WallMs).UTC(), wm.Mono())
			release()
		}()
		e := exp
		w := okWelcome()
		w.Policy.PreviewOnly = true
		return &Attempt{
			Pin: p.Pin, OpPub: p.Pin.Pub, TranscriptHash: randomHash(), Version: proto.Current, Expiry: &e,
			Ratchet:        p.Ratchet,
			RatchetChanged: false, PinChanged: false,
			Reply: Reply{Type: proto.TypeWelcome, Welcome: w}, Session: newFakeSession(),
		}, nil
	})
	f.run()
	eventually(t, func() bool { return f.conn.Status().LastError != "" })
	got := f.storedEnrollment()
	if got.WorkerID != "" || got.Policy.PreviewOnly {
		t.Fatalf("an expired welcome was committed: %+v", got)
	}
	if f.policy.HubPreviewOnly() {
		t.Fatal("an expired welcome left its restriction applied in memory")
	}
	if f.conn.Status().State == ConnConnected {
		t.Fatal("an expired welcome was served")
	}
}

// --- W1 regression: exposure cleanup must not erase a successor's record ---

// An old pairer runs with a restored record. Its attempt exposes the code and
// fails; before the cleanup runs, `minion worker pair --new-code` replaces the
// identity and a successor pairing saves its own record. The old pairer's
// cleanup must leave the successor's record alone.
func TestExposureCleanupOfAnOldPairerKeepsTheSuccessorsResumeRecord(t *testing.T) {
	f := newPairFixture(t)
	f.codeSeq("B2C3D4E5", "C3D4E5F6")
	ctx := context.Background()
	rec := store.PairResume{Code: "MN-K7Q4-XY12", PairingID: "K7Q4", ExpiresAt: f.clk.Now().Add(5 * time.Minute),
		RootPub: bytes.Repeat([]byte{7}, 32), Serial: 4, CertSHA256: bytes.Repeat([]byte{2}, 32)}
	if err := f.enr.repo.SavePairResume(ctx, rec); err != nil {
		t.Fatal(err)
	}
	succ := store.PairResume{Code: "MN-P9R8-ZZ34", PairingID: "P9R8", ExpiresAt: f.clk.Now().Add(5 * time.Minute),
		RootPub: bytes.Repeat([]byte{8}, 32), Serial: 6, CertSHA256: bytes.Repeat([]byte{3}, 32)}
	var raceErr error
	f.dial.steps = []pairStep{func(_ context.Context, p PairParams) (PairResult, error) {
		p.OnWaiting()
		p.OnExposed()
		// The old attempt has failed after exposing the code; the replacement
		// and the successor's record land before its cleanup runs.
		if _, raceErr = RenewWorkerIdentity(ctx, f.enr); raceErr == nil {
			gen, _ := f.enr.Watch()
			raceErr = f.enr.SavePairResumeIf(ctx, gen, succ)
		}
		return PairResult{}, io.ErrUnexpectedEOF
	}}
	r, _ := f.run(f.pairer())
	r.wait(t)
	if raceErr != nil {
		t.Fatal(raceErr)
	}
	got, ok := recordOf(t, f.enr)
	if !ok || got.PairingID != succ.PairingID || got.Code != succ.Code {
		t.Fatalf("the successor's resume record was erased by the old pairer's cleanup (present %v, %+v)", ok, got)
	}
}
