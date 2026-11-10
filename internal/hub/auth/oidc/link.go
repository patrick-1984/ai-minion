package oidc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/patrick-1984/ai-minion/internal/hub/audit"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/logx"
	"github.com/patrick-1984/ai-minion/internal/ops"
)

// This file is Story 6.4 (linked accounts, AD-10, R67-R70). It has three
// parts, each with its own port:
//
//   - [DecideLink], a pure decision table: may a first IdP sign-in be linked
//     to the local account that has the same e-mail address?
//   - [Linker], which the sign-in [Service] asks for an identity it has not
//     seen. It applies the decision and writes the link and its audit row in
//     one transaction.
//   - [LinkAdmin], the administration behind identity_list, identity_detach,
//     user_set_password, admin_link_consent and the provider linking policy.
//
// Later sign-ins never come back here: an identity that is linked matches by
// (issuer, subject) alone, whatever its e-mail claim says.

// Audit vocabulary of linked accounts (AD-20). A row names the account or the
// identity by id and never holds an e-mail address, a subject, a token or a
// password.
const (
	AuditActionIdentityLink   = "identity.link"
	AuditActionIdentityDetach = "identity.detach"
	AuditActionSetPassword    = "user.set_password"
	AuditActionLinkConsent    = "admin.link_consent"
	AuditActionLinkPolicy     = "oidc.link_policy_set"
)

// LinkReason says why a link was refused, or [ReasonLinked] when it is
// allowed.
type LinkReason string

// The reasons of [DecideLink]. They are checked in this order.
const (
	// ReasonLinked: every condition holds.
	ReasonLinked LinkReason = "linked"
	// ReasonLinkingOff: the provider's "linking allowed" flag is off.
	ReasonLinkingOff LinkReason = "linking_off"
	// ReasonEmailUnverified: the IdP did not say email_verified=true.
	ReasonEmailUnverified LinkReason = "email_unverified"
	// ReasonDomainNotAllowed: the e-mail domain is not on the provider's
	// allow-list.
	ReasonDomainNotAllowed LinkReason = "domain_not_allowed"
	// ReasonInactive: the local account is deactivated.
	ReasonInactive LinkReason = "inactive"
	// ReasonIssuerLinked: the local account already has an external identity.
	// An account holds at most one: a second one at the same issuer would let
	// any account of the IdP that carries the address take the account over,
	// and one at another issuer would break the background refresh's guard
	// (it compares the revision of the one identity it refreshed).
	ReasonIssuerLinked LinkReason = "issuer_already_linked"
	// ReasonAdminNoConsent: the local account is an admin and has no panel
	// consent.
	ReasonAdminNoConsent LinkReason = "admin_no_consent"
	// ReasonChanged: the account changed between the decision and the write
	// (deactivated, new e-mail address, a second identity, withdrawn
	// consent), so the store refused the link. It is not a [DecideLink]
	// result.
	ReasonChanged LinkReason = "changed"
)

// LinkPolicy is the linking policy of a provider; the zero value allows
// nothing.
type LinkPolicy struct {
	// LinkingAllowed is the provider's "linking allowed" flag (R67).
	LinkingAllowed bool
	// AutoCreate creates a separate account when linking is refused, instead
	// of refusing the sign-in.
	AutoCreate bool
	// Domains is the allow-list of e-mail domains. Entries match the whole
	// domain, lower case: "example.com" does not allow "sub.example.com".
	Domains []string
}

// LinkTarget is what the decision needs to know about the local account.
type LinkTarget struct {
	Active bool
	// Admin is true when the account's effective role is admin.
	Admin bool
	// AdminConsent is the panel consent of an admin account (R68).
	AdminConsent bool
	// IssuerLinked is true when the account already has an external
	// identity (one account holds at most one).
	IssuerLinked bool
}

// LinkInput is the input of [DecideLink].
type LinkInput struct {
	Policy LinkPolicy
	// Domain is the lower-case domain of the e-mail claim.
	Domain string
	// EmailVerified is the email_verified claim, true only for the JSON
	// boolean true.
	EmailVerified bool
	Target        LinkTarget
}

// DecideLink is the decision table of R67 to R69. The link is made only when
// all of these hold: the provider allows linking, the IdP verified the
// e-mail address, its domain is on the provider's allow-list, the account is
// active, it has no external identity yet, and, for an admin account,
// the admin gave the panel consent. It returns [ReasonLinked] or the first
// condition that failed.
func DecideLink(in LinkInput) LinkReason {
	switch {
	case !in.Policy.LinkingAllowed:
		return ReasonLinkingOff
	case !in.EmailVerified:
		return ReasonEmailUnverified
	case !domainAllowed(in.Policy.Domains, in.Domain):
		return ReasonDomainNotAllowed
	case !in.Target.Active:
		return ReasonInactive
	case in.Target.IssuerLinked:
		return ReasonIssuerLinked
	case in.Target.Admin && !in.Target.AdminConsent:
		return ReasonAdminNoConsent
	}
	return ReasonLinked
}

func domainAllowed(allowed []string, domain string) bool {
	if domain == "" {
		return false
	}
	for _, d := range allowed {
		if store.NormalizeLinkDomain(d) == domain {
			return true
		}
	}
	return false
}

// maxEmailLen is the longest e-mail address accepted from a claim (RFC 5321).
const maxEmailLen = 254

// ParseEmailClaim returns the lower-case address and its domain from the
// "email" claim, and false when the claim is missing, is not a string or is
// not a plain local@domain address. Spaces, control characters, angle
// brackets, commas and more than one "@" are refused: the value comes from
// the IdP and decides which account it is compared with.
func ParseEmailClaim(claims map[string]any) (email, domain string, ok bool) {
	raw, _ := claims["email"].(string)
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > maxEmailLen || !utf8.ValidString(raw) {
		return "", "", false
	}
	for _, r := range raw {
		if unicode.IsSpace(r) || unicode.IsControl(r) || strings.ContainsRune("<>,;()\"\\", r) {
			return "", "", false
		}
	}
	at := strings.IndexByte(raw, '@')
	if at <= 0 || at != strings.LastIndexByte(raw, '@') || at == len(raw)-1 {
		return "", "", false
	}
	// The address is split before it is normalised and only ASCII letters are
	// folded: a full Unicode fold can change the byte length (U+0130, U+212A),
	// which would move the "@" and make the checked domain a different string
	// than the one in the matched address. A domain must be ASCII (an IDN
	// arrives in its punycode form), so no fold can turn another domain into
	// an allow-listed one.
	local, dom := raw[:at], raw[at+1:]
	for i := 0; i < len(dom); i++ {
		if dom[i] >= utf8.RuneSelf {
			return "", "", false
		}
	}
	domain = asciiLower(dom)
	return asciiLower(local) + "@" + domain, domain, true
}

// asciiLower lower-cases the ASCII letters of s and leaves every other byte,
// so the byte length never changes.
func asciiLower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if 'A' <= c && c <= 'Z' {
			b[i] = c + ('a' - 'A')
		}
	}
	return string(b)
}

// emailVerified reads email_verified. Only the JSON boolean true counts; a
// string such as "true" does not.
func emailVerified(claims map[string]any) bool {
	v, _ := claims["email_verified"].(bool)
	return v
}

// LinkStore is the persistence port of the [Linker]; *store.Store implements
// it.
type LinkStore interface {
	IdentityExists(ctx context.Context, issuer, subject string) (bool, error)
	LinkCandidateByEmail(ctx context.Context, email string) (store.LinkCandidate, error)
	GetLinkPolicy(ctx context.Context, providerID string) (store.LinkPolicyRow, error)
	LinkIdentity(ctx context.Context, p store.LinkIdentityParams) (store.IdentityRow, error)
}

var _ LinkStore = (*store.Store)(nil)

// Refusal is a sign-in that the linking rules refuse. The sign-in shows Title
// and Message on its error page and creates no account.
type Refusal struct {
	Reason  LinkReason
	Title   string
	Message string
}

// IdentityLinker is what the sign-in [Service] asks about an identity: it is
// called after the ID token is verified and before the account is written.
// It returns nil to continue the sign-in (the identity is known, there is
// nothing to link, it was linked, or a separate account is wanted) and a
// [*Refusal] to end it.
type IdentityLinker interface {
	Resolve(ctx context.Context, cfg Config, ident Identity, hasAccess bool) (*Refusal, error)
}

var _ IdentityLinker = (*Linker)(nil)

// Linker links a first IdP sign-in to an existing local account by e-mail
// address when the rules of [DecideLink] allow it (R67 to R69).
//
// It acts only for an identity that no account has, whose IdP roles give
// access (no matching rule means no access, so nothing is linked for it), and
// whose ID token carries a plain e-mail address that equals the e-mail of one
// local account. In every other case the sign-in goes on as before.
type Linker struct {
	Store LinkStore
	Audit Auditor
	Log   *slog.Logger
	// Now defaults to [time.Now]; tests fix it.
	Now func() time.Time
}

func (l *Linker) now() time.Time {
	if l.Now != nil {
		return l.Now()
	}
	return time.Now()
}

func (l *Linker) logger() *slog.Logger {
	if l.Log != nil {
		return l.Log
	}
	return slog.Default()
}

// Resolve implements [IdentityLinker].
func (l *Linker) Resolve(ctx context.Context, cfg Config, ident Identity, hasAccess bool) (*Refusal, error) {
	if !hasAccess {
		return nil, nil
	}
	known, err := l.Store.IdentityExists(ctx, ident.Issuer, ident.Subject)
	if err != nil {
		return nil, fmt.Errorf("oidc link: %w", err)
	}
	if known {
		return nil, nil // later sign-ins match by issuer and subject only
	}
	email, domain, ok := ParseEmailClaim(ident.Claims)
	if !ok {
		return nil, nil
	}
	cand, err := l.Store.LinkCandidateByEmail(ctx, email)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil // no local account has this address: a new account
	}
	if err != nil {
		return nil, fmt.Errorf("oidc link: %w", err)
	}
	pol, err := l.Store.GetLinkPolicy(ctx, cfg.ID)
	if err != nil {
		return nil, fmt.Errorf("oidc link: %w", err)
	}
	reason := DecideLink(LinkInput{
		Policy: LinkPolicy{LinkingAllowed: pol.LinkingAllowed, AutoCreate: pol.AutoCreate, Domains: pol.Domains},
		Domain: domain, EmailVerified: emailVerified(ident.Claims),
		Target: LinkTarget{Active: cand.User.Active, Admin: cand.User.Role == ops.RoleAdmin,
			AdminConsent: cand.AdminConsent, IssuerLinked: cand.IssuerLinked},
	})
	if reason == ReasonLinked {
		reason = l.link(ctx, cfg, ident, email, domain, cand.User)
		if reason == ReasonLinked || reason == reasonKnownNow {
			return nil, nil
		}
		if reason == reasonError {
			return nil, errors.New("oidc link: the link could not be written")
		}
	}
	l.deny(ctx, cand.User.ID, reason)
	if pol.AutoCreate {
		// The provider asked for a separate account instead of a refusal.
		return nil, nil
	}
	return refusal(reason), nil
}

// Internal results of [Linker.link] besides the public reasons.
const (
	// reasonKnownNow: a concurrent sign-in linked the same identity first;
	// the sign-in goes on as for a known identity.
	reasonKnownNow LinkReason = "known_now"
	reasonError    LinkReason = "error"
)

// link writes the link and its audit row in one transaction. The store checks
// the conditions again inside it, so a change since the decision is refused
// rather than linked through.
func (l *Linker) link(ctx context.Context, cfg Config, ident Identity, email, domain string, u store.UserRow) LinkReason {
	_, err := l.Store.LinkIdentity(ctx, store.LinkIdentityParams{
		UserID: u.ID, Email: email, Domain: domain, Issuer: ident.Issuer, Subject: ident.Subject, ProviderID: cfg.ID,
		NowMS: store.UnixMS(l.now()),
		OnLinked: func(ctx context.Context, ex store.Execer, id store.IdentityRow) error {
			return l.Audit.RecordTx(ctx, ex, audit.Event{Actor: u.ID, Action: AuditActionIdentityLink,
				Target: id.ID, Outcome: OutcomeSuccess})
		},
	})
	switch {
	case err == nil:
		return ReasonLinked
	case errors.Is(err, store.ErrIdentityTaken):
		return reasonKnownNow
	case errors.Is(err, store.ErrLinkRefused):
		// Deactivated, e-mail changed, a second identity,
		// consent or the provider's policy withdrawn since the decision: the caller refuses.
		return ReasonChanged
	}
	l.logger().Error("OIDC link failed", "provider", cfg.ID, "error", err)
	return reasonError
}

// deny records a refused link. A failing audit write does not change the
// answer, but it is logged.
func (l *Linker) deny(ctx context.Context, userID string, reason LinkReason) {
	l.logger().Info("OIDC sign-in not linked", "user", userID, "reason", string(reason))
	err := l.Audit.Record(context.WithoutCancel(ctx), audit.Event{Actor: anonymousActor, Action: AuditActionIdentityLink,
		Target: userID, Outcome: OutcomeDenied})
	if err != nil {
		l.logger().Error("audit of a refused OIDC link failed", "error", err)
	}
}

// refusal is the English explanation of a refused link. It never repeats a
// claim value.
func refusal(reason LinkReason) *Refusal {
	const (
		title = "Account not linked"
		tail  = " No account was created. Ask your administrator."
	)
	msg := map[LinkReason]string{
		ReasonLinkingOff:       "A local account already uses the e-mail address of your identity provider account, and linking accounts is turned off for this identity provider.",
		ReasonEmailUnverified:  "A local account already uses the e-mail address of your identity provider account, but the identity provider has not verified that address.",
		ReasonDomainNotAllowed: "A local account already uses the e-mail address of your identity provider account, but accounts with that e-mail domain may not be linked.",
		ReasonInactive:         "The local account with the e-mail address of your identity provider account is deactivated.",
		ReasonChanged:          "The local account with the e-mail address of your identity provider account changed while you signed in. Try again.",
		ReasonIssuerLinked:     "The local account with the e-mail address of your identity provider account is already linked to an identity provider account.",
		ReasonAdminNoConsent:   "The local account with the e-mail address of your identity provider account is an administrator account, and no administrator has allowed linking into it.",
	}[reason]
	if msg == "" {
		msg = "The sign-in could not be linked to an existing account."
	}
	return &Refusal{Reason: reason, Title: title, Message: msg + tail}
}

// ---- administration ----------------------------------------------------------

// Errors of [LinkAdmin]. Callers match them with errors.Is.
var (
	ErrUserNotFound      = errors.New("oidc link: user not found")
	ErrIdentityNotFound  = errors.New("oidc link: identity not found")
	ErrUserInactive      = errors.New("oidc link: user is deactivated")
	ErrEmailTaken        = errors.New("oidc link: e-mail address is taken")
	ErrHasLocalPassword  = errors.New("oidc link: the account already has a local password; reset it instead")
	ErrLastSignInMethod  = errors.New("oidc link: this is the only way to sign in to the account; set a local password first")
	ErrLastAdmin         = errors.New("oidc link: this is the last active admin")
	ErrNotAdminAccount   = errors.New("oidc link: the account is not an admin")
	ErrActAsNotAllowed   = errors.New("oidc link: an admin acting as a user cannot change the account's credentials or link consent")
	ErrUnknownProviderID = errors.New("oidc link: unknown provider")
)

// LinkAdminStore is the persistence port of the [LinkAdmin]; *store.Store
// implements it.
type LinkAdminStore interface {
	ListIdentities(ctx context.Context, userID string) ([]store.IdentityRow, error)
	DetachIdentity(ctx context.Context, p store.DetachIdentityParams) (store.IdentityRow, error)
	SetFirstPassword(ctx context.Context, p store.SetFirstPasswordParams) error
	SetAdminLinkConsent(ctx context.Context, p store.AdminLinkConsentParams) (bool, error)
	ListLinkPolicies(ctx context.Context) ([]store.LinkPolicyRow, error)
	PutLinkPolicy(ctx context.Context, p store.PutLinkPolicyParams) error
}

var _ LinkAdminStore = (*store.Store)(nil)

// PasswordHasher hashes a password; *local.Hasher implements it.
type PasswordHasher interface {
	Hash(ctx context.Context, password string) (string, error)
}

// LinkActor says who performs an administrative change, for the audit row.
type LinkActor struct {
	// ID is the audit actor: a user id, or "key:<id>" for an API key.
	ID string
	// ActingAs is the user an admin acts as (act-as); empty otherwise.
	ActingAs string
}

// LinkAdmin implements the administration of linked accounts: identity_list,
// identity_detach, user_set_password, admin_link_consent and the provider
// linking policy. It holds no state and is safe for concurrent use. Every
// change writes its audit row in the transaction of the change.
type LinkAdmin struct {
	Store  LinkAdminStore
	Hasher PasswordHasher
	// NewPassword returns a fresh temporary password (local.GeneratePassword
	// with the system random source).
	NewPassword func() (string, error)
	// ProviderExists, when set, tells whether a provider id is configured
	// (settings or panel); SetPolicy refuses an unknown id. Nil skips the
	// check.
	ProviderExists func(ctx context.Context, id string) (bool, error)
	// Now defaults to [time.Now].
	Now func() time.Time
}

func (a *LinkAdmin) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

func (a *LinkAdmin) audit(actor LinkActor, action, target string, ts int64) *store.AuditRow {
	return &store.AuditRow{ID: store.NewID(), TS: ts, Actor: actor.ID, ActingAs: actor.ActingAs,
		Action: action, Target: sanitizeTarget(target), Outcome: OutcomeSuccess}
}

// ListIdentities returns the identities of one account, or of all accounts
// when userID is empty, oldest first. Refresh tokens are never part of it.
func (a *LinkAdmin) ListIdentities(ctx context.Context, userID string) ([]ops.IdentityInfo, error) {
	rows, err := a.Store.ListIdentities(ctx, userID)
	if err != nil {
		return nil, err
	}
	out := make([]ops.IdentityInfo, 0, len(rows))
	for _, r := range rows {
		out = append(out, ops.IdentityInfo{ID: r.ID, UserID: r.UserID, ProviderID: r.ProviderID,
			Issuer: r.Issuer, Subject: r.Subject,
			CreatedAt: ops.Timestamp(time.UnixMilli(r.CreatedAt)), LastLoginAt: ops.Timestamp(time.UnixMilli(r.LastLoginAt))})
	}
	return out, nil
}

// Detach removes an identity from its account. The account stays; its IdP
// roles, sessions and the identity's refresh token end with it. It returns
// [ErrIdentityNotFound], [ErrLastSignInMethod] or [ErrLastAdmin].
func (a *LinkAdmin) Detach(ctx context.Context, actor LinkActor, identityID string) error {
	ts := store.UnixMS(a.now())
	_, err := a.Store.DetachIdentity(ctx, store.DetachIdentityParams{
		IdentityID: identityID, NowMS: ts, Audit: a.audit(actor, AuditActionIdentityDetach, identityID, ts),
	})
	return mapLinkErr(err, ErrIdentityNotFound)
}

// SetPassword gives an account that has no local password a generated
// temporary password, flagged must-change, and optionally an e-mail address
// (R69: the user can then sign in locally by user name or e-mail). The
// password is returned once. It returns [ErrUserNotFound], [ErrUserInactive],
// [ErrHasLocalPassword], [ErrEmailTaken] or, for an actor that acts as
// somebody, [ErrActAsNotAllowed].
func (a *LinkAdmin) SetPassword(ctx context.Context, actor LinkActor, userID, email string) (logx.Secret, error) {
	if actor.ActingAs != "" {
		return "", ErrActAsNotAllowed
	}
	password, err := a.NewPassword()
	if err != nil {
		return "", err
	}
	hash, err := a.Hasher.Hash(ctx, password)
	if err != nil {
		return "", fmt.Errorf("oidc link: set password: %w", err)
	}
	ts := store.UnixMS(a.now())
	err = a.Store.SetFirstPassword(ctx, store.SetFirstPasswordParams{
		UserID: userID, PasswordHash: hash, Email: strings.TrimSpace(email), NowMS: ts,
		Audit: a.audit(actor, AuditActionSetPassword, userID, ts),
	})
	if err != nil {
		return "", mapLinkErr(err, ErrUserNotFound)
	}
	return logx.Secret(password), nil
}

// SetConsent grants or withdraws the panel consent of an admin account to be
// the target of an e-mail link, and reports whether it changed. It returns
// [ErrUserNotFound], [ErrNotAdminAccount] or [ErrActAsNotAllowed].
func (a *LinkAdmin) SetConsent(ctx context.Context, actor LinkActor, userID string, granted bool) (changed bool, err error) {
	if actor.ActingAs != "" {
		return false, ErrActAsNotAllowed
	}
	ts := store.UnixMS(a.now())
	changed, err = a.Store.SetAdminLinkConsent(ctx, store.AdminLinkConsentParams{
		UserID: userID, Granted: granted, ActorID: actor.ID, NowMS: ts,
		Audit: a.audit(actor, AuditActionLinkConsent, userID, ts),
	})
	return changed, mapLinkErr(err, ErrUserNotFound)
}

// ListPolicies returns the stored linking policies ordered by provider id.
func (a *LinkAdmin) ListPolicies(ctx context.Context) ([]ops.OIDCLinkPolicyInfo, error) {
	rows, err := a.Store.ListLinkPolicies(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]ops.OIDCLinkPolicyInfo, 0, len(rows))
	for _, r := range rows {
		out = append(out, ops.OIDCLinkPolicyInfo{ProviderID: r.ProviderID, LinkingAllowed: r.LinkingAllowed,
			AutoCreate: r.AutoCreate, Domains: r.Domains})
	}
	return out, nil
}

// SetPolicy replaces the linking policy of a provider. It returns
// [ErrUnknownProviderID] when ProviderExists says the id is not configured.
func (a *LinkAdmin) SetPolicy(ctx context.Context, actor LinkActor, args ops.OIDCLinkPolicySetArgs) error {
	if a.ProviderExists != nil {
		ok, err := a.ProviderExists(ctx, args.ProviderID)
		if err != nil {
			return err
		}
		if !ok {
			return ErrUnknownProviderID
		}
	}
	ts := store.UnixMS(a.now())
	return a.Store.PutLinkPolicy(ctx, store.PutLinkPolicyParams{
		Policy: store.LinkPolicyRow{ProviderID: args.ProviderID, LinkingAllowed: args.LinkingAllowed,
			AutoCreate: args.AutoCreate, Domains: args.Domains, UpdatedAt: ts},
		Audit: a.audit(actor, AuditActionLinkPolicy, args.ProviderID, ts),
	})
}

// mapLinkErr translates the sentinel errors of the store; notFound is the
// error for store.ErrNotFound in the caller's context. Other errors pass
// through.
func mapLinkErr(err, notFound error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, store.ErrNotFound):
		return notFound
	case errors.Is(err, store.ErrUserInactive):
		return ErrUserInactive
	case errors.Is(err, store.ErrEmailTaken):
		return ErrEmailTaken
	case errors.Is(err, store.ErrHasLocalPassword):
		return ErrHasLocalPassword
	case errors.Is(err, store.ErrLastSignInMethod):
		return ErrLastSignInMethod
	case errors.Is(err, store.ErrLastAdmin):
		return ErrLastAdmin
	case errors.Is(err, store.ErrNotAdminAccount):
		return ErrNotAdminAccount
	}
	return err
}
