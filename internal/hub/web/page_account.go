package web

// The account page and the administration overview of the workbench shell.
//
// Account (/account) shows what the session itself knows about the signed-in
// person: the name, the role, an acting-as state and when the session ends,
// and links to pages that exist (change password, API keys). It does not show
// how the person signed in: the session records no authentication method, so
// the page has nothing true to say about it (see docs/design/workbench).
//
// The administration overview (/admin) lists the administration areas with a
// plain sentence each; it is a page for administrators and is also forbidden
// for everybody else by the role check of the page registry.

import (
	"net/http"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/auth/session"
	"github.com/patrick-1984/ai-minion/internal/hub/authz"
)

const pathAccount = "/account"

// AccountPage is what account.html sees as .Data.
type AccountPage struct {
	Username  string
	RoleLabel string
	Initials  string
	// ActingAs is the name of the user an admin currently acts as; empty
	// otherwise.
	ActingAs string
	// ExpiresAt is when the browser session ends; zero when unknown.
	ExpiresAt time.Time
	Admin     bool
	// MustChangePassword is true when the account has to choose a new password.
	MustChangePassword bool
}

func accountData(r *http.Request) (any, error) {
	sess, ok := session.FromContext(r.Context())
	if !ok {
		return nil, ErrNotFound()
	}
	p := AccountPage{
		Username: sess.Username, RoleLabel: roleLabel(sess.Role), Initials: initialsOf(sess.Username),
		ExpiresAt: sess.ExpiresAt.UTC(), Admin: sess.Role == authz.RoleAdmin, MustChangePassword: sess.MustChangePassword,
	}
	if sess.ActAs != nil {
		p.ActingAs = sess.ActAs.TargetUsername
	}
	return p, nil
}

// AdminArea is one card of the administration overview.
type AdminArea struct {
	Label string
	Path  string
	Icon  string
	Text  string
}

// AdminOverviewPage is what admin.html sees as .Data.
type AdminOverviewPage struct {
	Areas []AdminArea
}

// Crumbs implements [Crumbed]: the overview is the page of the Administration
// heading, so its own breadcrumb ends there.
func (AdminOverviewPage) Crumbs() []Crumb {
	return []Crumb{{Label: "Control center", Path: "/"}, {Label: "Administration"}}
}

var adminAreas = []AdminArea{
	{"Users", pathAdminUsers, "i-users", "Create local accounts, see roles and sign-in state, reset passwords, deactivate accounts or act as a user."},
	{"Hub settings", pathAdminSettings, "i-settings", "Every setting of the Hub with its source, whether it applies live, and whether a restart is pending."},
	{"Identity & sign-in", pathAdminOIDC, "i-shield", "OpenID Connect providers and the rules that turn provider claims into roles."},
	{"Audit", pathAdminAudit, "i-audit", "Who did what and when. Events never contain secrets or terminal content."},
}

func adminOverviewData(*http.Request) (any, error) { return AdminOverviewPage{Areas: adminAreas}, nil }

func init() {
	RegisterPage(Page{Pattern: pathAccount, Title: "Account", Template: "account.html", Data: accountData})
	RegisterPage(Page{Pattern: pathAdminOverview, Title: "Administration", Template: "admin.html",
		Roles: []authz.Role{authz.RoleAdmin}, Data: adminOverviewData})
}
