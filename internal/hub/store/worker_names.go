package store

// Worker names start with their owner (v0.9.1, A20): "<owner>-<name>", where
// <owner> is the owner's username without an e-mail domain, in lower case,
// with dots, underscores and spaces as dashes ("patrick.romatowski@x.com" →
// "patrick-romatowski"). The rest is what the Worker asked for (its short
// host name by default). A name that already starts with the prefix keeps it.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/patrick-1984/ai-minion/internal/ops"
)

// maxWorkerNameLen bounds a stored Worker name in bytes (the target parser's
// limit, ops.MaxWorkerNameLen).
const maxWorkerNameLen = ops.MaxWorkerNameLen

// cutBytes cuts s to at most n bytes at a UTF-8 boundary.
func cutBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// maxSlugLen leaves room for the Worker's own part of the name.
const maxSlugLen = 32

// OwnerSlug turns a username into the prefix of its Workers' names: letters
// and digits in lower case, runs of anything else as one dash, at most
// maxSlugLen bytes. It is "" when nothing is left.
func OwnerSlug(username string) string {
	u := strings.TrimSpace(username)
	if i := strings.IndexByte(u, '@'); i > 0 {
		u = u[:i]
	}
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(u) {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
			dash = false
		case !dash && b.Len() > 0:
			b.WriteByte('-')
			dash = true
		}
	}
	return strings.Trim(cutBytes(strings.Trim(b.String(), "-"), maxSlugLen), "-")
}

// cutPrefixFold returns name without "slug-" when name starts with it in any
// letter case, comparing rune by rune in the original string.
func cutPrefixFold(name, slug string) (string, bool) {
	rest := name
	for _, sr := range slug {
		r, size := utf8.DecodeRuneInString(rest)
		if size == 0 || unicode.ToLower(r) != sr {
			return name, false
		}
		rest = rest[size:]
	}
	if rest == "" {
		return "", true
	}
	if rest[0] != '-' {
		return name, false
	}
	return rest[1:], true
}

// StandardWorkerName prefixes name with slug unless it already starts with
// it ("slug" or "slug-..."). The result is always a valid Worker name of at
// most maxWorkerNameLen bytes ("worker" stands in for an empty name).
func StandardWorkerName(slug, name string) string {
	name = strings.Trim(strings.TrimSpace(name), "-")
	if name == "" {
		name = "worker"
	}
	out := name
	if slug != "" {
		if _, ok := cutPrefixFold(name, slug); !ok {
			out = slug + "-" + name
		}
	}
	out = strings.Trim(cutBytes(out, maxWorkerNameLen), "-")
	if !ops.ValidWorkerName(out) {
		out = strings.Trim(cutBytes(slug+"-worker", maxWorkerNameLen), "-")
		if !ops.ValidWorkerName(out) {
			out = "worker"
		}
	}
	return out
}

// RenameWorkerOwner swaps the owner prefix of name from oldSlug to newSlug.
func RenameWorkerOwner(name, oldSlug, newSlug string) string {
	rest := name
	if oldSlug != "" {
		if r, ok := cutPrefixFold(name, oldSlug); ok {
			rest = r
		}
	}
	return StandardWorkerName(newSlug, rest)
}

// workerNamesMarker is the auth_meta key that records the one-time rename of
// the Workers that existed before names started with their owner.
const workerNamesMarker = "worker_names_owner_prefix"

// normalizeWorkerNames renames, once, every Worker whose name does not start
// with its owner (the same rule as new names). A rename that would collide
// with another Worker of the owner, or would not be a valid name, is skipped.
func (s *Store) normalizeWorkerNames(ctx context.Context) error {
	return s.Tx(ctx, func(tx *sql.Tx) error {
		// A database made with other migrations (tests) may lack the tables.
		var tables int
		if err := tx.QueryRowContext(ctx,
			`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name IN ('workers', 'users', 'auth_meta')`).Scan(&tables); err != nil {
			return err
		}
		if tables != 3 {
			return nil
		}
		var done int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM auth_meta WHERE key = ?`, workerNamesMarker).Scan(&done); err != nil {
			return err
		}
		if done > 0 {
			return nil
		}
		rows, err := tx.QueryContext(ctx, `SELECT w.id, w.name, u.username FROM workers w JOIN users u ON u.id = w.owner_id`)
		if err != nil {
			return err
		}
		type ren struct{ id, name string }
		var todo []ren
		for rows.Next() {
			var id, name, user string
			if err := rows.Scan(&id, &name, &user); err != nil {
				_ = rows.Close()
				return err
			}
			if n := StandardWorkerName(OwnerSlug(user), name); n != name && ops.ValidWorkerName(n) {
				todo = append(todo, ren{id, n})
			}
		}
		_ = rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, r := range todo {
			if _, err := tx.ExecContext(ctx, `UPDATE OR IGNORE workers SET name = ? WHERE id = ?`, r.name, r.id); err != nil {
				return fmt.Errorf("store: rename worker %s: %w", r.id, err)
			}
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO auth_meta (key, value, created_at) VALUES (?, '1', 0)`, workerNamesMarker)
		return err
	})
}

// ownerSlugOf reads the owner's username through ex ("" when unknown).
func ownerSlugOf(ctx context.Context, ex Execer, ownerID string) (string, error) {
	q, ok := ex.(interface {
		QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	})
	if !ok {
		return "", nil
	}
	var username string
	err := q.QueryRowContext(ctx, `SELECT username FROM users WHERE id = ?`, ownerID).Scan(&username)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return OwnerSlug(username), nil
}

// WorkerNameFor is the standard name of a Worker of ownerID asking for name.
func (s *Store) WorkerNameFor(ctx context.Context, ownerID, name string) (string, error) {
	slug, err := ownerSlugOf(ctx, s.db, ownerID)
	if err != nil {
		return "", err
	}
	return StandardWorkerName(slug, name), nil
}
