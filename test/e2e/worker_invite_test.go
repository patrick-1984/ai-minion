//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/workers"
	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/transport"
	wagent "github.com/patrick-1984/ai-minion/internal/worker/agent"
	wstore "github.com/patrick-1984/ai-minion/internal/worker/store"
)

// Story 4.6 integration tests: the real Hub (Stories 4.3 and 4.5, over a TLS
// listener with a self-signed certificate for another host name) and the real
// Worker enrollment with the production dialer. The Worker has no TLS option,
// so every enrollment below proves that trust is the invitation fingerprint
// and never the certificate (A-15). Run with "make e2e".

// inviteHub is the real Hub with the invitation service and the enroll_invite
// handler registered, and one user who creates keys.
type inviteHub struct {
	*connHub
	inv *workers.Invitations
}

func newInviteHub(t *testing.T) *inviteHub {
	t.Helper()
	h := &inviteHub{connHub: newConnHub(t)}
	fpr := h.service().Identity().Fingerprint()
	inv, err := workers.NewInvitations(workers.InvitationsConfig{Store: h.st, HubFingerprint: fpr})
	if err != nil {
		t.Fatal(err)
	}
	enr, err := workers.NewEnroller(workers.EnrollConfig{Store: h.st, HubFingerprint: fpr})
	if err != nil {
		t.Fatal(err)
	}
	if err := workers.RegisterEnroller(h.service().Handler(), enr); err != nil {
		t.Fatal(err)
	}
	if _, err := h.st.DB().Exec(
		`INSERT INTO users (id, username, active, created_at, updated_at) VALUES ('alice', 'alice', 1, 1, 1)`); err != nil {
		t.Fatal(err)
	}
	h.inv = inv
	return h
}

func (h *inviteHub) newKey(t *testing.T, mode string) identity.InviteKey {
	t.Helper()
	res, err := h.inv.Create(context.Background(), workers.InvitationCaller{UserID: "alice", Actor: "alice"},
		ops.InvitationCreateArgs{Mode: mode, TTL: "1h", Name: "lab"})
	if err != nil {
		t.Fatal(err)
	}
	k, err := identity.ParseInviteKey(res.Key)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// inviteWorker is a Worker state with the production invitation enroller.
type inviteWorker struct {
	st       *wstore.Store
	enr      *wagent.Enrollments
	enroller *wagent.InviteEnroller
}

func newInviteWorker(t *testing.T, hubURL string, dialer wagent.InviteDialer) *inviteWorker {
	t.Helper()
	dir := t.TempDir()
	st, err := wstore.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	enr := wagent.NewEnrollments(st, dir)
	e, err := wagent.NewInviteEnroller(wagent.InviteEnrollerConfig{
		State: enr, Dialer: dialer, HubURL: hubURL, Name: "pi", Attempts: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	return &inviteWorker{st: st, enr: enr, enroller: e}
}

func TestWorkerEnrollsWithAnInvitationKeyAtTheRealHubAndThenConnectsWithNoTLSFlags(t *testing.T) {
	h := newInviteHub(t)
	key := h.newKey(t, ops.InvitationReusable)
	w := newInviteWorker(t, h.url(), nil)

	res, err := w.enroller.Enroll(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	if res.AlreadyEnrolled || res.Enrollment.Name != "pi" || res.Enrollment.Owner != "alice" || res.Enrollment.Kind != "private" {
		t.Fatalf("result = %+v", res)
	}
	stored, ok, err := w.enr.Load(context.Background())
	if err != nil || !ok || stored.WorkerID != res.Enrollment.WorkerID {
		t.Fatalf("stored = %+v, %v, %v", stored, ok, err)
	}
	if !bytes.Equal(stored.Pin.Pub, h.service().Identity().Public()) {
		t.Fatal("the stored pin is not the Hub key")
	}
	ek, err := w.enr.LoadKey()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.st.WorkerByPub(context.Background(), ek.Public()); err != nil {
		t.Fatalf("the Hub does not know the Worker's saved key pair: %v", err)
	}

	// The connector the Worker runs next connects in connect mode with the new
	// pin and the same key pair, with no TLS flag either.
	st, err := h.st.WorkerByPub(context.Background(), ek.Public())
	if err != nil {
		t.Fatal(err)
	}
	conn, err := wagent.NewConnector(wagent.ConnectorConfig{
		HubURL: h.url(), Name: "pi", Ops: func() []string { return []string{"sessions_list"} },
		Key: w.enr.LoadKey, Enrollment: w.enr, Policy: wagent.NewPolicy(w.st), Dialer: wagent.NewHubDialer(),
		Backoff:  &transport.Backoff{Initial: 5 * time.Millisecond, Max: 40 * time.Millisecond},
		IdlePoll: 20 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); conn.Run(ctx) }()
	defer func() { cancel(); <-done }()
	eventually(t, "the enrolled Worker to be online at the Hub", func() bool { return h.service().Registry().IsOnline(st.ID) })

	// A second run with the same key is ignored: nothing is sent.
	again, err := w.enroller.Enroll(context.Background(), key)
	if err != nil || !again.AlreadyEnrolled {
		t.Fatalf("second run = %+v, %v", again, err)
	}
}

func TestEnrollmentAbortsAtAHubWhoseIdentityIsNotTheKeysAndSendsNoSecret(t *testing.T) {
	h := newInviteHub(t)
	other, err := identity.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	wrong, err := identity.NewInviteKey(other.Fingerprint())
	if err != nil {
		t.Fatal(err)
	}
	w := newInviteWorker(t, h.url(), nil)

	_, err = w.enroller.Enroll(context.Background(), wrong)
	var fm *wagent.FingerprintMismatchError
	if !errors.As(err, &fm) {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(err.Error(), string(other.Fingerprint())) ||
		!strings.Contains(err.Error(), string(h.service().Identity().Fingerprint())) {
		t.Fatalf("the error does not name both fingerprints: %v", err)
	}
	if _, ok, _ := w.enr.Load(context.Background()); ok {
		t.Fatal("an enrollment was stored")
	}
	if strings.Contains(err.Error(), wrong.String()) {
		t.Fatal("the error carries the key")
	}
}

func TestEnrollmentRetryAfterALostAnswerGetsTheSameWorkerWithAOneTimeKey(t *testing.T) {
	h := newInviteHub(t)
	key := h.newKey(t, ops.InvitationOneTime)
	lossy := &lossyDialer{inner: wagent.NewInviteDialer(), loseFirst: true}
	w := newInviteWorker(t, h.url(), lossy)

	if _, err := w.enroller.Enroll(context.Background(), key); err == nil {
		t.Fatal("the lost answer was not an error")
	}
	if _, ok, _ := w.enr.Load(context.Background()); ok {
		t.Fatal("enrolled without the Hub's answer")
	}
	first, err := w.enr.LoadKey()
	if err != nil {
		t.Fatal("the key pair must be saved before the first connection: ", err)
	}

	res, err := w.enroller.Enroll(context.Background(), key)
	if err != nil {
		t.Fatalf("the retry with the same one-time key and key pair failed: %v", err)
	}
	if res.Enrollment.WorkerID != lossy.lostWorkerID || lossy.lostWorkerID == "" {
		t.Fatalf("retry got worker %q, the lost answer was for %q", res.Enrollment.WorkerID, lossy.lostWorkerID)
	}
	second, _ := w.enr.LoadKey()
	if string(first.Public()) != string(second.Public()) {
		t.Fatal("the retry used another key pair")
	}

	// The one-time key is used up for any other Worker.
	other := newInviteWorker(t, h.url(), nil)
	_, err = other.enroller.Enroll(context.Background(), key)
	var pe *proto.Error
	if !errors.As(err, &pe) || pe.Code != proto.CodeInviteExhausted {
		t.Fatalf("a second Worker got %v", err)
	}
}

// lossyDialer runs a real enrollment and then reports a lost connection,
// once: the Hub committed, the Worker never stored the result.
type lossyDialer struct {
	inner        wagent.InviteDialer
	loseFirst    bool
	lostWorkerID string
}

func (d *lossyDialer) Enroll(ctx context.Context, p wagent.InviteParams) (*wagent.InviteOutcome, error) {
	out, err := d.inner.Enroll(ctx, p)
	if err == nil && d.loseFirst {
		d.loseFirst = false
		d.lostWorkerID = out.Welcome.WorkerID
		return nil, errors.New("connection lost before the Worker stored the result")
	}
	return out, err
}
