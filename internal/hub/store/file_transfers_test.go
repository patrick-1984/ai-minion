package store

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/ops"
)

const v0240Schema = 42

func boolPtr(b bool) *bool { return &b }

// Migration 0042 on a database at v0.23.0's schema: every stored grant,
// acceptance and Worker reads as no file access / the default, and the new
// tables exist.
func TestUpgradeFromV0230SchemaFileTransfer(t *testing.T) {
	dir := t.TempDir()
	old := openBefore(t, dir, "0042")
	if v := dbSchemaVersion(t, old); v != v0230Schema {
		t.Fatalf("old schema version = %d, want %d", v, v0230Schema)
	}
	exec := func(s *Store, q string) {
		t.Helper()
		if _, err := s.DB().Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	exec(old, `INSERT INTO users (id, username, active, created_at, updated_at) VALUES ('usr', 'usr', 1, 1, 1)`)
	exec(old, `INSERT INTO user_roles VALUES ('usr', 'user', 'local')`)
	exec(old, `INSERT INTO workers (id, worker_pub, name, owner_id, kind, created_at) VALUES ('wa', randomblob(32), 'a', 'usr', 'private', 1)`)
	exec(old, `INSERT INTO workers (id, worker_pub, name, owner_id, kind, created_at) VALUES ('wb', randomblob(32), 'b', 'usr', 'private', 2)`)
	exec(old, `INSERT INTO session_grant (id, source_worker_id, source_session, source_incarnation, target_worker_id, target_session, level,
		authority, granted_by, source_owner_id, target_owner_id, created_at, updated_at)
		VALUES ('g1', 'wa', 'agent', 'inc', 'wb', 'shell', 3, 'owner', 'usr', 'usr', 'usr', 1, 1)`)
	exec(old, `INSERT INTO control_accept (target_worker_id, source_worker_id, sessions_json, created_by, created_at) VALUES ('wb', 'wa', NULL, 'usr', 1)`)
	_ = old.Close()

	cur, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cur.Close() }()
	ctx := context.Background()
	if v := dbSchemaVersion(t, cur); v < v0240Schema {
		t.Fatalf("upgraded schema version = %d, want at least %d", v, v0240Schema)
	}
	g, err := cur.SessionGrant(ctx, "g1")
	if err != nil || g.FilesPull || g.FilesPush {
		t.Errorf("v0.23 grant = %+v, %v", g, err)
	}
	acc, err := cur.ListAccepts(ctx)
	if err != nil || len(acc) != 1 || *acc[0].FilesPull || *acc[0].FilesPush {
		t.Errorf("v0.23 acceptance = %+v, %v", acc, err)
	}
	if fa, err := cur.FileAcceptances(ctx, "wa"); err != nil || len(fa) != 0 {
		t.Errorf("file acceptances = %+v, %v", fa, err)
	}
	if m, err := cur.WorkerFileMode(ctx, "wa"); err != nil || m != ops.FileModeOn {
		t.Errorf("worker file mode = %q, %v", m, err)
	}
	for _, table := range []string{"file_transfers", "file_transfer_entries"} {
		var n int
		if err := cur.DB().QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&n); err != nil || n != 1 {
			t.Errorf("table %s: %d, %v", table, n, err)
		}
	}
}

// The CHECKs of the grant flags: files_push only at level 3, both only on a
// verified grant; unbinding clears them.
func TestSessionGrantFileFlags(t *testing.T) {
	f := newGrantFixture(t)
	ctx := t.Context()
	// A claimed grant (every new grant is) refuses the flags.
	claimed := f.ownerGrant("boss")
	claimed.FilesPull = true
	if _, err := f.s.PutSessionGrant(ctx, claimed, 0, GrantWrite{ActorID: "alice"}); !errors.Is(err, ErrGrantInvalid) {
		t.Fatalf("pull flag on a claimed grant: %v", err)
	}
	row := f.ownerGrant("boss")
	row.Level = 3
	g := f.put(t, row)
	g, err := f.s.BindSessionGrant(ctx, g.ID, "inc-1", g.Revision, GrantWrite{ActorID: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	g.FilesPull, g.FilesPush, g.UpdatedAt = true, true, 0
	g, err = f.s.PutSessionGrant(ctx, g, g.Revision, GrantWrite{ActorID: "alice"})
	if err != nil || !g.FilesPull || !g.FilesPush {
		t.Fatalf("flags on a verified Control grant = %+v, %v", g, err)
	}
	if got, err := f.s.SessionGrant(ctx, g.ID); err != nil || !got.FilesPull || !got.FilesPush {
		t.Fatalf("read back = %+v, %v", got, err)
	}
	// files_push below Control is refused by the table.
	low := g
	low.Level, low.UpdatedAt = 2, 0
	if _, err := f.s.PutSessionGrant(ctx, low, g.Revision, GrantWrite{ActorID: "alice"}); !errors.Is(err, ErrGrantInvalid) {
		t.Fatalf("push flag at level 2: %v", err)
	}
	low.FilesPush = false
	if g, err = f.s.PutSessionGrant(ctx, low, g.Revision, GrantWrite{ActorID: "alice"}); err != nil || !g.FilesPull || g.FilesPush {
		t.Fatalf("pull flag at level 2 = %+v, %v", g, err)
	}
	// Unbinding makes the grant claimed: the flags go.
	g, err = f.s.BindSessionGrant(ctx, g.ID, "", g.Revision, GrantWrite{ActorID: "alice"})
	if err != nil || g.FilesPull || g.FilesPush || g.SourceIncarnation != "" {
		t.Fatalf("unbound = %+v, %v", g, err)
	}
}

// Acceptance flags: unset on a new row, kept by a put without them, set by a
// put with them, cleared by a revoke, listed only from their own row.
func TestAcceptFileFlags(t *testing.T) {
	f := newGrantFixture(t)
	ctx := t.Context()
	put := func(a AcceptRow) {
		t.Helper()
		if err := f.s.PutAcceptIfOwners(ctx, a, "alice", "alice"); err != nil {
			t.Fatal(err)
		}
	}
	read := func() AcceptRow {
		t.Helper()
		rows, err := f.s.ListAccepts(ctx)
		if err != nil || len(rows) != 1 {
			t.Fatalf("accepts = %+v, %v", rows, err)
		}
		return rows[0]
	}
	base := AcceptRow{TargetWorkerID: f.b.ID, SourceWorkerID: f.a.ID, CreatedBy: "alice", CreatedAt: 1}
	put(base)
	if r := read(); *r.FilesPull || *r.FilesPush {
		t.Fatalf("new row = %+v", r)
	}
	with := base
	with.FilesPull = boolPtr(true)
	put(with)
	if r := read(); !*r.FilesPull || *r.FilesPush {
		t.Fatalf("pull set = %+v", r)
	}
	narrowed := base
	narrowed.Sessions = []string{"x"}
	put(narrowed) // no flags given: kept
	if r := read(); !*r.FilesPull || !slices.Equal(r.Sessions, []string{"x"}) {
		t.Fatalf("flags kept = %+v", r)
	}
	fa, err := f.s.FileAcceptances(ctx, f.a.ID)
	if err != nil || len(fa) != 1 || fa[0].TargetWorkerID != f.b.ID || AcceptanceID(fa[0].TargetWorkerID, fa[0].SourceWorkerID) != f.b.ID+"/"+f.a.ID {
		t.Fatalf("file acceptances = %+v, %v", fa, err)
	}
	if fa, err := f.s.FileAcceptances(ctx, f.b.ID); err != nil || len(fa) != 0 {
		t.Fatalf("file acceptances of another source = %+v, %v", fa, err)
	}
	// A revoke on a Worker that accepts all keeps a '[]' row without flags.
	all := true
	if err := f.s.SetWorkerControl(ctx, f.b.ID, nil, &all); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.RevokeAcceptIfOwners(ctx, base, "alice", "alice"); err != nil {
		t.Fatal(err)
	}
	if r := read(); *r.FilesPull || *r.FilesPush || r.Sessions == nil || len(r.Sessions) != 0 {
		t.Fatalf("revoked = %+v", r)
	}
	if fa, err := f.s.FileAcceptances(ctx, f.a.ID); err != nil || len(fa) != 0 {
		t.Fatalf("file acceptances after the revoke = %+v, %v", fa, err)
	}
}

func TestWorkerFileMode(t *testing.T) {
	f := newGrantFixture(t)
	ctx := t.Context()
	if ch, err := f.s.SetWorkerFileMode(ctx, f.a.ID, ops.FileModeSendOnly); err != nil || !ch {
		t.Fatalf("set = %v, %v", ch, err)
	}
	if ch, err := f.s.SetWorkerFileMode(ctx, f.a.ID, ops.FileModeSendOnly); err != nil || ch {
		t.Fatalf("set again = %v, %v", ch, err)
	}
	if m, err := f.s.WorkerFileMode(ctx, f.a.ID); err != nil || m != ops.FileModeSendOnly {
		t.Fatalf("mode = %q, %v", m, err)
	}
	if _, err := f.s.SetWorkerFileMode(ctx, f.a.ID, "sometimes"); !errors.Is(err, ErrFileTransferInvalid) {
		t.Fatalf("unknown mode: %v", err)
	}
	if _, err := f.s.WorkerFileMode(ctx, "nope"); !errors.Is(err, ErrWorkerNotFound) {
		t.Fatalf("unknown Worker: %v", err)
	}
}

func admitted(id string, at int64, principalUser, srcOwner, tgtOwner, target string) FileTransferRow {
	deadline := at + 60_000
	return FileTransferRow{ID: id, Direction: ops.FileDirectionPull, State: ops.FileStateAdmitted,
		Principal: "user:" + principalUser, PrincipalUserID: principalUser, SourceOwnerID: srcOwner,
		TargetWorkerID: target, TargetSession: "s", TargetOwnerID: tgtOwner, Path: "a.txt",
		GrantIDs: []string{"g1"}, Receiver: "worker:w1", ReceiptHash: "ab", CreatedAt: at, DeadlineAt: &deadline}
}

// The record lifecycle: admitted before bytes move, prepared, terminal once;
// unknown resolved by a late receipt; denials terminal at once.
func TestFileTransferLifecycle(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	id := ops.NewUUIDv7()
	r := admitted(id, 1000, "alice", "", "bob", "wt")
	if err := s.InsertFileTransfer(ctx, r); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertFileTransfer(ctx, r); !errors.Is(err, ErrFileTransferExists) {
		t.Fatalf("duplicate: %v", err)
	}
	noDeadline := admitted(ops.NewUUIDv7(), 1000, "alice", "", "bob", "wt")
	noDeadline.DeadlineAt = nil
	if err := s.InsertFileTransfer(ctx, noDeadline); !errors.Is(err, ErrFileTransferInvalid) {
		t.Fatalf("admitted without deadline: %v", err)
	}
	bad := admitted(ops.NewUUIDv7(), 1000, "alice", "", "bob", "wt")
	bad.State = ops.FileStateDelivered
	if err := s.InsertFileTransfer(ctx, bad); !errors.Is(err, ErrFileTransferInvalid) {
		t.Fatalf("starts delivered: %v", err)
	}
	if err := s.MarkFileTransferPrepared(ctx, id, FilePrepared{TargetRoot: "~/app", Kind: ops.FileKindFile, Bytes: 5}, 1100); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkFileTransferPrepared(ctx, id, FilePrepared{}, 1100); !errors.Is(err, ErrFileTransferState) {
		t.Fatalf("prepared twice: %v", err)
	}
	got, err := s.FileTransfer(ctx, id)
	if err != nil || got.State != ops.FileStatePrepared || got.TargetRoot != "~/app" || got.Kind != ops.FileKindFile ||
		!slices.Equal(got.GrantIDs, []string{"g1"}) || got.EndedAt != nil || got.ReceiptHash != "ab" {
		t.Fatalf("prepared = %+v, %v", got, err)
	}
	// Reconciliation: past the deadline it becomes unknown.
	if ids, err := s.ReconcileFileTransfers(ctx, 1000+59_999); err != nil || len(ids) != 0 {
		t.Fatalf("early reconcile = %v, %v", ids, err)
	}
	if ids, err := s.ReconcileFileTransfers(ctx, 1000+60_001); err != nil || !slices.Equal(ids, []string{id}) {
		t.Fatalf("reconcile = %v, %v", ids, err)
	}
	if got, _ := s.FileTransfer(ctx, id); got.State != ops.FileStateUnknown || got.EndedAt == nil || *got.EndedAt != 61_001 {
		t.Fatalf("reconciled = %+v", got)
	}
	// unknown does not overwrite unknown; a late receipt resolves it once.
	if _, ch, err := s.FinishFileTransfer(ctx, id, FileOutcome{State: ops.FileStateUnknown}, 70_000); err != nil || ch {
		t.Fatalf("unknown again = %v, %v", ch, err)
	}
	entries := make([]ops.FileTransferEntry, MaxStoredFileEntries+5)
	for i := range entries {
		entries[i] = ops.FileTransferEntry{Path: "f", Size: int64(i)}
	}
	digest := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	row, ch, err := s.FinishFileTransfer(ctx, id, FileOutcome{State: ops.FileStateDelivered, Files: 3, Bytes: 9, WireBytes: 7,
		ManifestDigest: digest, Entries: entries, EntriesOmitted: 2}, 80_000)
	if err != nil || !ch || row.State != ops.FileStateDelivered || row.EntriesOmitted != 7 || *row.EndedAt != 61_001 || row.DeadlineAt != nil {
		t.Fatalf("late receipt = %+v, %v, %v", row, ch, err)
	}
	if es, err := s.FileTransferEntries(ctx, id); err != nil || len(es) != MaxStoredFileEntries || es[10].Size != 10 {
		t.Fatalf("entries = %d, %v", len(es), err)
	}
	// A repeated receipt is acknowledged with the state reached.
	row, ch, err = s.FinishFileTransfer(ctx, id, FileOutcome{State: ops.FileStateFailedNotSent}, 90_000)
	if err != nil || ch || row.State != ops.FileStateDelivered {
		t.Fatalf("repeat = %+v, %v, %v", row, ch, err)
	}
	if _, _, err := s.FinishFileTransfer(ctx, id, FileOutcome{State: ops.FileStateAdmitted}, 90_000); !errors.Is(err, ErrFileTransferInvalid) {
		t.Fatalf("not an outcome: %v", err)
	}
	if _, _, err := s.FinishFileTransfer(ctx, ops.NewUUIDv7(), FileOutcome{State: ops.FileStateDelivered}, 90_000); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
	// A denial is terminal at once and has no digest.
	den := admitted(ops.NewUUIDv7(), 2000, "alice", "", "bob", "wt")
	den.State, den.Reason, den.ManifestDigest = ops.FileStateDenied, "forbidden", digest
	if err := s.InsertFileTransfer(ctx, den); err != nil {
		t.Fatal(err)
	}
	if got, err := s.FileTransfer(ctx, den.ID); err != nil || got.EndedAt == nil || got.DeadlineAt != nil || got.ManifestDigest != "" {
		t.Fatalf("denied = %+v, %v", got, err)
	}
	if _, ch, err := s.FinishFileTransfer(ctx, den.ID, FileOutcome{State: ops.FileStateDelivered}, 3000); err != nil || ch {
		t.Fatalf("finish a denial = %v, %v", ch, err)
	}
	rec := row.Record(nil)
	if rec.TransferID != id || rec.GrantID != "g1" || rec.State != ops.FileStateDelivered || rec.EndedAt == nil {
		t.Fatalf("record = %+v", rec)
	}
}

// The owner-scoped history: the target owner sees an admin's push into their
// session, an unrelated user does not, an admin sees all; pages, filters and
// retention.
func TestListFileTransfersScope(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	var ids []string
	add := func(at int64, principal, srcOwner, tgtOwner, target string) {
		t.Helper()
		r := admitted(ops.NewUUIDv7(), at, principal, srcOwner, tgtOwner, target)
		r.Direction = ops.FileDirectionPush
		if err := s.InsertFileTransfer(ctx, r); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, r.ID)
	}
	add(1000, "root", "", "bob", "wb")       // 0: admin pushes into bob's session
	add(2000, "alice", "alice", "bob", "wb") // 1: alice's Worker to bob's
	add(3000, "alice", "alice", "alice", "wa")
	add(4000, "carol", "carol", "carol", "wc")
	list := func(f FileTransferFilter) []string {
		t.Helper()
		rows, _, err := s.ListFileTransfers(ctx, f)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, r := range rows {
			out = append(out, r.ID)
		}
		return out
	}
	if got := list(FileTransferFilter{ViewerID: "bob"}); !slices.Equal(got, []string{ids[1], ids[0]}) {
		t.Errorf("bob sees %v", got)
	}
	if got := list(FileTransferFilter{ViewerID: "alice"}); !slices.Equal(got, []string{ids[2], ids[1]}) {
		t.Errorf("alice sees %v", got)
	}
	if got := list(FileTransferFilter{ViewerID: "dave"}); len(got) != 0 {
		t.Errorf("an unrelated user sees %v", got)
	}
	if got := list(FileTransferFilter{}); len(got) != 0 {
		t.Errorf("no viewer sees %v", got)
	}
	if got := list(FileTransferFilter{ViewerID: "root", Admin: true}); !slices.Equal(got, []string{ids[3], ids[2], ids[1], ids[0]}) {
		t.Errorf("admin sees %v", got)
	}
	if got := list(FileTransferFilter{ViewerID: "root", Admin: true, WorkerID: "wb"}); !slices.Equal(got, []string{ids[1], ids[0]}) {
		t.Errorf("admin, Worker wb: %v", got)
	}
	if got := list(FileTransferFilter{ViewerID: "bob", TransferID: ids[2]}); len(got) != 0 {
		t.Errorf("bob, alice's own transfer: %v", got)
	}
	// Pages of one.
	var paged []string
	cursor := ""
	for range 10 {
		rows, next, err := s.ListFileTransfers(ctx, FileTransferFilter{Admin: true, Limit: 1, Cursor: cursor})
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range rows {
			paged = append(paged, r.ID)
		}
		if next == "" {
			break
		}
		cursor = next
	}
	if !slices.Equal(paged, []string{ids[3], ids[2], ids[1], ids[0]}) {
		t.Errorf("pages = %v", paged)
	}
	if _, _, err := s.ListFileTransfers(ctx, FileTransferFilter{Admin: true, Cursor: "!!"}); !errors.Is(err, ErrFileCursor) {
		t.Errorf("bad cursor: %v", err)
	}
	// Retention: records created before the limit go, with their entries.
	if _, _, err := s.FinishFileTransfer(ctx, ids[0], FileOutcome{State: ops.FileStatePartial,
		Entries: []ops.FileTransferEntry{{Path: "x"}}}, 5000); err != nil {
		t.Fatal(err)
	}
	if n, err := s.PurgeFileTransfers(ctx, 2500); err != nil || n != 2 {
		t.Fatalf("purge = %d, %v", n, err)
	}
	if got := list(FileTransferFilter{Admin: true}); !slices.Equal(got, []string{ids[3], ids[2]}) {
		t.Errorf("after the purge: %v", got)
	}
	if es, err := s.FileTransferEntries(ctx, ids[0]); err != nil || len(es) != 0 {
		t.Errorf("entries after the purge = %v, %v", es, err)
	}
}
