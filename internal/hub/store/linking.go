package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// Errors of the account linking methods. Callers match them with errors.Is.
// ErrNotFound (users.go) reports an unknown user or identity.
var (
	// ErrIdentityTaken: the (issuer, subject) identity is already linked to an
	// account.
	ErrIdentityTaken = errors.New("store: identity is already linked")
	// ErrLinkRefused: the account no longer meets the conditions of the link
	// (it was deactivated, changed its e-mail address, already has an
	// identity, is an admin account without consent, or the provider's policy
	// no longer allows it) between the decision and the write.
	ErrLinkRefused = errors.New("store: the account cannot be linked now")
	// ErrLastSignInMethod: detaching the identity would leave the account
	// without any way to sign in (no local password and no other identity).
	ErrLastSignInMethod = errors.New("store: this is the only way to sign in to the account")
	// ErrHasLocalPassword: the account already has a local password, so a
	// reset (not a first password) applies.
	ErrHasLocalPassword = errors.New("store: user already has a local password")
	// ErrNotAdminAccount: admin link consent is only kept for admin accounts.
	ErrNotAdminAccount = errors.New("store: user is not an admin")
)

// IdentityRow is one external identity. The refresh token is never part of
// it.
type IdentityRow struct {
	ID          string // UUIDv7
	UserID      string
	Issuer      string
	Subject     string
	ProviderID  string
	CreatedAt   int64 // unix milliseconds
	LastLoginAt int64
}

const identityColumns = `id, user_id, issuer, subject, provider_id, created_at, last_login_at`

func scanIdentity(scan func(...any) error) (IdentityRow, error) {
	var r IdentityRow
	if err := scan(&r.ID, &r.UserID, &r.Issuer, &r.Subject, &r.ProviderID, &r.CreatedAt, &r.LastLoginAt); err != nil {
		return IdentityRow{}, err
	}
	return r, nil
}

// ListIdentities returns the external identities of one user, or of every
// user when userID is empty, ordered by creation time and id.
func (s *Store) ListIdentities(ctx context.Context, userID string) ([]IdentityRow, error) {
	q, args := `SELECT `+identityColumns+` FROM external_identities`, []any(nil)
	if userID != "" {
		q, args = q+` WHERE user_id = ?`, []any{userID}
	}
	rows, err := s.db.QueryContext(ctx, q+` ORDER BY created_at, id`, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list identities: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []IdentityRow
	for rows.Next() {
		r, err := scanIdentity(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("store: list identities: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list identities: %w", err)
	}
	return out, nil
}

// IdentityExists reports whether (issuer, subject) is linked to an account.
func (s *Store) IdentityExists(ctx context.Context, issuer, subject string) (bool, error) {
	var one int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM external_identities WHERE issuer = ? AND subject = ?`, issuer, subject).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("store: read external identity: %w", err)
	}
	return true, nil
}

// LinkCandidate is the local account an e-mail address points at, with the
// facts the linking decision needs.
type LinkCandidate struct {
	User UserRow
	// AdminConsent is true when the account is an admin account whose admin
	// gave the panel consent to be linked.
	AdminConsent bool
	// IssuerLinked is true when the account already has an external identity,
	// at any issuer. One account holds at most one identity: the background
	// refresh replaces the account's IdP roles and is guarded by the revision
	// of the one identity it refreshed, which is only sound with a single
	// identity per account.
	IssuerLinked bool
}

// LinkCandidateByEmail returns the account whose e-mail address equals email
// (case-insensitively), or [ErrNotFound]. Only the e-mail column is consulted: a user name
// never makes an account a candidate.
func (s *Store) LinkCandidateByEmail(ctx context.Context, email string) (LinkCandidate, error) {
	if email == "" {
		return LinkCandidate{}, ErrNotFound
	}
	var c LinkCandidate
	err := s.Tx(ctx, func(tx *sql.Tx) error {
		var sc userScan
		err := tx.QueryRowContext(ctx, `SELECT `+userColumns+` FROM users u WHERE u.email = ? AND u.email <> ''`, email).Scan(sc.dest()...)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("store: read link candidate: %w", err)
		}
		c.User = sc.row()
		if c.AdminConsent, err = rowExists(ctx, tx, `SELECT 1 FROM admin_link_consent WHERE user_id = ?`, c.User.ID); err != nil {
			return fmt.Errorf("store: read admin link consent: %w", err)
		}
		if c.IssuerLinked, err = rowExists(ctx, tx, `SELECT 1 FROM external_identities WHERE user_id = ?`, c.User.ID); err != nil {
			return fmt.Errorf("store: read linked identities: %w", err)
		}
		return nil
	})
	if err != nil {
		return LinkCandidate{}, err
	}
	return c, nil
}

// LinkIdentityParams describes the link of an IdP identity to a local
// account.
type LinkIdentityParams struct {
	UserID string
	// Email is the address the decision matched. The link applies only while
	// the account still has it.
	Email string
	// Issuer, Subject and ProviderID are the identity and the provider the
	// sign-in went through.
	Issuer, Subject, ProviderID string
	// Domain is the e-mail domain the decision checked against the provider's
	// allow-list. The link applies only while the provider still allows
	// linking and still lists the domain.
	Domain string
	NowMS  int64
	// OnLinked, when not nil, runs inside the transaction after the identity
	// was written; the caller writes the audit row with the given Execer so
	// that it commits with the link. An error rolls the link back.
	OnLinked func(ctx context.Context, ex Execer, id IdentityRow) error
}

// LinkIdentity attaches the identity to the account in one transaction and
// returns it. The conditions are checked again inside the transaction, so a
// change between the decision and the write cannot be linked through: the
// account must be active, still have p.Email, have no external identity,
// and, when it is an admin account, hold the panel consent; the provider must
// still allow linking and still list p.Domain, so a policy that was revoked
// after the decision is not linked through. It returns
// [ErrNotFound] for an unknown account, [ErrLinkRefused] when a condition no
// longer holds and [ErrIdentityTaken] when the identity is linked already.
func (s *Store) LinkIdentity(ctx context.Context, p LinkIdentityParams) (IdentityRow, error) {
	if p.UserID == "" || p.Email == "" || p.Issuer == "" || p.Subject == "" || p.ProviderID == "" || p.Domain == "" {
		return IdentityRow{}, errors.New("store: a link needs a user, an e-mail address, a domain, an issuer, a subject and a provider")
	}
	var row IdentityRow
	err := s.Tx(ctx, func(tx *sql.Tx) error {
		u, err := txUser(ctx, tx, p.UserID)
		if err != nil {
			return err
		}
		if !u.Active || !strings.EqualFold(u.Email, p.Email) {
			return ErrLinkRefused
		}
		if on, err := rowExists(ctx, tx, `SELECT 1 FROM oidc_link_policies WHERE provider_id = ? AND linking_allowed = 1`, p.ProviderID); err != nil {
			return fmt.Errorf("store: link identity: %w", err)
		} else if !on {
			return ErrLinkRefused
		}
		if listed, err := rowExists(ctx, tx, `SELECT 1 FROM oidc_link_domains WHERE provider_id = ? AND domain = ?`,
			p.ProviderID, NormalizeLinkDomain(p.Domain)); err != nil {
			return fmt.Errorf("store: link identity: %w", err)
		} else if !listed {
			return ErrLinkRefused
		}
		if taken, err := rowExists(ctx, tx, `SELECT 1 FROM external_identities WHERE issuer = ? AND subject = ?`, p.Issuer, p.Subject); err != nil {
			return fmt.Errorf("store: link identity: %w", err)
		} else if taken {
			return ErrIdentityTaken
		}
		if linked, err := rowExists(ctx, tx, `SELECT 1 FROM external_identities WHERE user_id = ?`, p.UserID); err != nil {
			return fmt.Errorf("store: link identity: %w", err)
		} else if linked {
			return ErrLinkRefused
		}
		if u.Role == roleAdmin {
			if ok, err := rowExists(ctx, tx, `SELECT 1 FROM admin_link_consent WHERE user_id = ?`, p.UserID); err != nil {
				return fmt.Errorf("store: link identity: %w", err)
			} else if !ok {
				return ErrLinkRefused
			}
		}
		row = IdentityRow{ID: NewID(), UserID: p.UserID, Issuer: p.Issuer, Subject: p.Subject, ProviderID: p.ProviderID,
			CreatedAt: p.NowMS, LastLoginAt: p.NowMS}
		if _, err := tx.ExecContext(ctx, `
INSERT INTO external_identities (id, user_id, issuer, subject, provider_id, created_at, last_login_at)
VALUES (?, ?, ?, ?, ?, ?, ?)`, row.ID, row.UserID, row.Issuer, row.Subject, row.ProviderID, row.CreatedAt, row.LastLoginAt); err != nil {
			return fmt.Errorf("store: link identity: %w", err)
		}
		if p.OnLinked != nil {
			return p.OnLinked(ctx, tx, row)
		}
		return nil
	})
	if err != nil {
		return IdentityRow{}, err
	}
	return row, nil
}

// DetachIdentityParams describes the removal of an identity from an account.
type DetachIdentityParams struct {
	IdentityID string
	NowMS      int64
	// Audit, when not nil, is written in the same transaction.
	Audit *AuditRow
}

// DetachIdentity removes the identity and, in the same transaction, drops the
// account's IdP roles (they came from the IdP login the identity stood for,
// and AD-10 forbids them outliving it), bumps its credential version and ends
// its sessions. The account itself, its local roles and its Workers stay.
// It returns the removed identity, [ErrNotFound], [ErrLastSignInMethod] when
// the account would be left with no local password and no identity, or
// [ErrLastAdmin] when the dropped IdP roles are the last active admin.
func (s *Store) DetachIdentity(ctx context.Context, p DetachIdentityParams) (IdentityRow, error) {
	var row IdentityRow
	err := s.Tx(ctx, func(tx *sql.Tx) error {
		var err error
		row, err = scanIdentity(tx.QueryRowContext(ctx, `SELECT `+identityColumns+` FROM external_identities WHERE id = ?`, p.IdentityID).Scan)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("store: detach identity: %w", err)
		}
		u, err := txUser(ctx, tx, row.UserID)
		if err != nil {
			return err
		}
		var others int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM external_identities WHERE user_id = ? AND id <> ?`, row.UserID, row.ID).Scan(&others); err != nil {
			return fmt.Errorf("store: detach identity: %w", err)
		}
		if others == 0 && u.PasswordHash == "" {
			return ErrLastSignInMethod
		}
		if u.Role == roleAdmin {
			localAdmin, err := rowExists(ctx, tx, `SELECT 1 FROM user_roles WHERE user_id = ? AND role = 'admin' AND source = 'local'`, row.UserID)
			if err != nil {
				return fmt.Errorf("store: detach identity: %w", err)
			}
			if !localAdmin && u.Active {
				if err := requireOtherAdmin(ctx, tx, row.UserID); err != nil {
					return err
				}
			}
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM external_identities WHERE id = ?`, row.ID); err != nil {
			return fmt.Errorf("store: detach identity: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM user_roles WHERE user_id = ? AND source = 'idp'`, row.UserID); err != nil {
			return fmt.Errorf("store: detach identity: drop idp roles: %w", err)
		}
		if u.PasswordHash != "" {
			// The account keeps its local credential, so it keeps a local
			// grant to use it with (F-07).
			if err := ensureLocalUserGrant(ctx, tx, row.UserID); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE users SET credential_version = credential_version + 1, updated_at = ? WHERE id = ?`, p.NowMS, row.UserID); err != nil {
			return fmt.Errorf("store: detach identity: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ?`, row.UserID); err != nil {
			return fmt.Errorf("store: detach identity: revoke sessions: %w", err)
		}
		return s.auditIn(ctx, tx, p.Audit)
	})
	if err != nil {
		return IdentityRow{}, err
	}
	return row, nil
}

// SetFirstPasswordParams describes an admin setting the first local password
// of an account that has none (an account an IdP created).
type SetFirstPasswordParams struct {
	UserID string
	// PasswordHash is the PHC hash of the temporary password.
	PasswordHash string
	// Email, when not empty, is stored as the account's e-mail address so the
	// user can sign in by it; it must be free and the account must have none.
	Email string
	NowMS int64
	Audit *AuditRow
}

// SetFirstPassword stores the temporary password hash of an account without a
// local password, flags it must-change and, when asked, sets its e-mail
// address, in one transaction. Existing sessions stay: a password is an
// additional way in. It returns [ErrNotFound], [ErrUserInactive],
// [ErrHasLocalPassword] or [ErrEmailTaken] (the address is used, or is another
// account's user name, as in [Store.CreateUser]; an account that has another
// address already keeps it).
func (s *Store) SetFirstPassword(ctx context.Context, p SetFirstPasswordParams) error {
	if p.PasswordHash == "" {
		return errors.New("store: a first password needs a hash")
	}
	return s.Tx(ctx, func(tx *sql.Tx) error {
		u, err := txUser(ctx, tx, p.UserID)
		if err != nil {
			return err
		}
		switch {
		case !u.Active:
			return ErrUserInactive
		case u.PasswordHash != "":
			return ErrHasLocalPassword
		}
		email := u.Email
		if p.Email != "" && !strings.EqualFold(p.Email, u.Email) {
			if u.Email != "" {
				return ErrEmailTaken // the account has another address; change it first
			}
			if taken, err := rowExists(ctx, tx, `SELECT 1 FROM users WHERE (email = ?1 OR username = ?1) AND id <> ?2`, p.Email, p.UserID); err != nil {
				return fmt.Errorf("store: set first password: %w", err)
			} else if taken {
				return ErrEmailTaken
			}
			email = p.Email
		}
		if _, err := tx.ExecContext(ctx, `
UPDATE users SET password_hash = ?, must_change_password = 1, email = ?, updated_at = ? WHERE id = ?`,
			p.PasswordHash, email, p.NowMS, p.UserID); err != nil {
			return fmt.Errorf("store: set first password: %w", err)
		}
		// A local credential is a local way in: the account holds an
		// explicit local grant, so that losing its IdP roles does not leave
		// the password without any access (F-07).
		if err := ensureLocalUserGrant(ctx, tx, p.UserID); err != nil {
			return err
		}
		return s.auditIn(ctx, tx, p.Audit)
	})
}

// AdminLinkConsentParams describes a change of the panel consent of an admin
// account.
type AdminLinkConsentParams struct {
	UserID  string
	Granted bool
	// ActorID is the admin who changed the flag.
	ActorID string
	NowMS   int64
	// Audit is written in the same transaction, only when the flag changed.
	Audit *AuditRow
}

// SetAdminLinkConsent grants or withdraws the consent of an admin account to
// be the target of an e-mail link (off by default). It returns [ErrNotFound]
// for an unknown account and [ErrNotAdminAccount] for an account that is not
// an admin, and reports whether the flag changed.
func (s *Store) SetAdminLinkConsent(ctx context.Context, p AdminLinkConsentParams) (changed bool, err error) {
	err = s.Tx(ctx, func(tx *sql.Tx) error {
		u, err := txUser(ctx, tx, p.UserID)
		if err != nil {
			return err
		}
		if u.Role != roleAdmin {
			return ErrNotAdminAccount
		}
		have, err := rowExists(ctx, tx, `SELECT 1 FROM admin_link_consent WHERE user_id = ?`, p.UserID)
		if err != nil {
			return fmt.Errorf("store: admin link consent: %w", err)
		}
		if have == p.Granted {
			return nil
		}
		if p.Granted {
			_, err = tx.ExecContext(ctx, `INSERT INTO admin_link_consent (user_id, granted_by, granted_at) VALUES (?, ?, ?)`,
				p.UserID, p.ActorID, p.NowMS)
		} else {
			_, err = tx.ExecContext(ctx, `DELETE FROM admin_link_consent WHERE user_id = ?`, p.UserID)
		}
		if err != nil {
			return fmt.Errorf("store: admin link consent: %w", err)
		}
		changed = true
		return s.auditIn(ctx, tx, p.Audit)
	})
	if err != nil {
		return false, err
	}
	return changed, nil
}

// AdminLinkConsent reports whether the account has the panel consent.
func (s *Store) AdminLinkConsent(ctx context.Context, userID string) (bool, error) {
	var one int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM admin_link_consent WHERE user_id = ?`, userID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("store: admin link consent: %w", err)
	}
	return true, nil
}

// LinkPolicyRow is the linking policy of one OIDC provider. The zero value is
// the default: nothing is allowed.
type LinkPolicyRow struct {
	ProviderID string
	// LinkingAllowed lets a first sign-in link to the local account that has
	// the same e-mail address (R67).
	LinkingAllowed bool
	// AutoCreate creates a separate account when linking is refused, instead of
	// refusing the sign-in.
	AutoCreate bool
	// Domains are the normalised e-mail domains that may be linked, sorted.
	Domains   []string
	UpdatedAt int64
}

// NormalizeLinkDomain returns the canonical form of an e-mail domain: trimmed,
// lower case, without a leading "@".
func NormalizeLinkDomain(d string) string {
	return strings.ToLower(strings.TrimPrefix(strings.TrimSpace(d), "@"))
}

// normalizeLinkDomains normalises, drops empty entries and duplicates, and
// sorts.
func normalizeLinkDomains(in []string) []string {
	var out []string
	for _, d := range in {
		if d = NormalizeLinkDomain(d); d != "" && !slices.Contains(out, d) {
			out = append(out, d)
		}
	}
	slices.Sort(out)
	return out
}

// GetLinkPolicy returns the policy of a provider; a provider without a row
// gets the all-off default.
func (s *Store) GetLinkPolicy(ctx context.Context, providerID string) (LinkPolicyRow, error) {
	all, err := s.listLinkPolicies(ctx, providerID)
	if err != nil {
		return LinkPolicyRow{}, err
	}
	if len(all) == 0 {
		return LinkPolicyRow{ProviderID: providerID}, nil
	}
	return all[0], nil
}

// ListLinkPolicies returns every stored policy ordered by provider id.
func (s *Store) ListLinkPolicies(ctx context.Context) ([]LinkPolicyRow, error) {
	return s.listLinkPolicies(ctx, "")
}

func (s *Store) listLinkPolicies(ctx context.Context, providerID string) ([]LinkPolicyRow, error) {
	var out []LinkPolicyRow
	err := s.Tx(ctx, func(tx *sql.Tx) error {
		out = nil
		q, args := `SELECT provider_id, linking_allowed, auto_create, updated_at FROM oidc_link_policies`, []any(nil)
		if providerID != "" {
			q, args = q+` WHERE provider_id = ?`, []any{providerID}
		}
		rows, err := tx.QueryContext(ctx, q+` ORDER BY provider_id`, args...)
		if err != nil {
			return fmt.Errorf("store: list link policies: %w", err)
		}
		for rows.Next() {
			var (
				r         LinkPolicyRow
				lnk, auto int
			)
			if err := rows.Scan(&r.ProviderID, &lnk, &auto, &r.UpdatedAt); err != nil {
				_ = rows.Close()
				return fmt.Errorf("store: list link policies: %w", err)
			}
			r.LinkingAllowed, r.AutoCreate = lnk != 0, auto != 0
			out = append(out, r)
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			return fmt.Errorf("store: list link policies: %w", err)
		}
		for i := range out {
			drows, err := tx.QueryContext(ctx, `SELECT domain FROM oidc_link_domains WHERE provider_id = ? ORDER BY domain`, out[i].ProviderID)
			if err != nil {
				return fmt.Errorf("store: list link domains: %w", err)
			}
			for drows.Next() {
				var d string
				if err := drows.Scan(&d); err != nil {
					_ = drows.Close()
					return fmt.Errorf("store: list link domains: %w", err)
				}
				out[i].Domains = append(out[i].Domains, d)
			}
			err = drows.Err()
			_ = drows.Close()
			if err != nil {
				return fmt.Errorf("store: list link domains: %w", err)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// PutLinkPolicyParams describes the replacement of a provider's policy.
type PutLinkPolicyParams struct {
	Policy LinkPolicyRow
	// Audit, when not nil, is written in the same transaction.
	Audit *AuditRow
}

// PutLinkPolicy replaces the policy of a provider and its domain list in one
// transaction. The domains are normalised.
func (s *Store) PutLinkPolicy(ctx context.Context, p PutLinkPolicyParams) error {
	pol := p.Policy
	if pol.ProviderID == "" {
		return errors.New("store: a link policy needs a provider id")
	}
	domains := normalizeLinkDomains(pol.Domains)
	return s.Tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `
INSERT INTO oidc_link_policies (provider_id, linking_allowed, auto_create, updated_at) VALUES (?, ?, ?, ?)
ON CONFLICT (provider_id) DO UPDATE SET linking_allowed = excluded.linking_allowed,
    auto_create = excluded.auto_create, updated_at = excluded.updated_at`,
			pol.ProviderID, boolInt(pol.LinkingAllowed), boolInt(pol.AutoCreate), pol.UpdatedAt); err != nil {
			return fmt.Errorf("store: put link policy: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM oidc_link_domains WHERE provider_id = ?`, pol.ProviderID); err != nil {
			return fmt.Errorf("store: put link policy: %w", err)
		}
		for _, d := range domains {
			if _, err := tx.ExecContext(ctx, `INSERT INTO oidc_link_domains (provider_id, domain) VALUES (?, ?)`, pol.ProviderID, d); err != nil {
				return fmt.Errorf("store: put link policy: %w", err)
			}
		}
		return s.auditIn(ctx, tx, p.Audit)
	})
}

// ensureLocalUserGrant gives the account a local "user" grant unless it holds
// a local grant already.
func ensureLocalUserGrant(ctx context.Context, tx *sql.Tx, userID string) error {
	if _, err := tx.ExecContext(ctx, `
INSERT INTO user_roles (user_id, role, source)
SELECT ?1, 'user', 'local' WHERE NOT EXISTS (SELECT 1 FROM user_roles WHERE user_id = ?1 AND source = 'local')`, userID); err != nil {
		return fmt.Errorf("store: grant the local user role: %w", err)
	}
	return nil
}
