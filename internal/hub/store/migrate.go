package store

import (
	"context"
	"database/sql"
	"embed"
	"io/fs"
	"path/filepath"

	"github.com/patrick-1984/ai-minion/internal/sqlmigrate"
)

// hubMigrations holds the Hub migrations, migrations/NNNN_name.sql. The
// "all:" prefix embeds the .gitkeep that keeps the directory in git before
// the first story adds a migration; the runner ignores non-SQL files.
//
//go:embed all:migrations
var hubMigrations embed.FS

const (
	migrationsDir = "migrations"
	backupDirName = "backups"
	backupPrefix  = "minion"
)

// migrationSet names where migrations come from; tests replace the embedded
// Hub set with their own.
type migrationSet struct {
	fsys fs.FS
	dir  string
}

func embeddedMigrations() migrationSet {
	return migrationSet{fsys: hubMigrations, dir: migrationsDir}
}

// migrate applies the pending migrations of set to db. Backups go to
// <dataDir>/backups/minion-<timestamp>.db.
func migrate(ctx context.Context, db *sql.DB, dataDir string, set migrationSet, clock Clock) (sqlmigrate.Result, error) {
	return sqlmigrate.Apply(ctx, db, sqlmigrate.Options{
		FS:           set.fsys,
		Dir:          set.dir,
		BackupDir:    backupDirOf(dataDir),
		BackupPrefix: backupPrefix,
		Now:          clock,
	})
}

// backupDirOf is the pre-migration backup directory of a data directory; an
// empty data directory (a restore staging copy) takes no backup.
func backupDirOf(dataDir string) string {
	if dataDir == "" {
		return ""
	}
	return filepath.Join(dataDir, backupDirName)
}
