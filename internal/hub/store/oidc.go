package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
)

// Names that bind sealed OIDC values to their place (AD-16).
const (
	oidcProvidersTable  = "oidc_providers"
	oidcSecretColumn    = "client_secret"
	identitiesTable     = "external_identities"
	identitiesTokenCol  = "refresh_token"
	loginTxTable        = "oidc_login_transactions"
	loginTxVerifierCol  = "code_verifier"
	roleAdmin           = "admin"
	roleUser            = "user"
	maxUsernameAttempts = 50
)

// ErrIdentityConflict is returned when an identity cannot be created because
// no free user name was found.
var ErrIdentityConflict = errors.New("store: no free user name for the new OIDC account")

// OIDCProviderRow is one row of oidc_providers with the client secret already
// decrypted. ClientSecret is a secret: callers must never log it, and
// printing or logging a row redacts it.
type OIDCProviderRow struct {
	ID                 string
	Name               string
	Issuer             string
	ClientID           string
	ClientSecret       string // "" when the provider has none
	RoleClaim          string
	Scopes             string // space separated; "" means the default scopes
	CAFile             string
	InsecureSkipVerify bool
	Enabled            bool
	CreatedAt          int64 // unix milliseconds
	UpdatedAt          int64
}

func (r OIDCProviderRow) shownSecret() string {
	if r.ClientSecret != "" {
		return redactedValue
	}
	return ""
}

// String implements fmt.Stringer; the client secret is redacted.
func (r OIDCProviderRow) String() string {
	return fmt.Sprintf("oidc_provider{id=%s issuer=%s client_id=%s client_secret=%s}", r.ID, r.Issuer, r.ClientID, r.shownSecret())
}

// GoString implements fmt.GoStringer, so that %#v redacts too.
func (r OIDCProviderRow) GoString() string { return r.String() }

// LogValue implements slog.LogValuer; the client secret is redacted.
func (r OIDCProviderRow) LogValue() slog.Value {
	return slog.GroupValue(slog.String("id", r.ID), slog.String("issuer", r.Issuer),
		slog.String("client_id", r.ClientID), slog.String("client_secret", r.shownSecret()))
}

// RoleRuleRow is one claim value of a provider's rule for a role.
type RoleRuleRow struct {
	ProviderID string
	Role       string // "admin" or "user"
	Value      string
}

// OIDCLoginTx is a started sign-in, awaiting its callback. StateHash and
// BindingHash are the SHA-256 (hex) of the state parameter and of the
// browser-binding cookie value; HashValue computes them.
type OIDCLoginTx struct {
	StateHash    string
	BindingHash  string
	ProviderID   string
	Nonce        string
	CodeVerifier string
	NextPath     string
	CreatedAt    int64
	ExpiresAt    int64
}

// HashValue returns the lower-case hex SHA-256 of v, the form in which the
// state parameter and the binding cookie are stored.
func HashValue(v string) string {
	sum := sha256.Sum256([]byte(v))
	return hex.EncodeToString(sum[:])
}

// ListOIDCProviders returns the providers set in the panel, ordered by id,
// with their secrets decrypted.
func (s *Store) ListOIDCProviders(ctx context.Context) ([]OIDCProviderRow, error) {
	rows, err := s.db.QueryContext(ctx, providerSelect+` ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("store: list OIDC providers: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []OIDCProviderRow
	for rows.Next() {
		p, err := s.scanProvider(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list OIDC providers: %w", err)
	}
	return out, nil
}

// GetOIDCProvider returns one provider, or [ErrNotFound].
func (s *Store) GetOIDCProvider(ctx context.Context, id string) (OIDCProviderRow, error) {
	p, err := s.scanProvider(s.db.QueryRowContext(ctx, providerSelect+` WHERE id = ?`, id).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return OIDCProviderRow{}, ErrNotFound
	}
	return p, err
}

const providerSelect = `SELECT id, name, issuer, client_id, client_secret_enc, role_claim, scopes, ca_file,
    tls_insecure_skip_verify, enabled, created_at, updated_at FROM oidc_providers`

func (s *Store) scanProvider(scan func(...any) error) (OIDCProviderRow, error) {
	var (
		p             OIDCProviderRow
		sealed        []byte
		insecure, ena int
	)
	if err := scan(&p.ID, &p.Name, &p.Issuer, &p.ClientID, &sealed, &p.RoleClaim, &p.Scopes, &p.CAFile,
		&insecure, &ena, &p.CreatedAt, &p.UpdatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return OIDCProviderRow{}, err
		}
		return OIDCProviderRow{}, fmt.Errorf("store: read OIDC provider: %w", err)
	}
	p.InsecureSkipVerify, p.Enabled = insecure != 0, ena != 0
	if len(sealed) > 0 {
		plain, err := s.box.Open(oidcProvidersTable, oidcSecretColumn, p.ID, sealed)
		if err != nil {
			return OIDCProviderRow{}, fmt.Errorf("store: OIDC provider %q client secret: %w", p.ID, err)
		}
		p.ClientSecret = string(plain)
	}
	return p, nil
}

// PutOIDCProvider creates or replaces a provider. When p.ClientSecret is empty
// the stored secret is kept (a new provider then has none); otherwise it is
// sealed and replaces it. CreatedAt is kept for an existing provider.
func (s *Store) PutOIDCProvider(ctx context.Context, p OIDCProviderRow) error {
	if p.ID == "" || p.Issuer == "" || p.ClientID == "" || p.Name == "" {
		return errors.New("store: an OIDC provider needs an id, a name, an issuer and a client id")
	}
	var sealed []byte
	if p.ClientSecret != "" {
		var err error
		if sealed, err = s.box.Seal(oidcProvidersTable, oidcSecretColumn, p.ID, []byte(p.ClientSecret)); err != nil {
			return err
		}
	}
	return s.Tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
UPDATE oidc_providers SET name = ?, issuer = ?, client_id = ?, role_claim = ?, scopes = ?, ca_file = ?,
    tls_insecure_skip_verify = ?, enabled = ?, updated_at = ?,
    client_secret_enc = CASE WHEN ? THEN ? ELSE client_secret_enc END
WHERE id = ?`,
			p.Name, p.Issuer, p.ClientID, p.RoleClaim, p.Scopes, p.CAFile,
			boolInt(p.InsecureSkipVerify), boolInt(p.Enabled), p.UpdatedAt,
			sealed != nil, sealed, p.ID)
		if err != nil {
			return fmt.Errorf("store: update OIDC provider: %w", err)
		}
		if n, _ := res.RowsAffected(); n > 0 {
			return nil
		}
		_, err = tx.ExecContext(ctx, `
INSERT INTO oidc_providers (id, name, issuer, client_id, client_secret_enc, role_claim, scopes, ca_file,
    tls_insecure_skip_verify, enabled, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			p.ID, p.Name, p.Issuer, p.ClientID, sealed, p.RoleClaim, p.Scopes, p.CAFile,
			boolInt(p.InsecureSkipVerify), boolInt(p.Enabled), p.CreatedAt, p.UpdatedAt)
		if err != nil {
			return fmt.Errorf("store: insert OIDC provider: %w", err)
		}
		return nil
	})
}

// DeleteOIDCProvider removes a provider and its role rules. It reports whether
// the provider existed. Linked identities stay (their accounts keep working
// with a local password, and the identity is reusable if the provider returns).
func (s *Store) DeleteOIDCProvider(ctx context.Context, id string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM oidc_providers WHERE id = ?`, id)
	if err != nil {
		return false, fmt.Errorf("store: delete OIDC provider: %w", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// ListRoleRules returns the rules of one provider, or of every provider when
// providerID is empty, ordered by provider, role and value.
func (s *Store) ListRoleRules(ctx context.Context, providerID string) ([]RoleRuleRow, error) {
	q, args := `SELECT provider_id, role, value FROM role_rules`, []any(nil)
	if providerID != "" {
		q, args = q+` WHERE provider_id = ?`, []any{providerID}
	}
	rows, err := s.db.QueryContext(ctx, q+` ORDER BY provider_id, role, value`, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list role rules: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []RoleRuleRow
	for rows.Next() {
		var r RoleRuleRow
		if err := rows.Scan(&r.ProviderID, &r.Role, &r.Value); err != nil {
			return nil, fmt.Errorf("store: read role rule: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ReplaceRoleRules replaces the claim values of role for a provider in one
// transaction. Empty values are dropped and duplicates collapse; no value at
// all removes the rule. It returns [ErrNotFound] for an unknown provider.
func (s *Store) ReplaceRoleRules(ctx context.Context, providerID, role string, values []string) error {
	if role != roleAdmin && role != roleUser {
		return fmt.Errorf("store: unknown role %q", role)
	}
	return s.Tx(ctx, func(tx *sql.Tx) error {
		var one int
		if err := tx.QueryRowContext(ctx, `SELECT 1 FROM oidc_providers WHERE id = ?`, providerID).Scan(&one); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return fmt.Errorf("store: role rules: read provider: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM role_rules WHERE provider_id = ? AND role = ?`, providerID, role); err != nil {
			return fmt.Errorf("store: role rules: clear: %w", err)
		}
		for _, v := range values {
			if v == "" {
				continue
			}
			if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO role_rules (provider_id, role, value) VALUES (?, ?, ?)`, providerID, role, v); err != nil {
				return fmt.Errorf("store: role rules: insert: %w", err)
			}
		}
		return nil
	})
}

// CreateOIDCLoginTx stores a started sign-in and deletes the expired ones.
func (s *Store) CreateOIDCLoginTx(ctx context.Context, t OIDCLoginTx, nowMS int64) error {
	sealed, err := s.box.Seal(loginTxTable, loginTxVerifierCol, t.StateHash, []byte(t.CodeVerifier))
	if err != nil {
		return err
	}
	return s.Tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM oidc_login_transactions WHERE expires_at <= ?`, nowMS); err != nil {
			return fmt.Errorf("store: purge OIDC sign-ins: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
INSERT INTO oidc_login_transactions (state_hash, binding_hash, provider_id, nonce, code_verifier_enc, next_path, created_at, expires_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			t.StateHash, t.BindingHash, t.ProviderID, t.Nonce, sealed, t.NextPath, t.CreatedAt, t.ExpiresAt); err != nil {
			return fmt.Errorf("store: insert OIDC sign-in: %w", err)
		}
		return nil
	})
}

// ConsumeOIDCLoginTx deletes and returns the sign-in with the given state
// hash, so that a state is usable once. It returns [ErrNotFound] for an
// unknown, used or expired state (an expired row is deleted too).
func (s *Store) ConsumeOIDCLoginTx(ctx context.Context, stateHash string, nowMS int64) (OIDCLoginTx, error) {
	var (
		t      OIDCLoginTx
		sealed []byte
	)
	err := s.Tx(ctx, func(tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx, `
SELECT state_hash, binding_hash, provider_id, nonce, code_verifier_enc, next_path, created_at, expires_at
FROM oidc_login_transactions WHERE state_hash = ?`, stateHash).
			Scan(&t.StateHash, &t.BindingHash, &t.ProviderID, &t.Nonce, &sealed, &t.NextPath, &t.CreatedAt, &t.ExpiresAt)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("store: read OIDC sign-in: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM oidc_login_transactions WHERE state_hash = ?`, stateHash); err != nil {
			return fmt.Errorf("store: delete OIDC sign-in: %w", err)
		}
		return nil
	})
	if err != nil {
		return OIDCLoginTx{}, err
	}
	if t.ExpiresAt <= nowMS {
		return OIDCLoginTx{}, ErrNotFound
	}
	plain, err := s.box.Open(loginTxTable, loginTxVerifierCol, t.StateHash, sealed)
	if err != nil {
		return OIDCLoginTx{}, fmt.Errorf("store: OIDC sign-in verifier: %w", err)
	}
	t.CodeVerifier = string(plain)
	return t, nil
}

// OIDCSyncParams describes one successful IdP sign-in for [Store.SyncOIDCLogin].
type OIDCSyncParams struct {
	// Issuer and Subject are the identity of the account at the IdP.
	Issuer, Subject string
	// ProviderID is the provider the sign-in went through.
	ProviderID string
	// PreferredUsername is the suggested user name for a new account; it is
	// sanitised and made unique.
	PreferredUsername string
	// Roles are the IdP roles ("admin", "user") the claim rules gave. They
	// replace the source-idp rows of the account.
	Roles []string
	// CreateIfMissing allows a new account for an unknown identity. It is
	// false when no rule matched: such a sign-in must create nothing.
	CreateIfMissing bool
	// RefreshToken, when not empty, replaces the stored refresh token.
	RefreshToken string
	// NowMS is the sign-in time.
	NowMS int64
	// OnRolesChanged, when not nil, runs inside the transaction after the
	// roles were replaced, and only when they changed (a new account
	// included). The caller writes the required role-change audit row with
	// the given Execer, so the row commits with the change it describes; an
	// error rolls the whole sync back.
	OnRolesChanged func(ctx context.Context, ex Execer, res OIDCSyncResult) error
}

// OIDCSyncResult is the outcome of [Store.SyncOIDCLogin].
type OIDCSyncResult struct {
	// Found is false when the identity is unknown and none was created.
	Found bool
	// Created is true when the sign-in created the account.
	Created bool
	// Inactive is true when the account is deactivated; nothing was changed.
	Inactive bool
	// User is the account after the sync (its Role is the effective role).
	User UserRow
	// RolesBefore and RolesAfter are the source-idp roles, sorted.
	RolesBefore, RolesAfter []string
	// SessionsEnded is the number of browser sessions the sync deleted. It is
	// not zero only for a known, active account whose sign-in matched no rule
	// (AD-10: no matching rule means no access, so the earlier sessions end
	// with this sign-in).
	SessionsEnded int64
}

// RolesChanged reports whether the sync changed the account's IdP roles.
func (r OIDCSyncResult) RolesChanged() bool { return !slices.Equal(r.RolesBefore, r.RolesAfter) }

// SyncOIDCLogin applies a successful IdP sign-in in one transaction: it finds
// the account of (issuer, subject) or, when allowed, creates it, replaces its
// source-idp roles with p.Roles and stores the refresh token. Rows with source
// local are never touched, and a demotion applies even to the last admin
// (AD-10). A deactivated account is left unchanged (Inactive). When the
// account is known and p.Roles is empty, its browser sessions are deleted in
// the same transaction (SessionsEnded) and its credential_version is raised,
// so a session issued from an older snapshot of the account fails validation;
// local grants stay.
func (s *Store) SyncOIDCLogin(ctx context.Context, p OIDCSyncParams) (OIDCSyncResult, error) {
	if p.Issuer == "" || p.Subject == "" {
		return OIDCSyncResult{}, errors.New("store: an OIDC identity needs an issuer and a subject")
	}
	roles, err := normalizeRoles(p.Roles)
	if err != nil {
		return OIDCSyncResult{}, err
	}
	var res OIDCSyncResult
	err = s.Tx(ctx, func(tx *sql.Tx) error {
		res = OIDCSyncResult{}
		var identityID, userID string
		err := tx.QueryRowContext(ctx, `SELECT id, user_id FROM external_identities WHERE issuer = ? AND subject = ?`,
			p.Issuer, p.Subject).Scan(&identityID, &userID)
		switch {
		case err == nil:
			res.Found = true
		case errors.Is(err, sql.ErrNoRows):
			if !p.CreateIfMissing {
				return nil
			}
			if identityID, userID, err = s.createOIDCAccount(ctx, tx, p); err != nil {
				return err
			}
			res.Found, res.Created = true, true
		default:
			return fmt.Errorf("store: read external identity: %w", err)
		}

		user, err := queryUserTx(ctx, tx, userID)
		if err != nil {
			return err
		}
		res.User = user
		if !user.Active {
			res.Inactive = true
			return nil
		}
		if res.RolesBefore, res.RolesAfter, err = replaceIDPRolesTx(ctx, tx, userID, roles); err != nil {
			return err
		}
		if p.RefreshToken != "" {
			sealed, err := s.box.Seal(identitiesTable, identitiesTokenCol, identityID, []byte(p.RefreshToken))
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE external_identities SET refresh_token_enc = ? WHERE id = ?`, sealed, identityID); err != nil {
				return fmt.Errorf("store: store refresh token: %w", err)
			}
		}
		// last_login_at doubles as the revision of the identity that
		// ApplyOIDCRefresh compares. It must change on every sign-in whatever
		// the clock resolution or a clock step back, so it never goes below
		// the previous value plus one. A row this sign-in just created has no
		// earlier revision and keeps the sign-in time.
		step := 1
		if res.Created {
			step = 0
		}
		if _, err := tx.ExecContext(ctx, `UPDATE external_identities SET last_login_at = MAX(?, last_login_at + ?), provider_id = ? WHERE id = ?`,
			p.NowMS, step, p.ProviderID, identityID); err != nil {
			return fmt.Errorf("store: update external identity: %w", err)
		}
		if len(roles) == 0 {
			// No matching rule means no access: the sessions of earlier
			// sign-ins must not outlive the group membership that gave them.
			r, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ?`, userID)
			if err != nil {
				return fmt.Errorf("store: end sessions: %w", err)
			}
			res.SessionsEnded, _ = r.RowsAffected()
			// A callback that read the account before this sync may still be
			// about to issue a session from its stale snapshot. The version
			// bump makes such a session fail validation, so a concurrent
			// sign-in cannot restore access after the revocation.
			if _, err := tx.ExecContext(ctx, `UPDATE users SET credential_version = credential_version + 1, updated_at = ? WHERE id = ?`,
				p.NowMS, userID); err != nil {
				return fmt.Errorf("store: revoke stale sessions: %w", err)
			}
		}
		// The effective role may have changed with the roles.
		if res.User, err = queryUserTx(ctx, tx, userID); err != nil {
			return err
		}
		if p.OnRolesChanged != nil && res.RolesChanged() {
			if err := p.OnRolesChanged(ctx, tx, res); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return OIDCSyncResult{}, err
	}
	return res, nil
}

// ReplaceIDPRoles replaces the source-idp roles of a user in one transaction,
// leaving local grants alone, and returns the roles before and after (sorted).
// A background refresh (Story 6.3) uses it.
func (s *Store) ReplaceIDPRoles(ctx context.Context, userID string, roles []string) (before, after []string, err error) {
	norm, err := normalizeRoles(roles)
	if err != nil {
		return nil, nil, err
	}
	err = s.Tx(ctx, func(tx *sql.Tx) error {
		var err error
		before, after, err = replaceIDPRolesTx(ctx, tx, userID, norm)
		return err
	})
	return before, after, err
}

func normalizeRoles(roles []string) ([]string, error) {
	var out []string
	for _, r := range roles {
		if r != roleAdmin && r != roleUser {
			return nil, fmt.Errorf("store: unknown role %q", r)
		}
		if !slices.Contains(out, r) {
			out = append(out, r)
		}
	}
	slices.Sort(out)
	return out, nil
}

func replaceIDPRolesTx(ctx context.Context, tx *sql.Tx, userID string, roles []string) (before, after []string, err error) {
	rows, err := tx.QueryContext(ctx, `SELECT role FROM user_roles WHERE user_id = ? AND source = 'idp' ORDER BY role`, userID)
	if err != nil {
		return nil, nil, fmt.Errorf("store: read idp roles: %w", err)
	}
	for rows.Next() {
		var r string
		if err := rows.Scan(&r); err != nil {
			_ = rows.Close()
			return nil, nil, fmt.Errorf("store: read idp roles: %w", err)
		}
		before = append(before, r)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, nil, fmt.Errorf("store: read idp roles: %w", err)
	}
	_ = rows.Close()
	if _, err := tx.ExecContext(ctx, `DELETE FROM user_roles WHERE user_id = ? AND source = 'idp'`, userID); err != nil {
		return nil, nil, fmt.Errorf("store: clear idp roles: %w", err)
	}
	for _, r := range roles {
		if _, err := tx.ExecContext(ctx, `INSERT INTO user_roles (user_id, role, source) VALUES (?, ?, 'idp')`, userID, r); err != nil {
			return nil, nil, fmt.Errorf("store: grant idp role: %w", err)
		}
	}
	return before, slices.Clone(roles), nil
}

func queryUserTx(ctx context.Context, tx *sql.Tx, id string) (UserRow, error) {
	var sc userScan
	err := tx.QueryRowContext(ctx, `SELECT `+userColumns+` FROM users u WHERE u.id = ?`, id).Scan(sc.dest()...)
	if errors.Is(err, sql.ErrNoRows) {
		return UserRow{}, ErrNotFound
	}
	if err != nil {
		return UserRow{}, fmt.Errorf("store: read user: %w", err)
	}
	return sc.row(), nil
}

// createOIDCAccount inserts the user and its external identity inside tx.
func (s *Store) createOIDCAccount(ctx context.Context, tx *sql.Tx, p OIDCSyncParams) (identityID, userID string, err error) {
	userID, identityID = NewID(), NewID()
	base := sanitizeUsername(p.PreferredUsername)
	if base == "" {
		sum := sha256.Sum256([]byte(p.Issuer + "\x00" + p.Subject))
		base = "oidc-" + hex.EncodeToString(sum[:4])
	}
	name := ""
	for i := 1; i <= maxUsernameAttempts; i++ {
		cand := base
		if i > 1 {
			cand = fmt.Sprintf("%s-%d", base, i)
		}
		var one int
		err := tx.QueryRowContext(ctx, `SELECT 1 FROM users WHERE username = ?`, cand).Scan(&one)
		if errors.Is(err, sql.ErrNoRows) {
			name = cand
			break
		}
		if err != nil {
			return "", "", fmt.Errorf("store: check user name: %w", err)
		}
	}
	if name == "" {
		return "", "", ErrIdentityConflict
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO users (id, username, email, password_hash, must_change_password, credential_version, active, created_at, updated_at)
VALUES (?, ?, '', '', 0, 1, 1, ?, ?)`, userID, name, p.NowMS, p.NowMS); err != nil {
		return "", "", fmt.Errorf("store: create OIDC user: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO external_identities (id, user_id, issuer, subject, provider_id, created_at, last_login_at)
VALUES (?, ?, ?, ?, ?, ?, ?)`, identityID, userID, p.Issuer, p.Subject, p.ProviderID, p.NowMS, p.NowMS); err != nil {
		return "", "", fmt.Errorf("store: create external identity: %w", err)
	}
	return identityID, userID, nil
}

// sanitizeUsername keeps the characters that are safe in a user name and
// limits the length; it returns "" when nothing usable is left.
func sanitizeUsername(s string) string {
	var b strings.Builder
	for _, r := range strings.TrimSpace(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-', r == '@', r == '+':
			b.WriteRune(r)
		}
		if b.Len() >= 64 {
			break
		}
	}
	return b.String()
}
