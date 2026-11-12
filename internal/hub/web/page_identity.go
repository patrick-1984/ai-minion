package web

// Administration page "Hub identity" (contract docs/architecture/proposals/
// hub-identity-v2.md, section 8.2) and the expiry banner and countdown
// (section 8.4).
//
// The page shows the state of the Hub identity from the database and offers
// the four operations. It performs none itself: its script (identity.js) sends
// each one as a JSON POST of the browser to the REST operation of the
// catalog, so the server checks the administrator role, the fresh sign-in, the
// CSRF token, the same-origin headers, the effective HTTPS and the lockout.
// The page never echoes a passphrase: the fields are cleared after every
// submission, the script keeps nothing, and the Hub puts none in any response,
// log line or audit event.

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/auth/session"
	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/ops"
)

const pathAdminIdentity = "/admin/identity"

// IdentityStatus is what the banner and the Workers countdown need from the
// Hub identity service. The Hub's identity module supplies it.
type IdentityStatus struct {
	// Label is the bootstrap state ("B3 established").
	Label string
	// Serving is true when Workers can connect.
	Serving     bool
	Established bool
	Passphrase  bool
	Migrated    bool
	NotAfter    time.Time
	DaysLeft    int
	// Warning is "", "warning", "error" or "expired".
	Warning string
}

// IdentityStatusFunc returns the (briefly cached) identity status, or false
// when it is unknown.
type IdentityStatusFunc func(ctx context.Context) (IdentityStatus, bool)

type identitySourceBox struct{ f IdentityStatusFunc }

var identitySource atomic.Pointer[identitySourceBox]

// SetIdentityStatus connects the banner and the countdown to the identity
// service. It returns the function that disconnects it again, which only takes
// effect while f is still the connected source.
func SetIdentityStatus(f IdentityStatusFunc) (release func()) {
	b := &identitySourceBox{f: f}
	identitySource.Store(b)
	return func() { identitySource.CompareAndSwap(b, nil) }
}

func currentIdentityStatus(ctx context.Context) (IdentityStatus, bool) {
	b := identitySource.Load()
	if b == nil || b.f == nil {
		return IdentityStatus{}, false
	}
	return b.f(ctx)
}

// adminSession returns the session of r when its user is an administrator
// acting as themselves.
func adminSession(r *http.Request) (*session.Session, bool) {
	sess, ok := session.FromContext(r.Context())
	if !ok || sess.Role != authz.RoleAdmin || sess.ActAs != nil {
		return nil, false
	}
	return sess, true
}

func dateText(t time.Time) string { return t.UTC().Format("2006-01-02") }

func daysText(n int) string {
	if n == 1 {
		return "1 day"
	}
	return fmt.Sprintf("%d days", n)
}

// identityBanner is the banner on every page for administrators: the Hub does
// not serve Workers (no passphrase set yet, or no certificate), or the
// certificate expires within 30 days or has expired.
func identityBanner(r *http.Request) []Banner {
	if _, ok := adminSession(r); !ok {
		return nil
	}
	st, ok := currentIdentityStatus(r.Context())
	if !ok {
		return nil
	}
	switch {
	case strings.HasPrefix(st.Label, "B0"), strings.HasPrefix(st.Label, "B1"):
		return []Banner{{Kind: "warning", Text: "Set the Hub passphrase: until you do, Workers cannot connect. Open Administration, then Hub identity."}}
	case strings.HasPrefix(st.Label, "B2"):
		return []Banner{{Kind: "warning", Text: "The Hub has no certificate yet, so Workers cannot connect. Open Administration, then Hub identity, and re-issue the certificate."}}
	case st.Warning == "expired":
		return []Banner{{Kind: "danger", Text: "The Hub certificate expired on " + dateText(st.NotAfter) + ". Workers are refused until it is re-issued. Open Administration, then Hub identity."}}
	case st.Warning == "error":
		return []Banner{{Kind: "danger", Text: "The Hub certificate expires on " + dateText(st.NotAfter) + " (" + daysText(st.DaysLeft) + "). Re-issue it."}}
	case st.Warning == "warning":
		return []Banner{{Kind: "warning", Text: "The Hub certificate expires on " + dateText(st.NotAfter) + " (" + daysText(st.DaysLeft) + "). Re-issue it."}}
	}
	return nil
}

// workersCountdown is the line in the header of the Workers page for an
// administrator.
func workersCountdown(r *http.Request) (text, tone string) {
	if _, ok := adminSession(r); !ok {
		return "", ""
	}
	st, ok := currentIdentityStatus(r.Context())
	if !ok || !st.Established {
		return "", ""
	}
	switch st.Warning {
	case "expired":
		return "Hub certificate expired on " + dateText(st.NotAfter) + ": Workers are refused.", "danger"
	case "error":
		return "Hub certificate: " + daysText(st.DaysLeft) + " left (expires " + dateText(st.NotAfter) + ").", "danger"
	case "warning":
		return "Hub certificate: " + daysText(st.DaysLeft) + " left (expires " + dateText(st.NotAfter) + ").", "warning"
	}
	return "Hub certificate valid until " + dateText(st.NotAfter) + " (" + daysText(st.DaysLeft) + " left).", "ok"
}

// IdentityPage is what identity.html sees as .Data.
type IdentityPage struct {
	Status ops.HubIdentityStatusResult
	// CanSetPassphrase is true only while no sealed root exists (B0, B1).
	CanSetPassphrase bool
	// CanReissue is true when a sealed root exists (B2, B3); CanRotate and
	// CanChange follow the states the service accepts.
	CanReissue, CanRotate, CanChange bool
	// Fresh is true when the session signed in within the last five minutes;
	// FreshUntil is when that ends.
	Fresh      bool
	FreshUntil time.Time
	// Method is "local" or "oidc": which re-authentication to offer.
	Method string
	// Reauth is the result of a re-authentication that returned to the page:
	// "", "ok" or "unavailable".
	Reauth string
	// ValidFrom, ValidUntil, LastOpAt, LockedUntil are formatted times.
	ValidFrom, ValidUntil, LastOpAt string
	// WebLocked is true while the web scope is locked; WebLockedUntil is the
	// unix time of the end (seconds) and WebRetry the whole seconds left.
	WebLocked      bool
	WebLockedUntil int64
	WebRetry       int64
	WebAttempts    int64
	RootShort      string
}

// Crumbs implements [Crumbed].
func (IdentityPage) Crumbs() []Crumb {
	return []Crumb{{Label: "Control center", Path: "/"}, {Label: "Administration", Path: pathAdminOverview}, {Label: "Hub identity"}}
}

func timeText(t *ops.Timestamp) string {
	if t == nil {
		return ""
	}
	return t.Time().Format("2006-01-02 15:04 UTC")
}

func identityData(r *http.Request, c OpCaller) (any, error) {
	var st ops.HubIdentityStatusResult
	if err := c.CallOp(r, ops.OpHubIdentityStatus, ops.HubIdentityStatusArgs{}, &st); err != nil {
		return nil, err
	}
	p := IdentityPage{Status: st}
	p.CanSetPassphrase = st.State == "B0" || st.State == "B1"
	p.CanReissue = st.State == "B2" || st.State == "B3"
	p.CanRotate = st.State == "B3"
	p.CanChange = st.State == "B2" || st.State == "B3"
	if sess, ok := session.FromContext(r.Context()); ok {
		now := time.Now()
		p.Fresh = sess.Fresh(now)
		if p.Fresh {
			p.FreshUntil = sess.AuthAt.Add(session.ReauthWindow)
		}
		p.Method = "local"
		if strings.HasPrefix(sess.AuthMethod, session.AuthMethodOIDCPrefix) {
			p.Method = "oidc"
		}
	}
	switch r.URL.Query().Get("reauth") {
	case "ok":
		p.Reauth = "ok"
	case "unavailable":
		p.Reauth = "unavailable"
	}
	p.ValidFrom, p.ValidUntil, p.LastOpAt = timeText(st.NotBefore), timeText(st.NotAfter), timeText(st.LastOpAt)
	if st.Web.Locked && st.Web.LockedUntil != nil {
		p.WebLocked = true
		p.WebLockedUntil = st.Web.LockedUntil.Time().Unix()
		p.WebRetry = st.Web.RetryAfterSecs
	}
	p.WebAttempts = st.Web.AttemptsThisHour
	if len(st.RootFingerprint) > 12 {
		p.RootShort = st.RootFingerprint[:12]
	}
	return p, nil
}

func init() {
	RegisterPage(Page{Pattern: pathAdminIdentity, Title: "Hub identity", Template: "identity.html", Data: adminPageData(identityData)})
	RegisterNav(NavItem{Label: "Hub identity", Path: pathAdminIdentity, Order: adminNavOrder + 15, Roles: []authz.Role{authz.RoleAdmin}, Group: NavGroupAdmin, Icon: "i-key"})
	RegisterBanner(identityBanner)
}
