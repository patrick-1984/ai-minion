package store

// Persistence of the OIDC background role refresh (AD-10, AD-16, Story 6.3).
// The refresher in internal/hub/auth/oidc decides; the SQL lives here.

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"slices"
)

// OIDCRefreshTarget is one account that holds a refresh token: an external
// identity of an active user.
type OIDCRefreshTarget struct {
	IdentityID string
	UserID     string
	Issuer     string
	Subject    string
	// ProviderID is the provider the account last signed in with.
	ProviderID string
	// LastLoginAt (unix milliseconds) is the revision of the identity: every
	// sign-in advances it by at least one, whether or not the sign-in changed
	// the refresh token and even when two sign-ins carry the same time or the
	// clock stepped back. It is the sign-in time only when that is later than
	// the previous revision. [Store.ApplyOIDCRefresh] applies an update only while it is
	// unchanged, so a refresh that started before a sign-in cannot overwrite
	// what that sign-in decided.
	LastLoginAt int64
	// RefreshToken is the stored token, decrypted. It is a secret: it is
	// redacted in logs and when printed.
	RefreshToken string
}

func (t OIDCRefreshTarget) shownToken() string {
	if t.RefreshToken != "" {
		return redactedValue
	}
	return ""
}

// String implements fmt.Stringer; the token is redacted.
func (t OIDCRefreshTarget) String() string {
	return fmt.Sprintf("oidc_refresh_target{identity=%s user=%s provider=%s token=%s}", t.IdentityID, t.UserID, t.ProviderID, t.shownToken())
}

// GoString implements fmt.GoStringer, so that %#v redacts too.
func (t OIDCRefreshTarget) GoString() string { return t.String() }

// LogValue implements slog.LogValuer; the token is redacted.
func (t OIDCRefreshTarget) LogValue() slog.Value {
	return slog.GroupValue(slog.String("identity", t.IdentityID), slog.String("user", t.UserID),
		slog.String("provider", t.ProviderID), slog.String("refresh_token", t.shownToken()))
}

// ListOIDCRefreshTargets returns the accounts that hold a refresh token, of
// active users only, oldest sign-in first. A token that cannot be opened (a
// damaged row) is left out and its identity id is returned in unreadable, so
// the caller can report it without the value.
func (s *Store) ListOIDCRefreshTargets(ctx context.Context) (targets []OIDCRefreshTarget, unreadable []string, err error) {
	type raw struct {
		OIDCRefreshTarget
		sealed []byte
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT e.id, e.user_id, e.issuer, e.subject, e.provider_id, e.last_login_at, e.refresh_token_enc
FROM external_identities e JOIN users u ON u.id = e.user_id
WHERE u.active = 1 AND e.refresh_token_enc IS NOT NULL AND length(e.refresh_token_enc) > 0
ORDER BY e.last_login_at, e.id`)
	if err != nil {
		return nil, nil, fmt.Errorf("store: list refresh tokens: %w", err)
	}
	var raws []raw
	for rows.Next() {
		var x raw
		if err := rows.Scan(&x.IdentityID, &x.UserID, &x.Issuer, &x.Subject, &x.ProviderID, &x.LastLoginAt, &x.sealed); err != nil {
			_ = rows.Close()
			return nil, nil, fmt.Errorf("store: list refresh tokens: %w", err)
		}
		raws = append(raws, x)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, nil, fmt.Errorf("store: list refresh tokens: %w", err)
	}
	_ = rows.Close()
	for _, x := range raws {
		plain, err := s.box.Open(identitiesTable, identitiesTokenCol, x.IdentityID, x.sealed)
		if err != nil {
			unreadable = append(unreadable, x.IdentityID)
			continue
		}
		x.RefreshToken = string(plain)
		targets = append(targets, x.OIDCRefreshTarget)
	}
	return targets, unreadable, nil
}

// OIDCRefreshUpdate is one change of a background refresh. It is applied only
// while the identity is still the one the refresh started with: the stored
// token equals ExpectedToken and the sign-in revision equals
// ExpectedLastLoginAt.
type OIDCRefreshUpdate struct {
	IdentityID string
	UserID     string
	// ExpectedToken and ExpectedLastLoginAt come from the
	// [OIDCRefreshTarget] the refresh started with. The token is compared in
	// constant time.
	ExpectedToken       string
	ExpectedLastLoginAt int64
	// ReplaceRoles replaces the source-idp roles with Roles (which may be
	// empty). It is false for an update that only stores a rotated token.
	ReplaceRoles bool
	Roles        []string
	// NewToken, when not empty, replaces the stored refresh token.
	NewToken string
	// Suspend removes the source-idp roles and the stored token (and ignores
	// ReplaceRoles, Roles and NewToken).
	Suspend bool
	// NowMS is the time of the change (unix milliseconds).
	NowMS int64
	// OnApplied, when not nil, runs inside the transaction after the change,
	// so the audit rows commit with the change they describe. An error rolls
	// the update back.
	OnApplied func(ctx context.Context, ex Execer, res OIDCRefreshResult) error
}

// OIDCRefreshResult is the outcome of [Store.ApplyOIDCRefresh].
type OIDCRefreshResult struct {
	// Applied is false when nothing was changed: the identity is gone, the
	// user was deactivated, or the identity changed since the refresh started
	// (another token, or a newer sign-in).
	Applied bool
	UserID  string
	// RolesBefore and RolesAfter are the source-idp roles, sorted. They are
	// set only when the update replaced the roles.
	RolesBefore, RolesAfter []string
	// SessionsEnded is the number of browser sessions deleted because the
	// account lost its last IdP role.
	SessionsEnded int64
}

// RolesChanged reports whether the source-idp roles changed.
func (r OIDCRefreshResult) RolesChanged() bool { return !slices.Equal(r.RolesBefore, r.RolesAfter) }

// ApplyOIDCRefresh applies one background refresh in one transaction. It
//
//   - changes nothing unless the identity still has the token and the sign-in
//     revision the refresh started with (a later sign-in wins);
//   - replaces the source-idp roles, never the local grants;
//   - seals a rotated token, or drops the token on a suspension;
//   - when the account lost its last IdP role, ends its browser sessions and
//     raises its credential_version, as a sign-in that matches no rule does.
func (s *Store) ApplyOIDCRefresh(ctx context.Context, u OIDCRefreshUpdate) (OIDCRefreshResult, error) {
	replace, roles := u.ReplaceRoles, u.Roles
	if u.Suspend {
		replace, roles = true, nil
	}
	norm, err := normalizeRoles(roles)
	if err != nil {
		return OIDCRefreshResult{}, err
	}
	var res OIDCRefreshResult
	err = s.Tx(ctx, func(tx *sql.Tx) error {
		res = OIDCRefreshResult{}
		var (
			sealed    []byte
			active    int
			lastLogin int64
		)
		err := tx.QueryRowContext(ctx, `
SELECT e.refresh_token_enc, e.last_login_at, u.active FROM external_identities e JOIN users u ON u.id = e.user_id
WHERE e.id = ? AND e.user_id = ?`, u.IdentityID, u.UserID).Scan(&sealed, &lastLogin, &active)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("store: read refresh token: %w", err)
		}
		if active != 1 || len(sealed) == 0 || lastLogin != u.ExpectedLastLoginAt {
			return nil
		}
		plain, err := s.box.Open(identitiesTable, identitiesTokenCol, u.IdentityID, sealed)
		if err != nil {
			return fmt.Errorf("store: open refresh token: %w", err)
		}
		if subtle.ConstantTimeCompare(plain, []byte(u.ExpectedToken)) != 1 {
			return nil // a sign-in stored a newer token meanwhile
		}
		res.Applied, res.UserID = true, u.UserID

		if replace {
			if res.RolesBefore, res.RolesAfter, err = replaceIDPRolesTx(ctx, tx, u.UserID, norm); err != nil {
				return err
			}
		}
		switch {
		case u.Suspend:
			if _, err := tx.ExecContext(ctx, `UPDATE external_identities SET refresh_token_enc = NULL WHERE id = ?`, u.IdentityID); err != nil {
				return fmt.Errorf("store: drop refresh token: %w", err)
			}
		case u.NewToken != "":
			sealedNew, err := s.box.Seal(identitiesTable, identitiesTokenCol, u.IdentityID, []byte(u.NewToken))
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE external_identities SET refresh_token_enc = ? WHERE id = ?`, sealedNew, u.IdentityID); err != nil {
				return fmt.Errorf("store: store refresh token: %w", err)
			}
		}
		if replace && len(res.RolesBefore) > 0 && len(res.RolesAfter) == 0 {
			// The account lost its last IdP role: the sessions that the IdP
			// roles opened end with it, as at a sign-in that matches no rule.
			// The version bump makes a session that is being issued from an
			// older snapshot fail validation.
			r, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ?`, u.UserID)
			if err != nil {
				return fmt.Errorf("store: end sessions: %w", err)
			}
			res.SessionsEnded, _ = r.RowsAffected()
			if _, err := tx.ExecContext(ctx, `UPDATE users SET credential_version = credential_version + 1, updated_at = ? WHERE id = ?`,
				u.NowMS, u.UserID); err != nil {
				return fmt.Errorf("store: revoke stale sessions: %w", err)
			}
		}
		if u.OnApplied != nil {
			return u.OnApplied(ctx, tx, res)
		}
		return nil
	})
	if err != nil {
		return OIDCRefreshResult{}, err
	}
	return res, nil
}
