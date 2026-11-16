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
	"strings"
	"unicode"
)

// maxWorkerNameLen bounds a stored Worker name (config.MaxNameLength).
const maxWorkerNameLen = 64

// OwnerSlug turns a username into the prefix of its Workers' names.
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
	return strings.Trim(b.String(), "-")
}

// StandardWorkerName prefixes name with slug unless it already starts with
// it ("slug" or "slug-..."), and keeps the result within the length limit.
func StandardWorkerName(slug, name string) string {
	name = strings.Trim(name, "-")
	if slug == "" {
		return name
	}
	lower := strings.ToLower(name)
	if lower == slug || strings.HasPrefix(lower, slug+"-") {
		return name
	}
	if name == "" {
		return slug
	}
	out := slug + "-" + name
	if r := []rune(out); len(r) > maxWorkerNameLen {
		out = strings.TrimRight(string(r[:maxWorkerNameLen]), "-")
	}
	return out
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
