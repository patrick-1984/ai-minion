package api

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/hub/audit"
	"github.com/patrick-1984/ai-minion/internal/hub/auth/oidc"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// Audit vocabulary of the OIDC administration (AD-20): the target is the
// provider id; no event carries a secret, a claim value or a path.
const (
	AuditActionOIDCProviderSet    = "oidc.provider.set"
	AuditActionOIDCProviderDelete = "oidc.provider.delete"
	AuditActionRoleRuleSet        = "role_rule.set"

	AuditOutcomeOK     = "ok"
	AuditOutcomeRefuse = "refused"
)

// SettingsProviderReadOnlyMessage is the message of the conflict returned when
// a change targets the provider that the settings own (R72).
const SettingsProviderReadOnlyMessage = "set by environment variable or the settings page"

// OIDCStore is the persistence port of the OIDC operations; *store.Store
// implements it.
type OIDCStore interface {
	ListOIDCProviders(ctx context.Context) ([]store.OIDCProviderRow, error)
	GetOIDCProvider(ctx context.Context, id string) (store.OIDCProviderRow, error)
	PutOIDCProvider(ctx context.Context, p store.OIDCProviderRow) error
	DeleteOIDCProvider(ctx context.Context, id string) (bool, error)
	ListRoleRules(ctx context.Context, providerID string) ([]store.RoleRuleRow, error)
	ReplaceRoleRules(ctx context.Context, providerID, role string, values []string) error
}

var _ OIDCStore = (*store.Store)(nil)

// OIDCAuditor records audit events; *audit.Log implements it.
type OIDCAuditor interface {
	Record(ctx context.Context, ev audit.Event) error
}

var _ OIDCAuditor = (*audit.Log)(nil)

// OIDCActor identifies who changes the OIDC configuration, for the audit
// record. The REST framework fills it from the authorised principal.
type OIDCActor struct {
	// ID is the acting account. Required.
	ID string
	// ActingAs is the account the actor acts as (admin act-as); empty when none.
	ActingAs string
}

// OIDCHandlers implement the operations oidc_provider_list, oidc_provider_set
// and role_rule_set (Story 6.2). The REST framework (Story 3.9) calls them
// after it has authorised the manage action; this file holds only the logic,
// so the framework's handler registry binds them without further work:
//
//	api.Handle(ops.OpOIDCProviderList, adapt(h.ProviderList))
type OIDCHandlers struct {
	Store OIDCStore
	Audit OIDCAuditor
	// Settings returns the provider that the environment or the settings
	// describe, and false when none is configured. It is listed read-only.
	Settings func(ctx context.Context) (oidc.Config, bool, error)
	// Timeout is the deadline of the discovery probe of oidc_provider_set;
	// zero means the OIDC default.
	Timeout time.Duration
	// Discover probes a provider; nil means [oidc.Discover]. Tests replace it.
	Discover func(ctx context.Context, cfg oidc.Config) (*oidc.Provider, error)
	// Now defaults to [time.Now].
	Now func() time.Time
}

func (h *OIDCHandlers) now() time.Time {
	if h.Now != nil {
		return h.Now()
	}
	return time.Now()
}

// ProviderList returns the settings provider first, then the panel providers
// ordered by id. The client secret is never returned.
func (h *OIDCHandlers) ProviderList(ctx context.Context, _ ops.OIDCProviderListArgs) (ops.OIDCProviderListResult, error) {
	res := ops.OIDCProviderListResult{Providers: []ops.OIDCProviderInfo{}}
	if h.Settings != nil {
		cfg, ok, err := h.Settings(ctx)
		if err != nil {
			return ops.OIDCProviderListResult{}, fmt.Errorf("api: read the OIDC settings: %w", err)
		}
		if ok {
			res.Providers = append(res.Providers, info(cfg, true))
		}
	}
	rows, err := h.Store.ListOIDCProviders(ctx)
	if err != nil {
		return ops.OIDCProviderListResult{}, err
	}
	rules, err := h.Store.ListRoleRules(ctx, "")
	if err != nil {
		return ops.OIDCProviderListResult{}, err
	}
	for _, row := range rows {
		res.Providers = append(res.Providers, info(oidc.FromRow(row, rules, 0), false))
	}
	for _, p := range res.Providers {
		if p.TLSInsecureSkipVerify && p.Enabled {
			res.InsecureTLS = true
		}
	}
	return res, nil
}

func info(c oidc.Config, readOnly bool) ops.OIDCProviderInfo {
	i := ops.OIDCProviderInfo{
		ID: c.ID, Name: c.Name, Issuer: c.Issuer, ClientID: c.ClientID,
		ClientSecretSet: c.ClientSecret != "", RoleClaim: c.RoleClaim,
		AdminValues: nonNil(c.AdminValues), UserValues: nonNil(c.UserValues),
		Scopes: strings.Join(c.Scopes, " "), CAFile: c.CAFile,
		TLSInsecureSkipVerify: c.InsecureSkipVerify, Enabled: !c.Disabled,
		Source: ops.OIDCSourcePanel, ReadOnly: readOnly,
	}
	if readOnly {
		i.Source = ops.OIDCSourceSettings
	}
	return i
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// ProviderSet creates, replaces or deletes a panel provider. The provider of
// the settings (id "default") is refused with a conflict. The audit record is
// written before the change, so no change exists without its record; when the
// change then fails, a second record says so.
func (h *OIDCHandlers) ProviderSet(ctx context.Context, actor OIDCActor, args ops.OIDCProviderSetArgs) (ops.WriteResult, error) {
	if actor.ID == "" {
		return ops.WriteResult{}, errors.New("api: OIDC change without an actor")
	}
	if err := args.Validate(); err != nil {
		return ops.WriteResult{}, err
	}
	action := AuditActionOIDCProviderSet
	if args.Delete {
		action = AuditActionOIDCProviderDelete
	}
	if args.ID == ops.OIDCDefaultProviderID {
		h.audit(ctx, actor, action, args.ID, AuditOutcomeRefuse)
		return ops.WriteResult{}, proto.NewError(proto.CodeConflict, SettingsProviderReadOnlyMessage)
	}
	if err := h.auditStrict(ctx, actor, action, args.ID, AuditOutcomeOK); err != nil {
		return ops.WriteResult{}, fmt.Errorf("api: provider %q was not changed, its audit record failed: %w", args.ID, err)
	}

	res := ops.WriteResult{Outcome: ops.OutcomeDelivered}
	if args.Delete {
		ok, err := h.Store.DeleteOIDCProvider(ctx, args.ID)
		if err != nil {
			h.failed(ctx, actor, action, args.ID)
			return ops.WriteResult{}, err
		}
		if !ok {
			return ops.WriteResult{}, proto.NewError(proto.CodeNotFound, "no such OIDC provider")
		}
		res.Warnings = append(res.Warnings, restartWarning())
		return res, nil
	}

	now := store.UnixMS(h.now())
	row := store.OIDCProviderRow{
		ID: args.ID, Name: strings.TrimSpace(args.Name), Issuer: strings.TrimSpace(args.Issuer),
		ClientID: strings.TrimSpace(args.ClientID), ClientSecret: args.ClientSecret,
		RoleClaim: strings.TrimSpace(args.RoleClaim), Scopes: strings.Join(strings.Fields(args.Scopes), " "),
		CAFile: strings.TrimSpace(args.CAFile), InsecureSkipVerify: args.TLSInsecureSkipVerify,
		Enabled: args.Enabled == nil || *args.Enabled, CreatedAt: now, UpdatedAt: now,
	}
	if err := h.Store.PutOIDCProvider(ctx, row); err != nil {
		h.failed(ctx, actor, action, args.ID)
		return ops.WriteResult{}, err
	}
	res.Warnings = append(res.Warnings, restartWarning())
	if row.InsecureSkipVerify {
		res.Warnings = append(res.Warnings, ops.Warning{Code: ops.WarnTLSVerificationOff,
			Message: "TLS verification of this provider is off; use a CA file instead. A permanent warning is shown."})
	}
	if w, ok := h.probe(ctx, row.ID); ok {
		res.Warnings = append(res.Warnings, w)
	}
	return res, nil
}

func restartWarning() ops.Warning {
	return ops.Warning{Code: ops.WarnLoginButtonAfterRestart, Message: "the login page lists the provider after the Hub restarts; sign-ins use the new settings at once"}
}

// probe tries discovery with the stored provider and reports a warning when it
// fails. The provider is stored either way: the IdP may be down only now.
func (h *OIDCHandlers) probe(ctx context.Context, id string) (ops.Warning, bool) {
	stored, err := h.Store.GetOIDCProvider(ctx, id)
	if err != nil {
		return ops.Warning{}, false
	}
	cfg := oidc.FromRow(stored, nil, h.Timeout)
	discover := h.Discover
	if discover == nil {
		discover = oidc.Discover
	}
	if _, err := discover(ctx, cfg); err != nil {
		return ops.Warning{Code: ops.WarnOIDCDiscoveryFailed,
			Message: "the provider was saved, but its discovery document could not be fetched now; check the issuer URL, the CA file and the network"}, true
	}
	return ops.Warning{}, false
}

// RoleRuleSet replaces the claim values that give a role for a panel provider.
func (h *OIDCHandlers) RoleRuleSet(ctx context.Context, actor OIDCActor, args ops.RoleRuleSetArgs) (ops.WriteResult, error) {
	if actor.ID == "" {
		return ops.WriteResult{}, errors.New("api: OIDC change without an actor")
	}
	if err := args.Validate(); err != nil {
		return ops.WriteResult{}, err
	}
	if args.ProviderID == ops.OIDCDefaultProviderID {
		h.audit(ctx, actor, AuditActionRoleRuleSet, args.ProviderID, AuditOutcomeRefuse)
		return ops.WriteResult{}, proto.NewError(proto.CodeConflict,
			"the rules of this provider are set by "+config.EnvPrefix+"OIDC_ADMIN_VALUES, "+config.EnvPrefix+"OIDC_USER_VALUES or the settings page")
	}
	if _, err := h.Store.GetOIDCProvider(ctx, args.ProviderID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return ops.WriteResult{}, proto.NewError(proto.CodeNotFound, "no such OIDC provider")
		}
		return ops.WriteResult{}, err
	}
	if err := h.auditStrict(ctx, actor, AuditActionRoleRuleSet, args.ProviderID, AuditOutcomeOK); err != nil {
		return ops.WriteResult{}, fmt.Errorf("api: role rule was not changed, its audit record failed: %w", err)
	}
	values := make([]string, 0, len(args.Values))
	for _, v := range args.Values {
		values = append(values, strings.TrimSpace(v))
	}
	if err := h.Store.ReplaceRoleRules(ctx, args.ProviderID, args.Role, values); err != nil {
		h.failed(ctx, actor, AuditActionRoleRuleSet, args.ProviderID)
		if errors.Is(err, store.ErrNotFound) {
			return ops.WriteResult{}, proto.NewError(proto.CodeNotFound, "no such OIDC provider")
		}
		return ops.WriteResult{}, err
	}
	return ops.WriteResult{Outcome: ops.OutcomeDelivered}, nil
}

func (h *OIDCHandlers) auditStrict(ctx context.Context, a OIDCActor, action, target, outcome string) error {
	return h.Audit.Record(ctx, audit.Event{Actor: a.ID, ActingAs: a.ActingAs, Action: action, Target: target, Outcome: outcome})
}

// audit records an event whose failure must not change the answer.
func (h *OIDCHandlers) audit(ctx context.Context, a OIDCActor, action, target, outcome string) {
	_ = h.auditStrict(context.WithoutCancel(ctx), a, action, target, outcome)
}

func (h *OIDCHandlers) failed(ctx context.Context, a OIDCActor, action, target string) {
	h.audit(ctx, a, action, target, AuditOutcomeFailed)
}
