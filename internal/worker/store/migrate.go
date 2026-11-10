package store

import (
	"context"
	"database/sql"
	"embed"
	"io/fs"
	"path/filepath"

	"github.com/patrick-1984/ai-minion/internal/sqlmigrate"
)

// workerMigrations holds the Worker migrations, migrations/NNNN_name.sql.
//
//go:embed migrations/*.sql
var workerMigrations embed.FS

const (
	migrationsDir = "migrations"
	backupDirName = "backups"
	backupPrefix  = "worker"
)

// migrationSet names where migrations come from; tests replace the embedded
// Worker set with their own.
type migrationSet struct {
	fsys fs.FS
	dir  string
}

func embeddedMigrations() migrationSet {
	return migrationSet{fsys: workerMigrations, dir: migrationsDir}
}

// migrate applies the pending migrations of set to db through sqlmigrate.
// Backups go to <stateDir>/backups/worker-<timestamp>.db.
func migrate(ctx context.Context, db *sql.DB, stateDir string, set migrationSet, clock Clock) (sqlmigrate.Result, error) {
	return sqlmigrate.Apply(ctx, db, sqlmigrate.Options{
		FS:           set.fsys,
		Dir:          set.dir,
		BackupDir:    filepath.Join(stateDir, backupDirName),
		BackupPrefix: backupPrefix,
		Now:          clock,
	})
}
