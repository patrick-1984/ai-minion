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

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/hub/auth/session"
	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/ops"
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
	// Prefs are the account's own preferences (account_prefs); nil when the
	// Hub could not read them, so the Sessions card says so.
	Prefs *ops.AccountPrefsResult
	// PrefsURL is the post of the Sessions card; PrefsFor the effective user
	// the form was rendered for, which the post must still be (an act-as
	// session that starts or ends in between does not save the choice for
	// another account). The form is not offered while acting as someone else.
	PrefsURL string
	PrefsFor string
}

// HiddenSessionsHub is the sentence of the Sessions card on what the Hub
// setting hidden_sessions does to the person's choice.
func (p AccountPage) HiddenSessionsHub() string {
	if p.Prefs == nil {
		return ""
	}
	switch p.Prefs.HiddenSessions {
	case config.HiddenSessionsAllowed:
		return "An administrator allows hidden sessions for everybody on this Hub (Hub setting hidden_sessions), so this choice has no effect now."
	case config.HiddenSessionsDisabled:
		return "An administrator has disabled hidden sessions for everybody on this Hub (Hub setting hidden_sessions), so this choice has no effect now."
	}
	return ""
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
	if c, err := adminCaller(); err == nil {
		var prefs ops.AccountPrefsResult
		if err := c.CallOp(r, ops.OpAccountPrefs, ops.AccountPrefsArgs{}, &prefs); err == nil {
			p.Prefs = &prefs
			if u, ok := userOf(r); ok && sess.ActAs == nil {
				p.PrefsURL, p.PrefsFor = pathAccountSessions, u.ID
			}
		}
	}
	return p, nil
}

// pathAccountSessions is the post of the Sessions card of the account page.
const pathAccountSessions = "/account/sessions"

// handleAccountSessions saves the hidden-sessions preference of the signed-in
// person: the checkbox "allow_hidden_sessions" is on or absent.
//
// The form names the user it was rendered for (for_user); a post for another
// account than the current effective one is refused, so a tab opened while
// acting as someone, or before, cannot save the choice for the wrong person.
func handleAccountSessions(w http.ResponseWriter, r *http.Request, c OpCaller, u pageUser) {
	if sess, ok := session.FromContext(r.Context()); ok && sess.ActAs != nil {
		writeResultError(w, "While you act as another user, their hidden-sessions choice cannot be changed; they change it in their own Account page.")
		return
	}
	if r.PostFormValue("for_user") != u.ID {
		writeResultError(w, "This form was opened for another account (an act-as session started or ended since). Reload the page and choose again.")
		return
	}
	allow := r.PostFormValue("allow_hidden_sessions") == "on"
	text := "New sessions that would be hidden from your terminal are refused."
	if allow {
		text = "New sessions that would be hidden from your terminal are allowed."
	}
	writeOp(w, r, c, ops.OpAccountPrefsSet, ops.AccountPrefsSetArgs{WriteArgs: newWriteArgs(), AllowHiddenSessions: &allow}, text)
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
	{"Hub identity", pathAdminIdentity, "i-key", "The Hub key and certificate: state, expiry, and the passphrase operations (set the passphrase, re-issue the certificate, rotate the master key)."},
	{"Minion control", pathMinionControl, "i-sessions", "Master sessions, which Workers accept control from which (a matrix), user-managed Workers, and who can control a session."},
	{"Updates", pathAdminUpdates, "i-settings", "The Hub's release and the newest one in its update channel; update the Hub (Docker, opt-in) and the Workers."},
	{"Diagnostics", pathAdminDiagnostics, "i-panel", "Version, uptime, public URL, trusted proxies, connected Workers, and how the Hub sees your own request (peer, client IP, forwarding headers, effective HTTPS)."},
	{"Audit", pathAdminAudit, "i-audit", "Who did what and when. Events never contain secrets or terminal content."},
}

func adminOverviewData(*http.Request) (any, error) { return AdminOverviewPage{Areas: adminAreas}, nil }

func init() {
	RegisterPage(Page{Pattern: pathAccount, Title: "Account", Template: "account.html", Data: accountData})
	RegisterRoute(Route{Pattern: "POST " + pathAccountSessions, Handler: userRoute(handleAccountSessions)})
	RegisterPage(Page{Pattern: pathAdminOverview, Title: "Administration", Template: "admin.html",
		Roles: []authz.Role{authz.RoleAdmin}, Data: adminOverviewData})
}
