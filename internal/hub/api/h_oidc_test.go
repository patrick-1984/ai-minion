package api

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/audit"
	"github.com/patrick-1984/ai-minion/internal/hub/auth/oidc"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/logx"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

type recAudit struct {
	mu     sync.Mutex
	events []audit.Event
	err    error
}

func (r *recAudit) Record(_ context.Context, ev audit.Event) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return r.err
	}
	r.events = append(r.events, ev)
	return nil
}

func (r *recAudit) last() audit.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.events) == 0 {
		return audit.Event{}
	}
	return r.events[len(r.events)-1]
}

func newHandlers(t *testing.T) (*OIDCHandlers, *store.Store, *recAudit) {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	aud := &recAudit{}
	h := &OIDCHandlers{
		Store: st, Audit: aud,
		Discover: func(context.Context, oidc.Config) (*oidc.Provider, error) { return nil, nil },
		Now:      func() time.Time { return time.UnixMilli(5000) },
	}
	return h, st, aud
}

var actor = OIDCActor{ID: "admin-1"}

func setArgs(id string) ops.OIDCProviderSetArgs {
	return ops.OIDCProviderSetArgs{
		WriteArgs: ops.WriteArgs{RequestID: ops.NewUUIDv7()}, ID: id, Name: "Acme", Issuer: "https://idp.example",
		ClientID: "cid", ClientSecret: "panel-secret-value", RoleClaim: "groups",
	}
}

func codeOf(err error) proto.Code {
	var pe *proto.Error
	if errors.As(err, &pe) {
		return pe.Code
	}
	return ""
}

func warningCodes(r ops.WriteResult) []string {
	var out []string
	for _, w := range r.Warnings {
		out = append(out, w.Code)
	}
	return out
}

func TestProviderSetCreatesListsAndNeverReturnsTheSecret(t *testing.T) {
	h, st, aud := newHandlers(t)
	ctx := context.Background()
	res, err := h.ProviderSet(ctx, actor, setArgs("acme"))
	if err != nil || res.Outcome != ops.OutcomeDelivered {
		t.Fatalf("set: %+v, %v", res, err)
	}
	if !slices.Equal(warningCodes(res), []string{ops.WarnLoginButtonAfterRestart}) {
		t.Errorf("warnings = %v", warningCodes(res))
	}
	if ev := aud.last(); ev.Action != AuditActionOIDCProviderSet || ev.Target != "acme" || ev.Outcome != AuditOutcomeOK || ev.Actor != "admin-1" {
		t.Errorf("audit = %+v", ev)
	}
	row, err := st.GetOIDCProvider(ctx, "acme")
	if err != nil || row.ClientSecret != "panel-secret-value" || !row.Enabled || row.CreatedAt != 5000 {
		t.Fatalf("stored: %+v, %v", row, err)
	}

	if _, err := h.RoleRuleSet(ctx, actor, ops.RoleRuleSetArgs{WriteArgs: ops.WriteArgs{RequestID: ops.NewUUIDv7()},
		ProviderID: "acme", Role: "admin", Values: []string{" g1 ", "g2"}}); err != nil {
		t.Fatal(err)
	}
	list, err := h.ProviderList(ctx, ops.OIDCProviderListArgs{})
	if err != nil || len(list.Providers) != 1 {
		t.Fatalf("list: %+v, %v", list, err)
	}
	p := list.Providers[0]
	if p.ID != "acme" || !p.ClientSecretSet || p.Source != ops.OIDCSourcePanel || p.ReadOnly || !p.Enabled ||
		!slices.Equal(p.AdminValues, []string{"g1", "g2"}) || len(p.UserValues) != 0 || list.InsecureTLS {
		t.Errorf("info = %+v", p)
	}

	// Update without a secret keeps it; disabling hides nothing from the list.
	upd := setArgs("acme")
	upd.ClientSecret = ""
	f := false
	upd.Enabled = &f
	if _, err := h.ProviderSet(ctx, actor, upd); err != nil {
		t.Fatal(err)
	}
	row, _ = st.GetOIDCProvider(ctx, "acme")
	if row.ClientSecret != "panel-secret-value" || row.Enabled {
		t.Errorf("after update: %+v", row)
	}
}

func TestProviderListShowsTheSettingsProviderReadOnlyAndInsecureTLS(t *testing.T) {
	h, _, _ := newHandlers(t)
	h.Settings = func(context.Context) (oidc.Config, bool, error) {
		return oidc.Config{ID: "default", Name: "Env IdP", Issuer: "https://env", ClientID: "c", ClientSecret: logx.Secret("s"),
			RoleClaim: "groups", AdminValues: []string{"a"}, InsecureSkipVerify: true}, true, nil
	}
	list, err := h.ProviderList(context.Background(), ops.OIDCProviderListArgs{})
	if err != nil || len(list.Providers) != 1 {
		t.Fatalf("%+v %v", list, err)
	}
	p := list.Providers[0]
	if p.Source != ops.OIDCSourceSettings || !p.ReadOnly || !p.ClientSecretSet || !p.TLSInsecureSkipVerify || !list.InsecureTLS ||
		!slices.Equal(p.AdminValues, []string{"a"}) || p.UserValues == nil {
		t.Errorf("info = %+v insecure=%v", p, list.InsecureTLS)
	}
	h.Settings = func(context.Context) (oidc.Config, bool, error) { return oidc.Config{}, false, nil }
	if list, _ = h.ProviderList(context.Background(), ops.OIDCProviderListArgs{}); len(list.Providers) != 0 || list.Providers == nil {
		t.Errorf("no provider: %+v", list)
	}
	h.Settings = func(context.Context) (oidc.Config, bool, error) {
		return oidc.Config{}, false, errors.New("bad settings")
	}
	if _, err := h.ProviderList(context.Background(), ops.OIDCProviderListArgs{}); err == nil {
		t.Error("a settings error was swallowed")
	}
}

func TestSettingsProviderIsReadOnly(t *testing.T) {
	h, _, aud := newHandlers(t)
	ctx := context.Background()
	if _, err := h.ProviderSet(ctx, actor, setArgs("default")); codeOf(err) != proto.CodeConflict {
		t.Fatalf("set default: %v", err)
	}
	if aud.last().Outcome != AuditOutcomeRefuse {
		t.Errorf("refusal not audited: %+v", aud.last())
	}
	del := setArgs("default")
	del.Delete = true
	if _, err := h.ProviderSet(ctx, actor, del); codeOf(err) != proto.CodeConflict {
		t.Fatalf("delete default: %v", err)
	}
	rr := ops.RoleRuleSetArgs{WriteArgs: ops.WriteArgs{RequestID: ops.NewUUIDv7()}, ProviderID: "default", Role: "admin", Values: []string{"x"}}
	if _, err := h.RoleRuleSet(ctx, actor, rr); codeOf(err) != proto.CodeConflict {
		t.Fatalf("rules of default: %v", err)
	}
}

func TestProviderSetWarnings(t *testing.T) {
	h, _, _ := newHandlers(t)
	ctx := context.Background()
	a := setArgs("insecure")
	a.TLSInsecureSkipVerify = true
	res, err := h.ProviderSet(ctx, actor, a)
	if err != nil || !slices.Contains(warningCodes(res), ops.WarnTLSVerificationOff) {
		t.Fatalf("insecure: %v %v", warningCodes(res), err)
	}
	h.Discover = func(context.Context, oidc.Config) (*oidc.Provider, error) {
		return nil, errors.New("dial tcp 10.0.0.1: refused")
	}
	res, err = h.ProviderSet(ctx, actor, setArgs("down"))
	if err != nil || !slices.Contains(warningCodes(res), ops.WarnOIDCDiscoveryFailed) {
		t.Fatalf("discovery: %v %v", warningCodes(res), err)
	}
	for _, w := range res.Warnings {
		if strings.Contains(w.Message, "10.0.0.1") {
			t.Errorf("warning leaks the network error: %q", w.Message)
		}
	}
}

func TestProviderSetDeleteAndNotFound(t *testing.T) {
	h, st, _ := newHandlers(t)
	ctx := context.Background()
	del := setArgs("acme")
	del.Delete = true
	if _, err := h.ProviderSet(ctx, actor, del); codeOf(err) != proto.CodeNotFound {
		t.Fatalf("delete unknown: %v", err)
	}
	_, _ = h.ProviderSet(ctx, actor, setArgs("acme"))
	if res, err := h.ProviderSet(ctx, actor, del); err != nil || res.Outcome != ops.OutcomeDelivered {
		t.Fatalf("delete: %+v %v", res, err)
	}
	if _, err := st.GetOIDCProvider(ctx, "acme"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("still there: %v", err)
	}
	rr := ops.RoleRuleSetArgs{WriteArgs: ops.WriteArgs{RequestID: ops.NewUUIDv7()}, ProviderID: "acme", Role: "user", Values: []string{"x"}}
	if _, err := h.RoleRuleSet(ctx, actor, rr); codeOf(err) != proto.CodeNotFound {
		t.Fatalf("rule for a missing provider: %v", err)
	}
}

func TestChangesNeedAnAuditRecord(t *testing.T) {
	h, st, aud := newHandlers(t)
	ctx := context.Background()
	aud.err = errors.New("audit down")
	if _, err := h.ProviderSet(ctx, actor, setArgs("acme")); err == nil {
		t.Fatal("a change without an audit record succeeded")
	}
	if rows, _ := st.ListOIDCProviders(ctx); len(rows) != 0 {
		t.Fatalf("the provider was stored anyway: %v", rows)
	}
	aud.err = nil
	_, _ = h.ProviderSet(ctx, actor, setArgs("acme"))
	aud.err = errors.New("audit down")
	rr := ops.RoleRuleSetArgs{WriteArgs: ops.WriteArgs{RequestID: ops.NewUUIDv7()}, ProviderID: "acme", Role: "user", Values: []string{"x"}}
	if _, err := h.RoleRuleSet(ctx, actor, rr); err == nil {
		t.Fatal("a rule change without an audit record succeeded")
	}
	if rules, _ := st.ListRoleRules(ctx, ""); len(rules) != 0 {
		t.Fatalf("rules stored anyway: %v", rules)
	}
}

func TestValidationAndActor(t *testing.T) {
	h, _, aud := newHandlers(t)
	ctx := context.Background()
	if _, err := h.ProviderSet(ctx, OIDCActor{}, setArgs("acme")); err == nil {
		t.Error("no actor accepted")
	}
	bad := setArgs("acme")
	bad.Issuer = "ftp://x"
	if _, err := h.ProviderSet(ctx, actor, bad); !errors.Is(err, ops.ErrInvalidArgument) {
		t.Errorf("bad issuer: %v", err)
	}
	if _, err := h.RoleRuleSet(ctx, actor, ops.RoleRuleSetArgs{}); !errors.Is(err, ops.ErrInvalidArgument) {
		t.Errorf("bad rule args: %v", err)
	}
	if _, err := h.RoleRuleSet(ctx, OIDCActor{}, ops.RoleRuleSetArgs{}); err == nil {
		t.Error("no actor accepted")
	}
	if aud.last() != (audit.Event{}) {
		t.Errorf("an invalid request was audited: %+v", aud.last())
	}
	for _, ev := range []audit.Event{{Actor: "a", Action: AuditActionOIDCProviderSet, Target: "acme", Outcome: "ok"}} {
		if err := ev.Validate(); err != nil {
			t.Errorf("audit vocabulary is not valid audit input: %v", err)
		}
	}
}
