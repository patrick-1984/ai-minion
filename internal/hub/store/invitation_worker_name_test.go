package store

import (
	"context"
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"
)

// The Worker name of the key wins over the name the Worker announces: a
// Worker given only the key (the Android app, or any older Worker) announces
// its own name, and the Hub stores the one the key's creator chose.
func TestEnrollUsesTheInvitationWorkerName(t *testing.T) {
	s := openInviteTest(t)
	ctx := context.Background()
	one := inviteRow("owner", InvitationOneTime, 0, 1)
	one.WorkerName = "phone"
	if err := s.InsertInvitation(ctx, one); err != nil {
		t.Fatal(err)
	}
	if got, err := s.InvitationByID(ctx, one.ID); err != nil || got.WorkerName != "phone" {
		t.Fatalf("stored worker name = %q, %v", got.WorkerName, err)
	}
	res, err := s.EnrollWithInvitation(ctx, enrollParams(one, 1, "Pixel-8-Pro", 100))
	if err != nil {
		t.Fatal(err)
	}
	if res.Worker.Name != "owner-phone" {
		t.Errorf("worker name = %q, want owner-phone (the invitation's, with the owner prefix)", res.Worker.Name)
	}
	if w, err := s.WorkerByID(ctx, res.Worker.ID); err != nil || w.Name != "owner-phone" {
		t.Errorf("stored worker = %+v, %v", w, err)
	}

	// A reusable key names every Worker; the second gets the usual suffix.
	multi := inviteRow("owner", InvitationReusable, 0, 2)
	multi.WorkerName = "lab"
	if err := s.InsertInvitation(ctx, multi); err != nil {
		t.Fatal(err)
	}
	var names []string
	for i, key := range []byte{2, 3} {
		r, err := s.EnrollWithInvitation(ctx, enrollParams(multi, key, "host-"+string(rune('a'+i)), 200))
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, r.Worker.Name)
	}
	if strings.Join(names, ",") != "owner-lab,owner-lab-2" {
		t.Errorf("reusable names = %v", names)
	}

	// Without a Worker name, or with one that is not a valid Worker name
	// (a row written outside the operation), the announced name stays.
	plain := inviteRow("owner", InvitationOneTime, 0, 4)
	if err := s.InsertInvitation(ctx, plain); err != nil {
		t.Fatal(err)
	}
	if r, err := s.EnrollWithInvitation(ctx, enrollParams(plain, 4, "laptop", 300)); err != nil || r.Worker.Name != "owner-laptop" {
		t.Errorf("no worker name: %q, %v", r.Worker.Name, err)
	}
	bad := inviteRow("owner", InvitationOneTime, 0, 5)
	bad.WorkerName = "a b:c"
	if err := s.InsertInvitation(ctx, bad); err != nil {
		t.Fatal(err)
	}
	if r, err := s.EnrollWithInvitation(ctx, enrollParams(bad, 5, "desk", 400)); err != nil || r.Worker.Name != "owner-desk" {
		t.Errorf("invalid worker name: %q, %v", r.Worker.Name, err)
	}

	// The retry of an enrolled key changes nothing.
	again, err := s.EnrollWithInvitation(ctx, enrollParams(one, 1, "other", 500))
	if err != nil || !again.Existing || again.Worker.Name != "owner-phone" {
		t.Errorf("retry = %+v, %v", again, err)
	}
}

// The root-bound insert of invitation_create stores the Worker name too.
func TestInsertInvitationForRootKeepsTheWorkerName(t *testing.T) {
	s := openInviteTest(t)
	ctx := context.Background()
	r := inviteRow("owner", InvitationOneTime, 0, 9)
	r.WorkerName = "tablet"
	if err := s.InsertInvitationForRoot(ctx, r, "any"); err != nil {
		t.Fatal(err)
	}
	list, err := s.ListInvitations(ctx, "owner")
	if err != nil || len(list) != 1 || list[0].WorkerName != "tablet" {
		t.Fatalf("list = %+v, %v", list, err)
	}
}

// 0037 gives an unused one-time user key its name as the Worker name when the
// name can be one (the Add worker page used one field for both); other keys
// keep the Worker's own name.
func TestInvitationWorkerNameMigrationBackfill(t *testing.T) {
	old := fstest.MapFS{}
	entries, err := fs.ReadDir(hubMigrations, migrationsDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".sql") || e.Name() >= "0037" {
			continue
		}
		b, err := fs.ReadFile(hubMigrations, migrationsDir+"/"+e.Name())
		if err != nil {
			t.Fatal(err)
		}
		old[migrationsDir+"/"+e.Name()] = &fstest.MapFile{Data: b}
	}
	dir := t.TempDir()
	s, err := Open(dir, WithMigrations(old, migrationsDir))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	type row struct {
		id, name, mode string
		uses, revoked  int
		want           string
	}
	rows := []row{
		{"k1", "phone", "one_time", 0, 0, "phone"},
		{"k2", "lab worker setup", "one_time", 0, 0, ""},
		{"k3", "used", "one_time", 1, 0, ""},
		{"k4", "gone", "one_time", 0, 1, ""},
		{"k5", "fleet", "reusable", 0, 0, ""},
		{"k6", "", "one_time", 0, 0, ""},
		{"k7", "a:b", "one_time", 0, 0, ""},
		{"k8", strings.Repeat("x", 65), "one_time", 0, 0, ""},
	}
	for i, r := range rows {
		maxUses := any(nil)
		if r.mode == "one_time" {
			maxUses = 1
		}
		if _, err := s.db.Exec(`INSERT INTO invitation_keys (id, secret_hash, name, mode, kind, ttl, creator_id, created_at, uses, max_uses, revoked)
			VALUES (?, ?, ?, ?, 'private', '1h', 'u', 1, ?, ?, ?)`,
			r.id, []byte(strings.Repeat(string(rune('a'+i)), 32)), r.name, r.mode, r.uses, maxUses, r.revoked); err != nil {
			t.Fatal(err)
		}
	}
	_ = s.Close()
	s, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, r := range rows {
		got, err := s.InvitationByID(ctx, r.id)
		if err != nil {
			t.Fatal(err)
		}
		if got.WorkerName != r.want {
			t.Errorf("%s (%q): worker name = %q, want %q", r.id, r.name, got.WorkerName, r.want)
		}
	}
}
