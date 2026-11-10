package web

// Administration pages, part 3: OIDC providers and their role rules (Story
// 6.8, R38, R42, R96, UX-DR8). The provider that the environment or the
// settings describe is shown read-only; panel providers can be edited. While
// TLS verification is off for an enabled provider, a warning that cannot be
// dismissed stays on the page.

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/ops"
)

// OIDCLockedText is the text of a provider that the environment or the
// settings page own; it is the message of the API conflict (R72).
const OIDCLockedText = "set by environment variable or the settings page"

// InsecureTLSWarning is the permanent warning of the page.
const InsecureTLSWarning = "TLS certificate verification is switched off for an OIDC provider. " +
	"Anyone who can intercept the connection to the identity provider can forge sign-ins, including admin sign-ins. " +
	"Use a CA file instead and turn this off."

// OIDCPage is what oidc.html sees as .Data.
type OIDCPage struct {
	Providers []OIDCRow
	// InsecureTLS is true while an enabled provider skips TLS verification.
	InsecureTLS bool
	// InsecureTLSWarning is the text of the permanent warning.
	InsecureTLSWarning string
	// NewProvider holds the defaults of the "add a provider" form.
	NewProvider OIDCRow
}

// OIDCRow is one provider as the page shows it.
type OIDCRow struct {
	ops.OIDCProviderInfo
	// AdminLines and UserLines are the role rule values, one per line, for the
	// textareas.
	AdminLines string
	UserLines  string
	// Editable is false for the provider of the environment or the settings.
	Editable bool
	// LockedText is why the provider is read-only.
	LockedText string
	// Prefix makes the ids of the form fields unique on the page.
	Prefix string
}

func oidcRows(res ops.OIDCProviderListResult) OIDCPage {
	page := OIDCPage{InsecureTLS: res.InsecureTLS, InsecureTLSWarning: InsecureTLSWarning,
		NewProvider: OIDCRow{Editable: true, Prefix: "new", OIDCProviderInfo: ops.OIDCProviderInfo{Enabled: true}}}
	for _, p := range res.Providers {
		row := OIDCRow{
			OIDCProviderInfo: p,
			AdminLines:       strings.Join(p.AdminValues, "\n"),
			UserLines:        strings.Join(p.UserValues, "\n"),
			Editable:         !p.ReadOnly,
			Prefix:           "p-" + p.ID,
		}
		if p.ReadOnly {
			row.LockedText = OIDCLockedText
		}
		page.Providers = append(page.Providers, row)
		if p.TLSInsecureSkipVerify && p.Enabled {
			page.InsecureTLS = true
		}
	}
	return page
}

func oidcData(r *http.Request, c OpCaller) (any, error) {
	var res ops.OIDCProviderListResult
	if err := c.CallOp(r, ops.OpOIDCProviderList, ops.OIDCProviderListArgs{}, &res); err != nil {
		return nil, err
	}
	return oidcRows(res), nil
}

// formLines splits a textarea into its non-empty trimmed lines.
func formLines(s string) []string {
	out := []string{}
	for _, l := range strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}

// handleOIDCSave creates or replaces a panel provider. An empty client secret
// keeps the stored one. The form always shows the "enabled" and the insecure-TLS
// checkboxes, so an absent field means unchecked.
func handleOIDCSave(w http.ResponseWriter, r *http.Request, c OpCaller) {
	enabled := r.PostFormValue("enabled") != ""
	id := strings.TrimSpace(r.PostFormValue("id"))
	args := ops.OIDCProviderSetArgs{
		WriteArgs:             newWriteArgs(),
		ID:                    id,
		Name:                  strings.TrimSpace(r.PostFormValue("name")),
		Issuer:                strings.TrimSpace(r.PostFormValue("issuer")),
		ClientID:              strings.TrimSpace(r.PostFormValue("client_id")),
		ClientSecret:          r.PostFormValue("client_secret"),
		RoleClaim:             strings.TrimSpace(r.PostFormValue("role_claim")),
		Scopes:                strings.TrimSpace(r.PostFormValue("scopes")),
		CAFile:                strings.TrimSpace(r.PostFormValue("ca_file")),
		TLSInsecureSkipVerify: r.PostFormValue("tls_insecure_skip_verify") != "",
		Enabled:               &enabled,
	}
	writeOp(w, r, c, ops.OpOIDCProviderSet, args, "The provider "+id+" is saved.")
}

func handleOIDCDelete(w http.ResponseWriter, r *http.Request, c OpCaller) {
	id := r.PathValue("id")
	args := ops.OIDCProviderSetArgs{WriteArgs: newWriteArgs(), ID: id, Delete: true}
	writeOp(w, r, c, ops.OpOIDCProviderSet, args, "The provider "+id+" is deleted.")
}

// handleOIDCRoles replaces both role rules of a provider: the claim values
// that give the admin role and the ones that give the user role. Both rules are
// validated with the operation's own checks before the first one is written,
// so a bad value in the second field cannot leave the first one changed. If a
// write still fails after the first one was applied, the message says so.
func handleOIDCRoles(w http.ResponseWriter, r *http.Request, c OpCaller) {
	id := r.PathValue("id")
	var calls []ops.RoleRuleSetArgs
	for _, rule := range []struct{ role, field string }{
		{ops.RoleAdmin, "admin_values"}, {ops.RoleUser, "user_values"},
	} {
		args := ops.RoleRuleSetArgs{WriteArgs: newWriteArgs(), ProviderID: id, Role: rule.role, Values: formLines(r.PostFormValue(rule.field))}
		if err := args.Validate(); err != nil {
			writeResultError(w, fmt.Sprintf("Nothing was saved. The %s values are not valid: at most %d lines, each at most %d bytes of plain text.",
				rule.role, ops.MaxRoleRuleValues, ops.MaxRoleRuleValueLen))
			return
		}
		calls = append(calls, args)
	}
	var last ops.WriteResult
	for i, args := range calls {
		var res ops.WriteResult
		if err := c.CallOp(r, ops.OpRoleRuleSet, args, &res); err != nil {
			text := failureText(err)
			if i > 0 {
				text = "The " + calls[0].Role + " rule is saved, but the " + args.Role + " rule is not: " + text
			}
			writeResultError(w, text)
			return
		}
		last.Warnings = append(last.Warnings, res.Warnings...)
	}
	done(w, r, "The role rules of "+id+" are saved.", last)
}

func init() {
	RegisterPage(Page{Pattern: pathAdminOIDC, Title: "Identity & sign-in", Template: "oidc.html", Data: adminPageData(oidcData)})
	RegisterNav(NavItem{Label: "Identity & sign-in", Path: pathAdminOIDC, Order: adminNavOrder + 20, Roles: []authz.Role{authz.RoleAdmin}, Group: NavGroupAdmin, Icon: "i-shield"})
	for _, rt := range []struct {
		pattern string
		fn      func(http.ResponseWriter, *http.Request, OpCaller)
	}{
		{"POST " + pathAdminOIDC + "/providers", handleOIDCSave},
		{"POST " + pathAdminOIDC + "/providers/{id}/delete", handleOIDCDelete},
		{"POST " + pathAdminOIDC + "/providers/{id}/roles", handleOIDCRoles},
	} {
		RegisterRoute(Route{Pattern: rt.pattern, Handler: adminRoute(rt.fn)})
	}
}
