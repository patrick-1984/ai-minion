package workers

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/audit"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/ratelimit"
	"github.com/patrick-1984/ai-minion/internal/transport"
)

// testClock is a clock that tests move.
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// enrollHub is a testHub with the invitation service and the enroll_invite
// handler registered.
type enrollHub struct {
	*testHub
	inv   *Invitations
	enr   *Enroller
	clock *testClock
}

func newEnrollHub(t *testing.T, mutate func(*EnrollConfig)) *enrollHub {
	t.Helper()
	h := newTestHub(t, nil)
	clock := &testClock{t: time.Now().Truncate(time.Millisecond)}
	fpr := h.svc.Identity().Fingerprint()
	inv, err := NewInvitations(InvitationsConfig{Store: h.st, HubFingerprint: fpr, Auditor: h.audit, Now: clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	cfg := EnrollConfig{Store: h.st, HubFingerprint: fpr, Auditor: h.audit, Now: clock.Now, Logger: textLogger(h.logs)}
	if mutate != nil {
		mutate(&cfg)
	}
	enr, err := NewEnroller(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := RegisterEnroller(h.svc.Handler(), enr); err != nil {
		t.Fatal(err)
	}
	return &enrollHub{testHub: h, inv: inv, enr: enr, clock: clock}
}

// create makes a key for owner through the service.
func (h *enrollHub) create(owner string, admin bool, a ops.InvitationCreateArgs) ops.InvitationCreateResult {
	h.t.Helper()
	ensureUser(h.t, h.st, owner)
	res, err := h.inv.Create(context.Background(), InvitationCaller{UserID: owner, Actor: owner, Admin: admin}, a)
	if err != nil {
		h.t.Fatal(err)
	}
	return res
}

func secretOf(t *testing.T, key string) string {
	t.Helper()
	_, secret, ok := strings.Cut(key, ".")
	if !ok {
		t.Fatalf("malformed key %q", "mnk_...")
	}
	return secret
}

// answer is the Hub's reply to an enrollment attempt.
type answer struct {
	res  *transport.WorkerResult
	typ  string
	body []byte
}

func (a answer) welcome(t *testing.T) Welcome {
	t.Helper()
	var w Welcome
	if a.typ != proto.TypeWelcome || json.Unmarshal(a.body, &w) != nil {
		t.Fatalf("not a welcome: %s", a.body)
	}
	return w
}

func (a answer) code() proto.Code {
	var f proto.ErrorFrame
	if a.typ != proto.TypeError || json.Unmarshal(a.body, &f) != nil {
		return ""
	}
	return f.Code
}

// enroll runs the Worker side of an enroll_invite connection with frame as the
// invite.secret payload (nil sends the standard frame for secret).
func (h *enrollHub) enroll(key *identity.Key, name string, frame any) answer {
	h.t.Helper()
	cfg := transport.WorkerConfig{
		Key: key, Name: name, Mode: transport.ModeEnrollInvite, HubFingerprint: h.svc.Identity().Fingerprint(),
		Ops: []string{"send_text"}, Timeout: 10 * time.Second,
	}
	res, err := h.dial(cfg)
	if err != nil {
		h.t.Fatalf("handshake: %v", err)
	}
	h.t.Cleanup(func() { _ = res.Channel.Close() })
	if err := proto.WriteFrame(res.Channel, proto.StreamFrameLimit, frame); err != nil {
		h.t.Fatal(err)
	}
	_ = res.Channel.SetReadDeadline(time.Now().Add(10 * time.Second))
	body, err := proto.ReadFrame(res.Channel, proto.StreamFrameLimit)
	if err != nil {
		h.t.Fatalf("reading the answer: %v", err)
	}
	var head struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(body, &head); err != nil {
		h.t.Fatal(err)
	}
	return answer{res: res, typ: head.Type, body: body}
}

func secretFrame(secret string) InviteSecretFrame {
	return InviteSecretFrame{Type: proto.TypeInviteSecret, Secret: secret}
}

func (h *enrollHub) uses(id string) int {
	h.t.Helper()
	r, err := h.st.InvitationByID(context.Background(), id)
	if err != nil {
		h.t.Fatal(err)
	}
	return r.Uses
}

func TestEnrollCreatesTheWorkerForTheCreator(t *testing.T) {
	h := newEnrollHub(t, nil)
	h.svc.SetOwnerNames(func(_ context.Context, id string) string { return "name-of-" + id })
	inv := h.create("owner-1", false, ops.InvitationCreateArgs{Mode: ops.InvitationReusable, TTL: ops.TTL24h, Name: "lab"})
	key := newKey(t)

	a := h.enroll(key, "pi", secretFrame(secretOf(t, inv.Key)))
	w := a.welcome(t)
	if w.Name != "pi" || w.Owner != "name-of-owner-1" || w.Kind != "private" || w.WorkerID == "" {
		t.Fatalf("welcome = %+v", w)
	}
	row, err := h.st.WorkerByPub(context.Background(), key.Public())
	if err != nil || row.ID != w.WorkerID || row.OwnerID != "owner-1" || row.Kind != store.WorkerKindPrivate || row.Name != "pi" {
		t.Fatalf("stored worker = %+v err=%v", row, err)
	}
	if !row.Pending || w.Pending != true || row.EnrollAddr == "" {
		t.Errorf("an enrolled Worker must start pending, welcome=%v row=%+v", w.Pending, row)
	}
	if got := h.uses(inv.ID); got != 1 {
		t.Errorf("uses = %d, want 1", got)
	}
	// The Worker goes online right after welcome.
	sess, err := transport.NewSession(a.res.Channel, transport.RoleWorker, transport.SessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	eventually(t, "the enrolled Worker to be online", func() bool { return h.svc.Registry().IsOnline(w.WorkerID) })

	// Enrollment is audited without the secret.
	page, err := h.audit.List(context.Background(), audit.Filter{Action: AuditWorkerEnroll})
	if err != nil || len(page.Events) != 1 {
		t.Fatalf("audit = %+v err=%v", page, err)
	}
	if e := page.Events[0]; e.Outcome != "pending" || e.Target != w.WorkerID || !strings.HasPrefix(e.Actor, "worker:") {
		t.Errorf("audit event = %+v", e)
	}
}

func TestEnrollAcceptsTheWholeKeyInTheFrame(t *testing.T) {
	h := newEnrollHub(t, nil)
	inv := h.create("owner-1", false, ops.InvitationCreateArgs{Mode: ops.InvitationOneTime, TTL: ops.TTL1h})
	a := h.enroll(newKey(t), "pi", secretFrame(inv.Key))
	a.welcome(t)
}

func TestInfrastructureInvitationCreatesAnInfrastructureWorker(t *testing.T) {
	h := newEnrollHub(t, nil)
	inv := h.create("admin-1", true, ops.InvitationCreateArgs{Mode: ops.InvitationReusable, TTL: ops.TTLUnlimited, Kind: ops.KindInfrastructure})
	a := h.enroll(newKey(t), "node", secretFrame(secretOf(t, inv.Key)))
	if w := a.welcome(t); w.Kind != "infrastructure" {
		t.Errorf("welcome = %+v", w)
	}
	// A Worker enrolled with an expired-never key is unaffected by TTL.
	h.clock.Advance(10 * 365 * 24 * time.Hour)
	h.enroll(newKey(t), "node", secretFrame(secretOf(t, inv.Key))).welcome(t)
}

func TestOneTimeKeyEnrollsExactlyOneOfTwoRacingWorkers(t *testing.T) {
	h := newEnrollHub(t, nil)
	inv := h.create("owner-1", false, ops.InvitationCreateArgs{Mode: ops.InvitationOneTime, TTL: ops.TTL1h})
	secret := secretOf(t, inv.Key)
	keys := []*identity.Key{newKey(t), newKey(t)}

	var wg sync.WaitGroup
	answers := make([]answer, len(keys))
	start := make(chan struct{})
	for i := range keys {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			answers[i] = h.enroll(keys[i], "racer", secretFrame(secret))
		}()
	}
	close(start)
	wg.Wait()

	var enrolled, exhausted int
	for _, a := range answers {
		switch {
		case a.typ == proto.TypeWelcome:
			enrolled++
		case a.code() == proto.CodeInviteExhausted:
			exhausted++
		default:
			t.Errorf("unexpected answer %s", a.body)
		}
	}
	if enrolled != 1 || exhausted != 1 {
		t.Fatalf("enrolled=%d exhausted=%d, want 1 and 1", enrolled, exhausted)
	}
	if got := h.uses(inv.ID); got != 1 {
		t.Errorf("uses = %d, want 1", got)
	}
}

func TestEnrollingTheSameKeyAgainReturnsTheExistingWorker(t *testing.T) {
	h := newEnrollHub(t, nil)
	inv := h.create("owner-1", false, ops.InvitationCreateArgs{Mode: ops.InvitationOneTime, TTL: ops.TTL1h})
	secret := secretOf(t, inv.Key)
	key := newKey(t)

	first := h.enroll(key, "pi", secretFrame(secret)).welcome(t)
	// The retry after a lost reply: same key, one-time key already used up.
	again := h.enroll(key, "other-name", secretFrame(secret)).welcome(t)
	if again.WorkerID != first.WorkerID || again.Name != "pi" {
		t.Fatalf("second welcome = %+v, first = %+v", again, first)
	}
	if got := h.uses(inv.ID); got != 1 {
		t.Errorf("uses = %d, want 1 (no extra use)", got)
	}
	// Another Worker key is refused: the one use is gone.
	if c := h.enroll(newKey(t), "pi", secretFrame(secret)).code(); c != proto.CodeInviteExhausted {
		t.Errorf("another key got %q", c)
	}
	// The retry also works after the key was revoked: the Worker is enrolled.
	if err := h.inv.Revoke(context.Background(), InvitationCaller{UserID: "owner-1", Actor: "owner-1"}, inv.ID); err != nil {
		t.Fatal(err)
	}
	if again := h.enroll(key, "pi", secretFrame(secret)).welcome(t); again.WorkerID != first.WorkerID {
		t.Errorf("retry after revoke = %+v", again)
	}
	n, err := countWorkers(h.testHub)
	if err != nil || n != 1 {
		t.Errorf("workers = %d err=%v, want 1", n, err)
	}
}

func countWorkers(h *testHub) (int, error) {
	var n int
	err := h.st.DB().QueryRow(`SELECT COUNT(*) FROM workers`).Scan(&n)
	return n, err
}

func TestNameCollisionsGetASuffixWithinTheOwnersSpace(t *testing.T) {
	h := newEnrollHub(t, nil)
	mine := h.create("owner-1", false, ops.InvitationCreateArgs{Mode: ops.InvitationReusable, TTL: ops.TTL1h})
	other := h.create("owner-2", false, ops.InvitationCreateArgs{Mode: ops.InvitationReusable, TTL: ops.TTL1h})

	var names []string
	for range 3 {
		names = append(names, h.enroll(newKey(t), "pi", secretFrame(secretOf(t, mine.Key))).welcome(t).Name)
	}
	if want := []string{"pi", "pi-2", "pi-3"}; strings.Join(names, ",") != strings.Join(want, ",") {
		t.Errorf("names = %v, want %v", names, want)
	}
	// Names are unique per owner: another owner gets the plain name.
	if n := h.enroll(newKey(t), "pi", secretFrame(secretOf(t, other.Key))).welcome(t).Name; n != "pi" {
		t.Errorf("other owner's name = %q, want pi", n)
	}
}

func TestInvalidKeysAreRefusedWithTheirCodes(t *testing.T) {
	h := newEnrollHub(t, nil)
	owner := InvitationCaller{UserID: "owner-1", Actor: "owner-1"}
	revoked := h.create("owner-1", false, ops.InvitationCreateArgs{Mode: ops.InvitationReusable, TTL: ops.TTL1h})
	expiring := h.create("owner-1", false, ops.InvitationCreateArgs{Mode: ops.InvitationReusable, TTL: ops.TTL1h})
	if err := h.inv.Revoke(context.Background(), owner, revoked.ID); err != nil {
		t.Fatal(err)
	}
	foreign, err := identity.NewInviteKey(h.svc.Identity().Fingerprint())
	if err != nil {
		t.Fatal(err)
	}

	// An enrolled Worker, to prove that revocation and expiry leave it alone.
	enrolledKey := newKey(t)
	h.enroll(enrolledKey, "stays", secretFrame(secretOf(t, expiring.Key))).welcome(t)
	h.clock.Advance(time.Hour + time.Second)

	cases := []struct {
		name  string
		frame any
		want  proto.Code
	}{
		{"unknown secret", secretFrame(secretOf(t, foreign.String())), proto.CodeInviteInvalid},
		{"revoked", secretFrame(secretOf(t, revoked.Key)), proto.CodeInviteInvalid},
		{"expired", secretFrame(secretOf(t, expiring.Key)), proto.CodeInviteExpired},
		{"malformed secret", secretFrame("not-a-secret"), proto.CodeInviteInvalid},
		{"empty secret", secretFrame(""), proto.CodeInviteInvalid},
		{"key of another hub", secretFrame("mnk_" + strings.Repeat("a", 52) + "." + secretOf(t, revoked.Key)), proto.CodeInviteInvalid},
		{"wrong frame type", map[string]string{"type": "welcome"}, proto.CodeInvalidArgument},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a := h.enroll(newKey(t), "x", c.frame)
			if got := a.code(); got != c.want {
				t.Fatalf("code = %q (%s), want %q", got, a.body, c.want)
			}
			if bytes.Contains(a.body, []byte(secretOf(t, revoked.Key))) {
				t.Error("the refusal echoes the secret")
			}
		})
	}
	if n, _ := countWorkers(h.testHub); n != 1 {
		t.Errorf("workers = %d, want only the one enrolled before", n)
	}

	// The Worker enrolled before is untouched: it still connects after both
	// the revocation and the expiry.
	cfg := h.workerConfig(enrolledKey, "send_text")
	res, err := h.dial(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Channel.Close()
	if w := decodeWelcome(t, res); w.Name != "stays" {
		t.Errorf("welcome = %+v", w)
	}
	// Refusals are audited by code, never with the secret.
	page, err := h.audit.List(context.Background(), audit.Filter{Action: AuditWorkerEnroll, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	codes := map[string]int{}
	for _, e := range page.Events {
		codes[e.Outcome]++
		if strings.Contains(e.Target+e.Actor, "mnk_") {
			t.Errorf("audit event leaks a key: %+v", e)
		}
	}
	if codes["invite_invalid"] != 5 || codes["invalid_argument"] != 1 || codes["invite_expired"] != 1 || codes["pending"] != 1 {
		t.Errorf("audit outcomes = %v", codes)
	}
}

func TestRevokedKeyAuditNamesTheInvitation(t *testing.T) {
	h := newEnrollHub(t, nil)
	inv := h.create("owner-1", false, ops.InvitationCreateArgs{Mode: ops.InvitationReusable, TTL: ops.TTL1h})
	if err := h.inv.Revoke(context.Background(), InvitationCaller{UserID: "owner-1", Actor: "owner-1"}, inv.ID); err != nil {
		t.Fatal(err)
	}
	if c := h.enroll(newKey(t), "x", secretFrame(secretOf(t, inv.Key))).code(); c != proto.CodeInviteInvalid {
		t.Fatalf("code = %q", c)
	}
	page, _ := h.audit.List(context.Background(), audit.Filter{Action: AuditWorkerEnroll})
	if len(page.Events) != 1 || page.Events[0].Target != inv.ID || page.Events[0].Outcome != "invite_invalid" {
		t.Errorf("audit = %+v", page.Events)
	}
}

func TestTombstonedKeyIsAnsweredWithARevokeAndConsumesNothing(t *testing.T) {
	h := newEnrollHub(t, nil)
	inv := h.create("owner-1", false, ops.InvitationCreateArgs{Mode: ops.InvitationOneTime, TTL: ops.TTL1h})
	key := newKey(t)
	w := h.enrol(key, "owner-1", "old")
	if err := h.st.TombstoneWorker(context.Background(), w.ID, "owner-1", time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	a := h.enroll(key, "old", secretFrame(secretOf(t, inv.Key)))
	if a.typ != proto.TypeRevoke {
		t.Fatalf("answer = %s, want revoke", a.body)
	}
	if got := h.uses(inv.ID); got != 0 {
		t.Errorf("uses = %d, want 0", got)
	}
}

func TestEnrollRateLimitAppliesBeforeTheSecretIsRead(t *testing.T) {
	lim := &recordingLimiter{allow: 1, wait: time.Minute}
	h := newEnrollHub(t, func(c *EnrollConfig) { c.Limiter = lim })
	inv := h.create("owner-1", false, ops.InvitationCreateArgs{Mode: ops.InvitationReusable, TTL: ops.TTL1h})

	h.enroll(newKey(t), "a", secretFrame(secretOf(t, inv.Key))).welcome(t)
	a := h.enroll(newKey(t), "b", secretFrame(secretOf(t, inv.Key)))
	if a.code() != proto.CodeRateLimited {
		t.Fatalf("answer = %s, want rate_limited", a.body)
	}
	if got := h.uses(inv.ID); got != 1 {
		t.Errorf("uses = %d: a limited attempt consumed a use", got)
	}
	for _, p := range lim.pol {
		if p != ratelimit.PolicyEnroll {
			t.Errorf("policy = %q, want enroll", p)
		}
	}
}

func TestInfrastructureKeyFileEnrollsForTheDefaultOwner(t *testing.T) {
	var infraKey identity.InviteKey
	path := filepath.Join(t.TempDir(), "infra.key")
	h := newEnrollHub(t, func(c *EnrollConfig) {
		k, err := identity.NewInviteKey(c.HubFingerprint)
		if err != nil {
			t.Fatal(err)
		}
		infraKey = k
		if err := os.WriteFile(path, []byte(k.String()+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if c.Infra, err = LoadInfraKey(path, c.HubFingerprint); err != nil {
			t.Fatal(err)
		}
	})
	admin := seedUser(t, h.st, "root", true)
	user := seedUser(t, h.st, "ines", false)
	secret := secretOf(t, infraKey.String())

	// Default: the seeded admin owns the Worker, kind infrastructure.
	w := h.enroll(newKey(t), "edge", secretFrame(secret)).welcome(t)
	row, err := h.st.WorkerByID(context.Background(), w.WorkerID)
	if err != nil || row.OwnerID != admin || row.Kind != store.WorkerKindInfrastructure || w.Kind != "infrastructure" {
		t.Fatalf("row = %+v err=%v welcome=%+v", row, err, w)
	}
	if !row.Pending {
		t.Error("an infrastructure Worker must start pending too")
	}
	// The key is reusable and lasts 30 days from its first load; the name gets its suffix.
	h.clock.Advance(20 * 24 * time.Hour)
	if n := h.enroll(newKey(t), "edge", secretFrame(secret)).welcome(t).Name; n != "edge-2" {
		t.Errorf("second name = %q", n)
	}
	// Enrolling the same Worker key again returns the existing record.
	key := newKey(t)
	first := h.enroll(key, "again", secretFrame(secret)).welcome(t)
	if again := h.enroll(key, "again", secretFrame(secret)).welcome(t); again.WorkerID != first.WorkerID {
		t.Errorf("repeat = %+v, first = %+v", again, first)
	}

	// infra_default_owner names another user.
	h.enr.cfg.InfraOwner = "ines"
	w3 := h.enroll(newKey(t), "edge", secretFrame(secret)).welcome(t)
	if row, _ := h.st.WorkerByID(context.Background(), w3.WorkerID); row.OwnerID != user || w3.Name != "edge" {
		t.Errorf("owner = %q name = %q, want %q and edge", row.OwnerID, w3.Name, user)
	}

	// An unknown owner fails the enrollment closed with an internal error.
	h.enr.cfg.InfraOwner = "nobody"
	if c := h.enroll(newKey(t), "edge", secretFrame(secret)).code(); c != proto.CodeInternal {
		t.Errorf("unknown owner: code = %q", c)
	}
	// The key file is a row of its own: an admin sees it in both views, with
	// its use counter, and a user does not.
	adminCaller := InvitationCaller{UserID: admin, Actor: admin, Admin: true}
	for _, all := range []bool{true, false} {
		list, err := h.inv.List(context.Background(), adminCaller, all)
		if err != nil || len(list.Invitations) != 1 || list.Invitations[0].Source != "infra_file" || list.Invitations[0].Uses != 4 ||
			list.Invitations[0].ExpiresAt == nil {
			t.Fatalf("list(all=%v) = %+v err=%v", all, list, err)
		}
	}
	if list, err := h.inv.List(context.Background(), InvitationCaller{UserID: user, Actor: user}, false); err != nil || len(list.Invitations) != 0 {
		t.Errorf("a user sees the key file: %+v err=%v", list, err)
	}
	// 30 days after the first load the key has expired; the file is not reloaded.
	h.enr.cfg.InfraOwner = ""
	h.clock.Advance(11 * 24 * time.Hour)
	if c := h.enroll(newKey(t), "late", secretFrame(secret)).code(); c != proto.CodeInviteExpired {
		t.Errorf("after 31 days: code = %q, want invite_expired", c)
	}
}

// The key file row keeps its expiry, counter and revocation when the Hub
// starts again with the same file, and a revoked file key stays revoked until
// the file holds another key.
func TestInfrastructureKeyRowSurvivesAHubRestartAndRevocationSticks(t *testing.T) {
	var infraKey identity.InviteKey
	var infra *InfraKey
	var fpr identity.Fingerprint
	h := newEnrollHub(t, func(c *EnrollConfig) {
		k, err := identity.NewInviteKey(c.HubFingerprint)
		if err != nil {
			t.Fatal(err)
		}
		infraKey, fpr = k, c.HubFingerprint
		if infra, err = NewInfraKey(k.String(), c.HubFingerprint); err != nil {
			t.Fatal(err)
		}
		c.Infra, c.InfraExpiry = infra, 10*24*time.Hour
	})
	ctx := context.Background()
	admin := seedUser(t, h.st, "root", true)
	secret := secretOf(t, infraKey.String())
	h.enroll(newKey(t), "edge", secretFrame(secret)).welcome(t)

	row := func() store.InvitationRow {
		t.Helper()
		l, err := h.st.ListInvitationsWithInfra(ctx, admin, true)
		if err != nil || len(l) != 1 {
			t.Fatalf("rows = %+v err=%v", l, err)
		}
		return l[0]
	}
	first := row()
	if first.Uses != 1 || first.ExpiresAt == 0 {
		t.Fatalf("row = %+v", first)
	}
	// "Restart": a new enroller over the same database, with another lifetime
	// and a later clock. Nothing of the stored row changes.
	h.clock.Advance(3 * 24 * time.Hour)
	if _, err := NewEnroller(EnrollConfig{
		Store: h.st, HubFingerprint: fpr, Infra: infra, InfraExpiry: 400 * 24 * time.Hour, Now: h.clock.Now, Logger: textLogger(h.logs),
	}); err != nil {
		t.Fatal(err)
	}
	if again := row(); again.ID != first.ID || again.ExpiresAt != first.ExpiresAt || again.Uses != 1 || again.CreatedAt != first.CreatedAt {
		t.Errorf("a reload changed the row: %+v -> %+v", first, again)
	}
	// An admin revokes it through the invitations API; a reload does not undo it.
	if err := h.inv.Revoke(ctx, InvitationCaller{UserID: admin, Actor: admin, Admin: true}, first.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := NewEnroller(EnrollConfig{Store: h.st, HubFingerprint: fpr, Infra: infra, Now: h.clock.Now, Logger: textLogger(h.logs)}); err != nil {
		t.Fatal(err)
	}
	if c := h.enroll(newKey(t), "again", secretFrame(secret)).code(); c != proto.CodeInviteInvalid {
		t.Errorf("revoked key file enrolled: code = %q", c)
	}
	if !row().Revoked {
		t.Error("the revocation was lost")
	}
}

// A key file without an expiry is allowed, and the Hub warns at start.
func TestInfrastructureKeyWithoutExpiryWarnsAtStart(t *testing.T) {
	h := newEnrollHub(t, func(c *EnrollConfig) {
		k, err := identity.NewInviteKey(c.HubFingerprint)
		if err != nil {
			t.Fatal(err)
		}
		if c.Infra, err = NewInfraKey(k.String(), c.HubFingerprint); err != nil {
			t.Fatal(err)
		}
		c.InfraExpiry = NoInfraExpiry
	})
	admin := seedUser(t, h.st, "root", true)
	l, err := h.st.ListInvitationsWithInfra(context.Background(), admin, true)
	if err != nil || len(l) != 1 || l[0].ExpiresAt != 0 {
		t.Fatalf("rows = %+v err=%v", l, err)
	}
	if !strings.Contains(h.logs.String(), "has no expiry") {
		t.Errorf("no start-up warning; log:\n%s", h.logs.String())
	}
}

// ensureUser makes sure an active user with this id exists: an invitation key
// whose creator is not an active user enrolls nothing (AD-10).
func ensureUser(t *testing.T, st *store.Store, id string) {
	t.Helper()
	if _, err := st.DB().Exec(`INSERT OR IGNORE INTO users (id, username, active, created_at, updated_at) VALUES (?, ?, 1, 1, 1)`, id, id); err != nil {
		t.Fatal(err)
	}
}

func seedUser(t *testing.T, st *store.Store, name string, admin bool) string {
	t.Helper()
	id := store.NewID()
	if admin {
		created, err := st.SeedAdmin(context.Background(), store.SeedAdminParams{
			User:   store.UserRow{ID: id, Username: name, CredentialVersion: 1, CreatedAt: 1, UpdatedAt: 1},
			Marker: store.AdminSeedMarker,
		})
		if err != nil || !created {
			t.Fatalf("seed admin: created=%v err=%v", created, err)
		}
		return id
	}
	if _, err := st.DB().Exec(`INSERT INTO users (id, username, active, created_at, updated_at) VALUES (?, ?, 1, 1, 1) ; INSERT INTO user_roles (user_id, role, source) SELECT id, 'user', 'local' FROM users WHERE id NOT IN (SELECT user_id FROM user_roles)`, id, name); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestInfraKeyParsing(t *testing.T) {
	fpr := newHubFingerprint(t)
	k, err := identity.NewInviteKey(fpr)
	if err != nil {
		t.Fatal(err)
	}
	secret := secretOf(t, k.String())
	for name, text := range map[string]string{"whole key": k.String(), "bare secret": secret, "padded": "  " + secret + "\t"} {
		if _, err := NewInfraKey(text, fpr); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	other := newHubFingerprint(t)
	for name, text := range map[string]string{
		"another hub": "mnk_" + string(other) + "." + secret, "garbage": "hello", "empty": "", "uppercase secret": strings.ToUpper(secret),
	} {
		_, err := NewInfraKey(text, fpr)
		if err == nil {
			t.Errorf("%s: accepted", name)
		} else if text != "" && strings.Contains(err.Error(), text) {
			t.Errorf("%s: the error echoes the text: %v", name, err)
		}
	}
	if _, err := NewInfraKey(secret, "bad"); err == nil {
		t.Error("a bad hub fingerprint was accepted")
	}
}

func TestLoadInfraKeyFileErrors(t *testing.T) {
	fpr := newHubFingerprint(t)
	dir := t.TempDir()
	if _, err := LoadInfraKey(filepath.Join(dir, "missing"), fpr); err == nil {
		t.Error("a missing file was accepted")
	}
	empty := filepath.Join(dir, "empty")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadInfraKey(empty, fpr); err == nil {
		t.Error("an empty file was accepted")
	}
	bad := filepath.Join(dir, "bad")
	if err := os.WriteFile(bad, []byte("secret-looking-garbage\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := LoadInfraKey(bad, fpr)
	if err == nil || strings.Contains(err.Error(), "secret-looking-garbage") {
		t.Errorf("bad file: %v", err)
	}
}

func newHubFingerprint(t *testing.T) identity.Fingerprint {
	t.Helper()
	return newKey(t).Fingerprint()
}

func TestCleanWorkerName(t *testing.T) {
	long := strings.Repeat("\u00e9", 100) // two bytes per character
	cases := []struct{ in, want string }{
		{"pi", "pi"},
		{"  my pi\t", "my-pi"},
		{"a:b", "a-b"},
		{"line\nbreak", "line-break"},
		{"", "worker"},
		{"---", "worker"},
		{"..", "worker"},
		{"bad\xffbyte", "bad-byte"},
		{"keep.dots-and_underscores", "keep.dots-and_underscores"},
	}
	for _, c := range cases {
		if got := cleanWorkerName(c.in); got != c.want {
			t.Errorf("cleanWorkerName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	got := cleanWorkerName(long)
	if len(got) > maxEnrollNameLen || !strings.HasPrefix(long, got) {
		t.Errorf("long name: %d bytes %q", len(got), got)
	}
}

func TestNewEnrollerValidatesItsConfig(t *testing.T) {
	if _, err := NewEnroller(EnrollConfig{HubFingerprint: newHubFingerprint(t)}); err == nil {
		t.Error("no store accepted")
	}
	h := newTestHub(t, nil)
	if _, err := NewEnroller(EnrollConfig{Store: h.st}); err == nil {
		t.Error("no fingerprint accepted")
	}
	if _, err := NewEnroller(EnrollConfig{Store: h.st, HubFingerprint: "nope"}); err == nil {
		t.Error("a bad fingerprint was accepted")
	}
	e, err := NewEnroller(EnrollConfig{Store: h.st, HubFingerprint: h.svc.Identity().Fingerprint()})
	if err != nil {
		t.Fatal(err)
	}
	if err := RegisterEnroller(h.svc.Handler(), e); err != nil {
		t.Fatal(err)
	}
	if err := RegisterEnroller(h.svc.Handler(), e); err == nil {
		t.Error("a second registration was accepted")
	}
}

// AC6: revoking a key leaves a Worker that enrolled with it unaffected, and it
// still connects.
func TestRevokingTheKeyLeavesTheEnrolledWorkerConnecting(t *testing.T) {
	h := newEnrollHub(t, nil)
	inv := h.create("owner-1", false, ops.InvitationCreateArgs{Mode: ops.InvitationReusable, TTL: ops.TTL1h})
	key := newKey(t)
	first := h.enroll(key, "pi", secretFrame(secretOf(t, inv.Key))).welcome(t)

	if err := h.inv.Revoke(context.Background(), InvitationCaller{UserID: "owner-1", Actor: "owner-1"}, inv.ID); err != nil {
		t.Fatal(err)
	}
	// New enrollments with the revoked key fail ...
	if c := h.enroll(newKey(t), "new", secretFrame(secretOf(t, inv.Key))).code(); c != proto.CodeInviteInvalid {
		t.Errorf("enrolling with the revoked key: %q, want invite_invalid", c)
	}
	// ... while the Worker that enrolled with it connects as before.
	res, err := h.dial(h.workerConfig(key, "send_text"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = res.Channel.Close() })
	w := decodeWelcome(t, res)
	if w.WorkerID != first.WorkerID || w.Owner != "owner-1" {
		t.Errorf("welcome after revoke = %+v, enrolled as %+v", w, first)
	}
}

// AC3: the same worker_pub enrolling again, with another valid key, gets its
// existing record and the other key loses no use.
func TestSameWorkerPubWithAnotherKeyReturnsTheExistingWorker(t *testing.T) {
	h := newEnrollHub(t, nil)
	k1 := h.create("owner-1", false, ops.InvitationCreateArgs{Mode: ops.InvitationReusable, TTL: ops.TTL1h})
	k2 := h.create("owner-2", false, ops.InvitationCreateArgs{Mode: ops.InvitationReusable, TTL: ops.TTL1h})
	key := newKey(t)

	first := h.enroll(key, "pi", secretFrame(secretOf(t, k1.Key))).welcome(t)
	again := h.enroll(key, "other", secretFrame(secretOf(t, k2.Key))).welcome(t)
	if again.WorkerID != first.WorkerID || again.Owner != first.Owner || again.Owner != "owner-1" || again.Name != "pi" {
		t.Fatalf("second welcome = %+v, first = %+v", again, first)
	}
	if got := h.uses(k1.ID); got != 1 {
		t.Errorf("first key uses = %d, want 1", got)
	}
	if got := h.uses(k2.ID); got != 0 {
		t.Errorf("second key uses = %d, want 0", got)
	}
	if n, err := countWorkers(h.testHub); err != nil || n != 1 {
		t.Errorf("workers = %d err=%v, want 1", n, err)
	}
}

// AD-10: deactivation revokes keys. A key whose creator was deactivated
// enrolls nothing and consumes no use; the refusal is audited with the key id.
func TestKeyOfADeactivatedCreatorIsRefused(t *testing.T) {
	h := newEnrollHub(t, nil)
	inv := h.create("owner-1", false, ops.InvitationCreateArgs{Mode: ops.InvitationReusable, TTL: ops.TTL1h})
	secret := secretOf(t, inv.Key)
	key := newKey(t)
	h.enroll(key, "pi", secretFrame(secret)).welcome(t)

	if _, err := h.st.DB().Exec(`UPDATE users SET active = 0 WHERE id = 'owner-1'`); err != nil {
		t.Fatal(err)
	}
	if c := h.enroll(newKey(t), "new", secretFrame(secret)).code(); c != proto.CodeInviteInvalid {
		t.Fatalf("code = %q, want invite_invalid", c)
	}
	if got := h.uses(inv.ID); got != 1 {
		t.Errorf("uses = %d, want 1 (the refusal consumed nothing)", got)
	}
	if n, err := countWorkers(h.testHub); err != nil || n != 1 {
		t.Errorf("workers = %d err=%v, want 1", n, err)
	}
	page, _ := h.audit.List(context.Background(), audit.Filter{Action: AuditWorkerEnroll})
	var refused bool
	for _, e := range page.Events {
		refused = refused || (e.Target == inv.ID && e.Outcome == string(proto.CodeInviteInvalid))
	}
	if !refused {
		t.Errorf("no refusal audited with the invitation id: %+v", page.Events)
	}
	// A retry by the Worker that is already enrolled still gets its record.
	h.enroll(key, "pi", secretFrame(secret)).welcome(t)
}
