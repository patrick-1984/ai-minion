// Package store owns the Hub SQLite database, its migrations and the column encryption (AD-16). It is the only Hub package that imports the SQLite driver.
//
// [Open] opens <data>/minion.db as one *sql.DB (WAL, busy_timeout=5000,
// _txlock=immediate, foreign keys on), creates <data>/data.key on first start,
// and applies the embedded migrations (migrations/NNNN_name.sql) through
// internal/sqlmigrate: forward-only, in one transaction, after a VACUUM INTO
// backup in <data>/backups, refusing a newer schema with schema_too_new.
//
// Files of this package:
//
//   - db.go: [Open], [Store], connection settings;
//   - migrate.go: the embedded Hub migrations;
//   - tx.go: [Tx], one immediate transaction with rollback on error or panic;
//   - ids.go: UUIDv7 identifiers and unix-millisecond helpers;
//   - secretbox.go: AES-256-GCM column encryption bound to table, column and row.
//
// Each later story adds its table file (for example audit.go) and its
// migration under migrations/; only this package contains SQL.
//
// Owner: Story 3.1.
package store
