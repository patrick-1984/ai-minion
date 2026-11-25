package server

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/auth/local"
	"github.com/patrick-1984/ai-minion/internal/hub/auth/session"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/hub/workers"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/testutil/hubstore"
)

// The demo Hub is a harness for looking at the web pages in a browser, not a
// test: it is skipped unless MINION_DEMO_ADDR names a listen address.
//
//	MINION_DEMO_ADDR=127.0.0.1:18790 MINION_DEMO_STOP=/tmp/demo-hub.stop \
//	  go test ./internal/hub/server/ -run TestDemoHub -count=1 -timeout 30m
//
// It serves the composed default Hub over the self-signed test TLS on that
// address, seeds two users, four Workers, 26 sessions, two master sessions with
// acceptances and eight grants, writes the URL, the credentials and the seeded
// identifiers to <MINION_DEMO_STOP>.info (and to standard error), and blocks
// until the file MINION_DEMO_STOP exists.
const (
	demoAdminName = "patryk"
	demoAdminPass = "demo-admin-pw-1"
	demoUserName  = "marta"
	demoUserPass  = "demo-user-pw-1"
)

// demoSessions are the sessions of the three online Workers, 26 in all.
var demoSessions = [][]string{
	{"master", "minion-1", "minion-2", "minion-3", "review-grants-store", "review-grants-web", "docs-access-architecture",
		"fix-control-page-accept-states", "release-0-21-0", "codex-gate-hub-core"},
	{"master", "minion-1", "minion-2", "feat-session-grants-authz", "feat-access-explain", "long-running-benchmark-of-the-pipe-broker",
		"scratch", "infra-dashboard", "home-lab"},
	{"minion-1", "minion-2", "marta-notes", "marta-private-research", "frontend-redesign", "billing-export", "support-inbox"},
}

func TestDemoHub(t *testing.T) {
	addr := os.Getenv("MINION_DEMO_ADDR")
	if addr == "" {
		t.Skip("set MINION_DEMO_ADDR (and MINION_DEMO_STOP) to run the demo Hub")
	}
	stop := os.Getenv("MINION_DEMO_STOP")
	if stop == "" {
		t.Fatal("MINION_DEMO_STOP must name the file whose creation stops the demo Hub")
	}
	if !strings.HasPrefix(addr, "127.0.0.1:") {
		t.Fatal("MINION_DEMO_ADDR must be on 127.0.0.1")
	}
	ctx := context.Background()
	ph := startDemoHub(t, addr)

	// The plain user, with a real password so that a browser can sign in.
	hash, err := local.NewHasher().Hash(ctx, demoUserPass)
	if err != nil {
		t.Fatal(err)
	}
	newUser := func(name, role, hash string) (store.UserRow, map[string]string) {
		t.Helper()
		now := time.Now().UnixMilli()
		u := store.UserRow{ID: store.NewID(), Username: name, PasswordHash: hash, CredentialVersion: 1, CreatedAt: now, UpdatedAt: now}
		if err := ph.st.CreateUser(ctx, store.CreateUserParams{User: u, Role: role}); err != nil {
			t.Fatal(err)
		}
		row, err := ph.st.UserByIdentifier(ctx, name)
		if err != nil {
			t.Fatal(err)
		}
		is, err := ph.mgr.Create(ctx, row, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		return row, asSession(is.Token, is.CSRFToken)
	}
	marta, _ := newUser(demoUserName, "user", hash)
	former, asFormer := newUser("former-admin", "admin", "x")

	connect := func(name string, owner store.UserRow, sessions []string) *pipeWorker {
		t.Helper()
		admin := ph.admin
		ph.admin = owner
		defer func() { ph.admin = admin }()
		pw := ph.connect(t, name)
		demoDeclare(t, ph, pw, sessions)
		return pw
	}
	w1 := connect("patryk-coder-patryk-work", ph.admin, demoSessions[0])
	w2 := connect("patryk-ausus-patrykf5", ph.admin, demoSessions[1])
	w3 := connect("marta-thinkpad-x1-carbon-gen-11", marta, demoSessions[2])
	// The fourth Worker is enrolled and never connects: offline.
	off := store.WorkerRow{ID: store.NewID(), Pub: []byte(strings.Repeat("o", 32)), Name: "patryk-raspberrypi-5-garage", OwnerID: ph.admin.ID,
		Kind: store.WorkerKindPrivate, CreatedAt: time.Now().Add(-48 * time.Hour).UnixMilli()}
	if err := ph.st.InsertWorker(ctx, nil, off); err != nil {
		t.Fatal(err)
	}

	var notes []string
	op := func(as map[string]string, name string, args map[string]any) (int, string) {
		t.Helper()
		args["request_id"] = ops.NewUUIDv7()
		code, body, _ := ph.hc.do("POST", "/api/v1/ops/"+name, args, as)
		if code != 200 {
			notes = append(notes, fmt.Sprintf("%s %v = %d %s", name, args, code, body))
		}
		return code, body
	}

	// Two name-only master sessions, and acceptances of all three kinds:
	// everything, limited to some sessions, and explicitly nothing.
	op(ph.asAdmin, ops.OpControlSetMaster, map[string]any{"worker_id": w1.id, "session_name": "master", "master": true})
	op(ph.asAdmin, ops.OpControlSetMaster, map[string]any{"worker_id": w2.id, "session_name": "master", "master": true})
	op(ph.asAdmin, ops.OpControlSetAccept, map[string]any{"worker_id": w2.id, "source_worker_id": w1.id, "accept": true})
	op(ph.asAdmin, ops.OpControlSetAccept, map[string]any{"worker_id": w3.id, "source_worker_id": w1.id, "accept": true,
		"sessions": []string{"minion-1", "frontend-redesign"}})
	op(ph.asAdmin, ops.OpControlSetWorker, map[string]any{"worker_id": w1.id, "accept_all": true})
	op(ph.asAdmin, ops.OpControlSetAccept, map[string]any{"worker_id": w1.id, "source_worker_id": w2.id, "accept": false})

	// Eight grants.
	type g struct {
		as                   map[string]string
		src, tgt             *pipeWorker
		srcSession, tgtSess  string
		level, expires, note string
	}
	past := time.Now().Add(-36 * time.Hour).UTC().Format("2006-01-02T15:04:05.000Z")
	future := time.Now().Add(14 * 24 * time.Hour).UTC().Format("2006-01-02T15:04:05.000Z")
	grants := []g{
		{ph.asAdmin, w1, w2, "review-grants-store", "feat-session-grants-authz", ops.GrantLevelView, "", "reviewer reads the implementer"},
		{ph.asAdmin, w1, w2, "review-grants-web", "feat-access-explain", ops.GrantLevelMessage, future, "review findings go back as messages"},
		{ph.asAdmin, w2, w1, "minion-1", "release-0-21-0", ops.GrantLevelControl, "", "release minion drives the release session"},
		{ph.asAdmin, w2, w1, "infra-dashboard", "", ops.GrantLevelView, "", "dashboard reads every shared session of the work machine"},
		{ph.asAdmin, w1, w2, "docs-access-architecture", "", ops.GrantLevelMessage, "", ""},
		{ph.asAdmin, w1, w3, "codex-gate-hub-core", "frontend-redesign", ops.GrantLevelView, "", "between users, granted by the administrator"},
		{ph.asAdmin, w1, w2, "minion-3", "long-running-benchmark-of-the-pipe-broker", ops.GrantLevelControl, past, "expired yesterday"},
		{asFormer, w2, w3, "home-lab", "support-inbox", ops.GrantLevelMessage, "", "granted by an administrator who was demoted"},
	}
	for _, x := range grants {
		args := map[string]any{"source_worker_id": x.src.id, "source_session": x.srcSession, "target_worker_id": x.tgt.id, "level": x.level}
		if x.tgtSess != "" {
			args["target_session"] = x.tgtSess
		}
		if x.expires != "" {
			args["expires_at"] = x.expires
		}
		if x.note != "" {
			args["note"] = x.note
		}
		op(x.as, ops.OpGrantSet, args)
	}
	// The administrator behind the last grant is demoted: the grant is suspended.
	if _, err := ph.st.DB().ExecContext(ctx, `DELETE FROM user_roles WHERE user_id = ? AND role = 'admin'`, former.ID); err != nil {
		t.Fatal(err)
	}
	// Whatever the write path did with a past expiry, the stored grant is expired.
	if _, err := ph.st.DB().ExecContext(ctx, `UPDATE session_grant SET expires_at = ? WHERE note = 'expired yesterday'`,
		time.Now().Add(-36*time.Hour).UnixMilli()); err != nil {
		notes = append(notes, "expire the grant in the store: "+err.Error())
	}

	info := map[string]any{
		"url":   "https://" + ph.h.srv.Addr(),
		"admin": map[string]string{"username": demoAdminName, "password": demoAdminPass},
		"user":  map[string]string{"username": demoUserName, "password": demoUserPass},
		"workers": []map[string]any{
			{"id": w1.id, "name": w1.name, "owner": demoAdminName, "sessions": demoSessions[0]},
			{"id": w2.id, "name": w2.name, "owner": demoAdminName, "sessions": demoSessions[1]},
			{"id": w3.id, "name": w3.name, "owner": demoUserName, "sessions": demoSessions[2]},
			{"id": off.ID, "name": off.Name, "owner": demoAdminName, "offline": true},
		},
		"seed_notes": notes,
	}
	b, _ := json.MarshalIndent(info, "", "  ")
	fmt.Fprintf(os.Stderr, "demo Hub (self-signed TLS): %s\n", b)
	if err := os.WriteFile(stop+".info", b, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(stop + ".info") })

	for {
		if _, err := os.Stat(stop); err == nil {
			_ = os.Remove(stop)
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// startDemoHub is startPipeHub on a fixed listen address with an administrator
// whose password the browser can type.
func startDemoHub(t *testing.T, addr string) *pipeHub {
	t.Helper()
	t.Setenv("MINION_ADMIN_USERNAME", demoAdminName)
	t.Setenv("MINION_ADMIN_PASSWORD", demoAdminPass)
	r := DefaultRegistry.clone()
	ph := &pipeHub{}
	if err := r.Register("grab", 100, func(_ context.Context, e *Env) error {
		var err error
		if ph.st, err = Require[*store.Store](e); err != nil {
			return err
		}
		if ph.wsv, err = Require[*workers.Service](e); err != nil {
			return err
		}
		if ph.settings, err = Require[*SettingsService](e); err != nil {
			return err
		}
		if ph.pipe, err = Require[*workers.Pipe](e); err != nil {
			return err
		}
		if ph.events, err = Require[*workers.EventHub](e); err != nil {
			return err
		}
		ph.mgr, err = Require[*session.Manager](e)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	ph.h = startHub(t, &Config{DataDir: hubstore.Dir(t), Listen: addr}, r)
	ph.hc = newHubClient(t, ph.h, "https")
	var err error
	if ph.admin, err = ph.st.UserByIdentifier(context.Background(), demoAdminName); err != nil {
		t.Fatal(err)
	}
	is, err := ph.mgr.Create(context.Background(), ph.admin, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	ph.asAdmin = asSession(is.Token, is.CSRFToken)
	return ph
}

// demoDeclare declares the share rule "all" of the Worker with a snapshot of
// the named sessions, in several kinds and states, and waits until the Hub
// holds them.
func demoDeclare(t *testing.T, ph *pipeHub, pw *pipeWorker, names []string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	evs, err := pw.sess.OpenEvents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { evs.Reset() })
	kinds := []string{"agent:claude", "agent:codex", "agent:claude", "shell"}
	states := []string{ops.PaneStateStable, ops.PaneStateBusy, ops.PaneStateStable, ops.PaneStateDialog}
	tasks := []string{"Review the store slice of the session grants", "Waiting for the integration branch",
		"Write the architecture page of the access model and link it from the guide", ""}
	var snap []map[string]any
	for i, n := range names {
		snap = append(snap, map[string]any{"name": n, "kind": kinds[i%len(kinds)], "state": states[i%len(states)], "task": tasks[i%len(tasks)]})
	}
	if err := evs.WriteFrame(map[string]any{"type": proto.TypeEventsSnap, "seq": 0, "sessions": snap,
		"share": map[string]any{"rule": map[string]any{"mode": "all"}, "revision": 1}}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, pw.name+" sessions held", func() bool {
		s, ok := ph.events.Sessions(pw.id)
		return ok && s.Synced && len(s.Sessions) == len(names) && !ph.events.Fenced(pw.id, names[0])
	})
}
