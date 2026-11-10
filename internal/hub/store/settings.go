package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// Names that bind a sealed setting value to its place (AD-16).
const (
	settingsTable     = "settings"
	settingsSecretCol = "value_enc"
)

// SettingRecord is one row of the settings table, with a secret value already
// decrypted. It is the only place where the plaintext of a secret setting
// exists outside the caller; callers must never log it. Printing and logging
// a record redacts a secret value.
type SettingRecord struct {
	Key       string
	Value     string
	Secret    bool // the value is stored encrypted
	UpdatedAt time.Time
	UpdatedBy string
}

// redactedValue is what printing shows in place of a secret value. It matches
// config.Redacted; it is a local constant so that this package needs no
// import for it.
const redactedValue = "[redacted]"

func (r SettingRecord) shownValue() string {
	if r.Secret && r.Value != "" {
		return redactedValue
	}
	return r.Value
}

// String implements fmt.Stringer; a secret value is redacted.
func (r SettingRecord) String() string {
	return fmt.Sprintf("setting{key=%s value=%s secret=%t updated_at=%s updated_by=%s}",
		r.Key, r.shownValue(), r.Secret, r.UpdatedAt.Format(time.RFC3339), r.UpdatedBy)
}

// GoString implements fmt.GoStringer, so that %#v redacts too.
func (r SettingRecord) GoString() string { return r.String() }

// LogValue implements slog.LogValuer; a secret value is redacted.
func (r SettingRecord) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("key", r.Key),
		slog.String("value", r.shownValue()),
		slog.Bool("secret", r.Secret),
		slog.Time("updated_at", r.UpdatedAt),
		slog.String("updated_by", r.UpdatedBy),
	)
}

// GetSetting returns the panel value stored under key; ok is false when the
// panel has not set the key. A secret value is decrypted.
func (s *Store) GetSetting(ctx context.Context, key string) (rec SettingRecord, ok bool, err error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT key, value, value_enc, updated_at, updated_by FROM settings WHERE key = ?`, key)
	rec, err = s.scanSetting(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return SettingRecord{}, false, nil
	}
	if err != nil {
		return SettingRecord{}, false, fmt.Errorf("store: read setting %q: %w", key, err)
	}
	return rec, true, nil
}

// ListSettings returns every stored setting ordered by key, secrets decrypted.
func (s *Store) ListSettings(ctx context.Context) ([]SettingRecord, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT key, value, value_enc, updated_at, updated_by FROM settings ORDER BY key`)
	if err != nil {
		return nil, fmt.Errorf("store: list settings: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []SettingRecord
	for rows.Next() {
		rec, err := s.scanSetting(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("store: list settings: %w", err)
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list settings: %w", err)
	}
	return out, nil
}

// PutSetting stores value under key, replacing an earlier value. A secret is
// encrypted with the data key and never written as text. updatedBy is the
// actor id, kept for the panel; it is not the audit record.
func (s *Store) PutSetting(ctx context.Context, key, value string, secret bool, updatedBy string) error {
	if key == "" {
		return errors.New("store: setting key is empty")
	}
	var text any = value
	var sealed any
	if secret {
		b, err := s.box.Seal(settingsTable, settingsSecretCol, key, []byte(value))
		if err != nil {
			return fmt.Errorf("store: seal setting %q: %w", key, err)
		}
		text, sealed = nil, b
	}
	_, err := s.db.ExecContext(ctx, `
INSERT INTO settings (key, value, value_enc, updated_at, updated_by) VALUES (?, ?, ?, ?, ?)
ON CONFLICT(key) DO UPDATE SET
    value = excluded.value, value_enc = excluded.value_enc,
    updated_at = excluded.updated_at, updated_by = excluded.updated_by`,
		key, text, sealed, UnixMS(time.Now()), updatedBy)
	if err != nil {
		return fmt.Errorf("store: write setting %q: %w", key, err)
	}
	return nil
}

// DeleteSetting removes the panel value of key, so that the default applies
// again. It reports whether a row existed.
func (s *Store) DeleteSetting(ctx context.Context, key string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM settings WHERE key = ?`, key)
	if err != nil {
		return false, fmt.Errorf("store: delete setting %q: %w", key, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("store: delete setting %q: %w", key, err)
	}
	return n > 0, nil
}

func (s *Store) scanSetting(scan func(dest ...any) error) (SettingRecord, error) {
	var (
		rec     SettingRecord
		text    sql.NullString
		sealed  []byte
		updated int64
	)
	if err := scan(&rec.Key, &text, &sealed, &updated, &rec.UpdatedBy); err != nil {
		return SettingRecord{}, err
	}
	rec.UpdatedAt = FromUnixMS(updated)
	if sealed != nil {
		plain, err := s.box.Open(settingsTable, settingsSecretCol, rec.Key, sealed)
		if err != nil {
			return SettingRecord{}, fmt.Errorf("setting %q: %w", rec.Key, err)
		}
		rec.Value, rec.Secret = string(plain), true
		return rec, nil
	}
	rec.Value = text.String
	return rec, nil
}
