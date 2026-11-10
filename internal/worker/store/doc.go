// Package store owns the Worker SQLite database worker.db and its migrations (AD-16). It is one of the two packages that import the SQLite driver.
//
// [Open] opens <state>/worker.db as one *sql.DB (WAL, busy_timeout=5000,
// _txlock=immediate, owner-only file) and applies the embedded migrations
// (migrations/NNNN_name.sql) through internal/sqlmigrate: forward-only, in one
// transaction, after a VACUUM INTO backup in <state>/backups (worker-<ts>.db,
// skipped for a new empty database), refusing a newer schema with an error
// that wraps [sqlmigrate.ErrSchemaTooNew] (code schema_too_new).
//
// On Windows the state directory carries a protected DACL with inheritable
// owner and SYSTEM ACEs (statedir_windows.go), so the -wal, -shm, backups/ and
// backup files that SQLite and sqlmigrate create inherit a private ACL.
//
// Files of this package:
//
//   - db.go: [Open], [Store], connection settings, owner-only file creation;
//   - statedir_windows.go, statedir_other.go: the owner-only state directory ACL (no-op off Windows);
//   - migrate.go: the embedded Worker migrations and the sqlmigrate wiring;
//   - counter.go: [Store.NextSessionNumber], the persisted minion-N counter (AD-13);
//   - share.go: [ShareMode], [ShareSelection] and [Store.IsShared] (R75);
//   - results.go: the 10-minute write result cache for duplicate request ids ([Store.PutResult], [Store.GetResult], AD-15);
//   - audit.go: the local audit of write ops, without content ([Store.AppendAudit], AD-20);
//   - enrollment.go: the pinned Hub key, its chain position and the welcome data ([Store.SaveEnrollment], [Store.UpdatePin], [Store.WipeEnrollment], AD-5, AD-9).
//
// Later stories add their tables (pipe log) as
// further files and migrations of this package; only this package contains
// SQL for worker.db.
//
// Owner: Story 2.3; results.go, audit.go and migration 0003: Story 2.6; enrollment.go and migration 0005: Story 4.4.
package store
