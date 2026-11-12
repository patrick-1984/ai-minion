package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
)

// OpenReadOnlyContext opens an existing Worker database for status queries.
// It never creates the state directory or database, changes permissions or
// journal mode, or runs migrations. A database being initialized or upgraded
// may not yet have the tables a status query needs; callers report those read
// errors and leave initialization to the Worker.
func OpenReadOnlyContext(ctx context.Context, dir string) (*Store, error) {
	if dir == "" {
		return nil, errors.New("store: state directory is empty")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("store: state directory %q: %w", dir, err)
	}
	u := databaseURL(filepath.Join(abs, DBFile))
	q := url.Values{}
	q.Set("mode", "ro")
	q.Add("_pragma", "query_only(ON)")
	q.Add("_pragma", fmt.Sprintf("busy_timeout(%d)", busyTimeoutMS))
	u.RawQuery = q.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, fmt.Errorf("store: open read-only database: %w", err)
	}
	// Ping alone need not read any database pages. Validate the file without
	// writing so a corrupt database is reported at the open boundary.
	var version int
	if err := db.QueryRowContext(ctx, "PRAGMA schema_version").Scan(&version); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: open read-only database: %w", err)
	}
	return &Store{db: db, dir: abs}, nil
}
