package workers

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/audit"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// pairAndLoseTheWelcome pairs key with code and closes the Worker's channel
// before it reads welcome: the Hub has committed, the Worker did not record it.
func pairAndLoseTheWelcome(t *testing.T, h *pairHub, key *identity.Key, code identity.PairingCode, owner string) store.WorkerRow {
	t.Helper()
	w := h.start(t, key, code, "pi")
	if _, err := h.claim(owner, code.String(), ""); err != nil {
		t.Fatal(err)
	}
	w.wait(t)
	if w.err != nil {
		t.Fatal(w.err)
	}
	_ = w.res.Channel.Close() // the welcome is lost
	row, err := h.st.WorkerByPub(context.Background(), key.Public())
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "the lost connection to go offline", func() bool { return !h.svc.Registry().IsOnline(row.ID) })
	return row
}

func countAudit(t *testing.T, h *pairHub, action string) int {
	t.Helper()
	page, err := h.audit.List(context.Background(), audit.Filter{Action: action})
	if err != nil {
		t.Fatal(err)
	}
	return len(page.Events)
}

// (1) Lost welcome: the same Worker with the same code and a fresh CPace, the
// owner types the code again: the existing enrollment is returned.
func TestResumeAfterALostWelcomeReturnsTheExistingEnrollment(t *testing.T) {
	h := newPairHub(t, nil, nil)
	key := newKey(t)
	code := identity.NewPairingCode()
	row := pairAndLoseTheWelcome(t, h, key, code, "u-alice")

	w := h.start(t, key, code, "pi")
	res, err := h.claim("u-alice", code.String(), "")
	if err != nil {
		t.Fatalf("resume claim: %v", err)
	}
	w.wait(t)
	if w.err != nil {
		t.Fatalf("the resuming Worker failed: %v", w.err)
	}
	welcome := decodeWelcome(t, w.res)
	if welcome.WorkerID != row.ID || welcome.Name != "pi" || !welcome.Pending || res.WorkerID != row.ID || res.Name != "pi" || res.Renamed {
		t.Errorf("welcome = %+v claim = %+v, want the existing row %s", welcome, res, row.ID)
	}
	list, err := h.st.ListWorkers(context.Background())
	if err != nil || len(list) != 1 {
		t.Fatalf("workers = %d err=%v, want one and no new suffix", len(list), err)
	}
	if n := countAudit(t, h, AuditPairResume); n != 1 {
		t.Errorf("%d worker.pair_resume events, want 1", n)
	}
	if n := countAudit(t, h, "worker.pair"); n != 1 {
		t.Errorf("%d worker.pair events, want only the first pairing", n)
	}
}

// (4) A different Worker key with the same code gets today's refusal; (5) the
// same key with a different code and no repair window gets already_paired.
func TestResumeIsBoundToTheWorkerKeyAndTheCode(t *testing.T) {
	h := newPairHub(t, nil, nil)
	key := newKey(t)
	code := identity.NewPairingCode()
	pairAndLoseTheWelcome(t, h, key, code, "u-alice")

	other := h.startWorker(t, newKey(t), code, "evil")
	other.wait(t)
	if proto.CodeOf(other.err) != proto.CodePairingIDCollision {
		t.Fatalf("another key saw %v, want pairing_id_collision", other.err)
	}

	fresh := identity.NewPairingCode()
	w := h.start(t, key, fresh, "pi")
	_, err := h.claim("u-alice", fresh.String(), "")
	if proto.CodeOf(err) != proto.CodeAlreadyPaired {
		t.Fatalf("claim = %v, want already_paired", err)
	}
	w.wait(t)
	// The refusal arrives inside the channel, after the handshake.
	m := readControl(t, w.res)
	if frameType(t, m) != proto.TypeError {
		t.Fatalf("the Worker saw %v, want an already_paired error frame", m)
	}
}

// (6) A different user typing the code during a resume is refused and the
// resume stays possible for the owner.
func TestResumeRefusesAnotherUserButKeepsTheOwnersChance(t *testing.T) {
	h := newPairHub(t, nil, nil)
	key := newKey(t)
	code := identity.NewPairingCode()
	row := pairAndLoseTheWelcome(t, h, key, code, "u-alice")

	w := h.start(t, key, code, "pi")
	_, err := h.claim("u-mallory", code.String(), "")
	_ = wantPairingCode(t, err, proto.CodePairingCodeInvalid)
	if !h.ps.IsPending(code.PairingID()) {
		t.Fatal("the refused claim ended the resume")
	}
	if _, err := h.claim("u-alice", code.String(), ""); err != nil {
		t.Fatal(err)
	}
	w.wait(t)
	if w.err != nil || decodeWelcome(t, w.res).WorkerID != row.ID {
		t.Fatalf("owner resume failed: %v", w.err)
	}
}

// (7) Resume after the code expired is refused.
func TestResumeAfterTheCodeExpiredIsRefused(t *testing.T) {
	h := newPairHub(t, nil, nil)
	key := newKey(t)
	code := identity.NewPairingCode()
	pairAndLoseTheWelcome(t, h, key, code, "u-alice")
	h.clk.Advance(PairingTTL + time.Second)

	w := h.start(t, key, code, "pi")
	_, err := h.claim("u-alice", code.String(), "")
	_ = wantPairingCode(t, err, proto.CodePairingCodeInvalid)
	h.ps.Close()
	w.wait(t)
}

// (8) Five wrong codes during a resume close it.
func TestFiveWrongCodesDuringAResumeCloseIt(t *testing.T) {
	h := newPairHub(t, nil, nil)
	key := newKey(t)
	code := identity.NewPairingCode()
	pairAndLoseTheWelcome(t, h, key, code, "u-alice")

	w := h.start(t, key, code, "pi")
	for i := 1; i <= store.MaxResumeAttempts; i++ {
		_, err := h.claim("u-alice", wrongTail(code), "")
		if i < store.MaxResumeAttempts {
			pe := wantPairingCode(t, err, proto.CodePairingCodeInvalid)
			if left := attemptsLeft(t, pe); left != store.MaxResumeAttempts-i {
				t.Fatalf("attempt %d: attempts_left = %d", i, left)
			}
		} else {
			_ = wantPairingCode(t, err, proto.CodePairingAttemptsExceeded)
		}
	}
	w.wait(t)
	// The resume is closed: the same Worker with the right code is refused.
	again := h.startWorker(t, key, code, "pi")
	again.wait(t)
	if proto.CodeOf(again.err) != proto.CodePairingIDCollision {
		t.Fatalf("after five wrong codes the Worker saw %v", again.err)
	}
}

// (9) A removed Worker during a resume gets revoked.
func TestAWorkerRemovedDuringAResumeIsRevoked(t *testing.T) {
	h := newPairHub(t, nil, nil)
	key := newKey(t)
	code := identity.NewPairingCode()
	row := pairAndLoseTheWelcome(t, h, key, code, "u-alice")
	if err := h.st.TombstoneWorker(context.Background(), row.ID, "admin", time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	w := h.start(t, key, code, "pi")
	_, err := h.claim("u-alice", code.String(), "")
	if proto.CodeOf(err) != proto.CodeRevoked {
		t.Fatalf("claim = %v, want revoked", err)
	}
	w.wait(t)
}

// Rows from before the migration have no pairing data and never resume.
func TestACommitRowWithoutPairingDataNeverResumes(t *testing.T) {
	st := openStore(t, t.TempDir())
	if _, err := st.DB().Exec(`INSERT INTO used_pairing_codes (code_digest, used_at) VALUES ('d', 1)`); err != nil {
		t.Fatal(err)
	}
	_, err := st.PairingCommitFor(context.Background(), "K7Q4", make([]byte, 32), 0)
	if !errors.Is(err, store.ErrNoPairingCommit) {
		t.Fatalf("err = %v", err)
	}
}

// ------------------------------------------------------------ owner re-issue

func enrolUnconnected(t *testing.T, h *pairHub, key *identity.Key, owner string) store.WorkerRow {
	t.Helper()
	return h.enrol(key, owner, "stuck") // never connected: first_connected_at is NULL
}

func reissue(h *pairHub, id, user string, admin bool) error {
	return ReissuePairing(context.Background(), h.st, h.audit, h.clk.Now(), ReissueRequest{WorkerID: id, UserID: user, Admin: admin})
}

func TestOwnerReissueRepairsAStuckWorkerWithItsIdNameAndApproval(t *testing.T) {
	h := newPairHub(t, nil, nil)
	key := newKey(t)
	row := enrolUnconnected(t, h, key, "u-alice")

	code := identity.NewPairingCode()
	// Without a window the new code gets already_paired.
	w := h.start(t, key, code, "stuck")
	_, err := h.claim("u-alice", code.String(), "")
	if proto.CodeOf(err) != proto.CodeAlreadyPaired {
		t.Fatalf("no window: %v", err)
	}
	w.wait(t)

	if err := reissue(h, row.ID, "u-alice", false); err != nil {
		t.Fatal(err)
	}
	code2 := identity.NewPairingCode()
	w = h.start(t, key, code2, "stuck")
	res, err := h.claim("u-alice", code2.String(), "")
	if err != nil {
		t.Fatalf("repair claim: %v", err)
	}
	w.wait(t)
	welcome := decodeWelcome(t, w.res)
	if welcome.WorkerID != row.ID || res.WorkerID != row.ID || welcome.Name != "stuck" {
		t.Errorf("welcome = %+v", welcome)
	}
	cur, _ := h.st.WorkerByID(context.Background(), row.ID)
	if cur.RepairUntil != 0 {
		t.Error("the repair window was not cleared")
	}
	if n := countAudit(t, h, AuditPairingReissue); n != 1 {
		t.Errorf("%d pairing_reissue events", n)
	}
	// The window is single use.
	code3 := identity.NewPairingCode()
	w3 := h.start(t, key, code3, "stuck")
	if _, err := h.claim("u-alice", code3.String(), ""); proto.CodeOf(err) != proto.CodeAlreadyPaired {
		t.Fatalf("second repair: %v", err)
	}
	w3.wait(t)
}

func TestOwnerReissueEligibilityAndAuthorization(t *testing.T) {
	h := newPairHub(t, nil, nil)
	stuck := enrolUnconnected(t, h, newKey(t), "u-alice")

	if err := reissue(h, stuck.ID, "u-bob", false); proto.CodeOf(err) != proto.CodeNotFound {
		t.Errorf("another user: %v", err)
	}
	if err := reissue(h, "nope", "u-alice", false); proto.CodeOf(err) != proto.CodeNotFound {
		t.Errorf("unknown worker: %v", err)
	}
	if err := reissue(h, stuck.ID, "u-admin", true); err != nil {
		t.Errorf("an administrator: %v", err)
	}

	// A Worker that connected, or that was backfilled by the migration, is not eligible.
	connected := h.enrol(newKey(t), "u-alice", "connected")
	if _, err := h.st.MarkFirstConnected(context.Background(), connected.ID, 1); err != nil {
		t.Fatal(err)
	}
	if err := reissue(h, connected.ID, "u-alice", false); proto.CodeOf(err) != proto.CodeConflict {
		t.Errorf("connected worker: %v", err)
	}
	// A removed Worker has no live row.
	gone := h.enrol(newKey(t), "u-alice", "gone")
	if err := h.st.TombstoneWorker(context.Background(), gone.ID, "admin", 1); err != nil {
		t.Fatal(err)
	}
	if err := reissue(h, gone.ID, "u-alice", false); proto.CodeOf(err) != proto.CodeNotFound {
		t.Errorf("removed worker: %v", err)
	}
}

func TestTheRepairWindowCannotBeUsedByAnotherKeyOrAfterItExpired(t *testing.T) {
	h := newPairHub(t, nil, nil)
	key := newKey(t)
	row := enrolUnconnected(t, h, key, "u-alice")
	if err := reissue(h, row.ID, "u-alice", false); err != nil {
		t.Fatal(err)
	}
	// Another key does not match the Worker row: it is simply a new pairing.
	code := identity.NewPairingCode()
	w := h.start(t, newKey(t), code, "other")
	res, err := h.claim("u-alice", code.String(), "")
	if err != nil || res.WorkerID == row.ID {
		t.Fatalf("another key = %+v %v", res, err)
	}
	w.wait(t)
	// After the window the right key gets already_paired again.
	h.clk.Advance(RepairWindow + time.Second)
	code2 := identity.NewPairingCode()
	w2 := h.start(t, key, code2, "stuck")
	if _, err := h.claim("u-alice", code2.String(), ""); proto.CodeOf(err) != proto.CodeAlreadyPaired {
		t.Fatalf("after the window: %v", err)
	}
	w2.wait(t)
}

// Each eligibility check and the write it guards are one conditional update, so
// a first connect racing the window wins or loses cleanly.
func TestRepairWindowAndFirstConnectAreSerialisedByConditionalUpdates(t *testing.T) {
	ctx := context.Background()
	for i := 0; i < 20; i++ {
		st := openStore(t, t.TempDir())
		w := store.WorkerRow{ID: store.NewID(), Pub: newKey(t).Public(), Name: "w", OwnerID: "o", Kind: store.WorkerKindPrivate, CreatedAt: 1}
		if err := st.InsertWorker(ctx, nil, w); err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		var setErr error
		var first bool
		wg.Add(2)
		go func() { defer wg.Done(); setErr = st.SetRepairWindow(ctx, w.ID, 1<<40) }()
		go func() { defer wg.Done(); first, _ = st.MarkFirstConnected(ctx, w.ID, 5) }()
		wg.Wait()
		cur, _ := st.WorkerByID(ctx, w.ID)
		if cur.FirstConnectedAt == 0 {
			t.Fatal("the first connect was lost")
		}
		if cur.RepairUntil != 0 {
			t.Fatalf("round %d: window open on a connected Worker (setErr=%v first=%v)", i, setErr, first)
		}
		// Whatever order won, the repair path refuses.
		err := st.RepairPairing(ctx, "ABCD", "digest", cur, 1)
		if !errors.Is(err, store.ErrRepairNotAllowed) {
			t.Fatalf("repair after a connect = %v", err)
		}
	}
}

// A different user typing wrong codes during a resume is a plain refusal: the
// owner is checked before the digest and nothing is charged, so five wrong
// codes of a stranger do not close the owner's resume.
func TestAnotherUsersWrongCodesDoNotChargeTheOwnersResume(t *testing.T) {
	h := newPairHub(t, nil, nil)
	key := newKey(t)
	code := identity.NewPairingCode()
	row := pairAndLoseTheWelcome(t, h, key, code, "u-alice")

	w := h.start(t, key, code, "pi")
	for i := 0; i < store.MaxResumeAttempts+1; i++ {
		_, err := h.claim("u-mallory", wrongTail(code), "")
		_ = wantPairingCode(t, err, proto.CodePairingCodeInvalid)
		if !h.ps.IsPending(code.PairingID()) {
			t.Fatalf("wrong code %d of another user ended the resume", i+1)
		}
	}
	if _, err := h.claim("u-alice", code.String(), ""); err != nil {
		t.Fatalf("the owner's resume after the stranger's attempts: %v", err)
	}
	w.wait(t)
	if w.err != nil || decodeWelcome(t, w.res).WorkerID != row.ID {
		t.Fatalf("owner resume failed: %v", w.err)
	}
}
