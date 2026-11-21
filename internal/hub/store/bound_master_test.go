package store

import (
	"errors"
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/sqlmigrate"
)

func TestBoundMasterFencesLegacyGenerationAndOwnership(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	for _, owner := range []string{"alice", "next"} {
		if _, err := s.DB().ExecContext(ctx, `INSERT INTO users(id,username,created_at,updated_at) VALUES(?,?,1,1);`, owner, owner); err != nil {
			t.Fatal(err)
		}
		if _, err := s.DB().ExecContext(ctx, `INSERT INTO user_roles(user_id,role,source) VALUES(?,'admin','local')`, owner); err != nil {
			t.Fatal(err)
		}
	}
	a := worker(ops.NewUUIDv7(), 4, "alice", "alice-master")
	a.Kind = WorkerKindInfrastructure
	b := worker(ops.NewUUIDv7(), 5, "alice", "alice-target")
	for _, w := range []WorkerRow{a, b} {
		if err := s.InsertWorker(ctx, nil, w); err != nil {
			t.Fatal(err)
		}
		if err := s.SaveWorkerShare(ctx, w.ID, ops.ShareDecl{Rule: ops.ShareRule{Mode: "all"}, Revision: 1}); err != nil {
			t.Fatal(err)
		}
	}
	old := SessionControlRow{WorkerID: a.ID, SessionName: "boss", CreatedBy: "alice",
		TargetsJSON: `[{"kind":"session","worker_id":"` + b.ID + `","session_name":"allowed"}]`}
	if err := s.PutSessionControl(ctx, old); err != nil {
		t.Fatal(err)
	}
	arg := func(incarnation string, revision uint64, active bool) ops.ControlSetBoundMasterArgs {
		return ops.ControlSetBoundMasterArgs{WriteArgs: ops.WriteArgs{RequestID: ops.NewUUIDv7()}, WorkerID: a.ID,
			SessionName: "boss", IncarnationID: incarnation, IfGeneration: &revision, Master: active}
	}
	nominate := arg("incarnation-one", 0, true)
	if _, err := s.SetBoundMaster(ctx, a, nominate, "alice", 1, false); !errors.Is(err, ErrMasterCandidate) {
		t.Fatalf("unverified nomination: %v", err)
	}
	if _, err := s.MasterSession(ctx, a.ID, "boss"); err != nil {
		t.Fatalf("failed conversion did not roll back legacy grant: %v", err)
	}
	if changed, err := s.SetBoundMaster(ctx, a, nominate, "alice", 1, true); err != nil || !changed {
		t.Fatalf("verified nomination: change=%t, %v", changed, err)
	}
	if _, err := s.MasterSession(ctx, a.ID, "boss"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("legacy authority survived conversion: %v", err)
	}
	rows, err := s.ListBoundMasters(ctx, a.ID)
	if err != nil || len(rows) != 1 || rows[0].LegacyTargetsJSON != old.TargetsJSON || rows[0].Generation != 1 {
		t.Fatalf("wrong durable conversion: %+v %v", rows, err)
	}
	first := rows[0]
	if _, err := s.BoundMasterTargets(ctx, first, &MasterTarget{WorkerID: b.ID, OwnerID: b.OwnerID, Sessions: []string{"allowed"}}); err != nil {
		t.Fatalf("migrated target was lost: %v", err)
	}
	if _, err := s.BoundMasterTargets(ctx, first, &MasterTarget{WorkerID: b.ID, OwnerID: b.OwnerID, Sessions: []string{"not-authorized"}}); !errors.Is(err, ErrMasterCandidate) {
		t.Fatalf("migrated target restriction disappeared: %v", err)
	}
	if err := s.PutMasterSession(ctx, MasterRow{WorkerID: a.ID, SessionName: "boss"}); !errors.Is(err, ErrBoundMasterSlot) {
		t.Fatalf("old API wrote bound slot: %v", err)
	}
	if err := s.PutSessionControl(ctx, old); !errors.Is(err, ErrBoundMasterSlot) {
		t.Fatalf("v0.8 API wrote bound slot: %v", err)
	}
	duplicate := nominate
	duplicate.IncarnationID = "different-input"
	if _, err := s.SetBoundMaster(ctx, a, duplicate, "alice", 2, true); !errors.Is(err, ErrMasterRevision) {
		t.Fatalf("changed input reused the request ID: %v", err)
	}
	revoke := arg("incarnation-one", 1, false)
	if changed, err := s.SetBoundMaster(ctx, a, revoke, "alice", 2, false); err != nil || !changed {
		t.Fatalf("offline revocation: change=%t, %v", changed, err)
	}
	if _, err := s.BoundMasterTargets(ctx, first, nil); !errors.Is(err, ErrMasterRevision) {
		t.Fatalf("committed revoke left old authority: %v", err)
	}
	if changed, err := s.SetBoundMaster(ctx, a, nominate, "alice", 2, false); err != nil || changed {
		t.Fatalf("recorded request replay mutated or required a live candidate: %t %v", changed, err)
	}
	if _, err := s.SetBoundMaster(ctx, a, arg("incarnation-two", 0, true), "alice", 3, true); !errors.Is(err, ErrMasterRevision) {
		t.Fatalf("a delayed initial create crossed nominate/revoke: %v", err)
	}
	next := arg("incarnation-two", 2, true)
	if changed, err := s.SetBoundMaster(ctx, a, next, "alice", 3, true); err != nil || !changed {
		t.Fatalf("explicit replacement: %t %v", changed, err)
	}
	if _, err := s.SetBoundMaster(ctx, a, arg("incarnation-one", 3, false), "alice", 4, false); !errors.Is(err, ErrMasterRevision) {
		t.Fatalf("wrong-incarnation revoke crossed replacement: %v", err)
	}
	for _, owner := range []string{"next", "alice"} {
		if _, err := s.ReassignWorker(ctx, ReassignParams{WorkerID: a.ID, NewOwnerID: owner, AtMS: 5}); err != nil {
			t.Fatal(err)
		}
	}
	rows, err = s.ListBoundMasters(ctx, a.ID)
	if err != nil || len(rows) != 1 || rows[0].Active || rows[0].Generation != 5 {
		t.Fatalf("reassignment did not retain an inactive revision fence: %+v %v", rows, err)
	}
}

func TestPreviousSchemaRefusesMigratedMasterDatabase(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	m := embeddedMigrations()
	entries, err := fs.ReadDir(m.fsys, m.dir)
	if err != nil {
		t.Fatal(err)
	}
	old := fstest.MapFS{}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "0029_") {
			continue
		}
		path := m.dir + "/" + e.Name()
		data, err := fs.ReadFile(m.fsys, path)
		if err != nil {
			t.Fatal(err)
		}
		old[path] = &fstest.MapFile{Data: data}
	}
	downgraded, err := Open(dir, WithMigrations(old, m.dir))
	if downgraded != nil {
		_ = downgraded.Close()
	}
	if !errors.Is(err, sqlmigrate.ErrSchemaTooNew) {
		t.Fatalf("previous-release migrations opened the bound master store: %v", err)
	}
}
