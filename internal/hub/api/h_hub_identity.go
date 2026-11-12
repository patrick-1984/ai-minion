package api

import (
	"context"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/auth/session"
	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/hub/hubid"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// IdentityService is the port to the Hub identity service (internal/hub/hubid);
// *hubid.Service implements it.
type IdentityService interface {
	SetPassphrase(ctx context.Context, actor hubid.Actor, pass, confirm string, opts hubid.IssueOptions) (hubid.Result, error)
	Reissue(ctx context.Context, actor hubid.Actor, pass string, opts hubid.IssueOptions) (hubid.Result, error)
	RotateMaster(ctx context.Context, actor hubid.Actor, oldPass, newPass, confirm string, opts hubid.IssueOptions) (hubid.Result, error)
	ChangePassphrase(ctx context.Context, actor hubid.Actor, oldPass, newPass, confirm string) (hubid.Result, error)
	Status(ctx context.Context) (hubid.Status, error)
}

var _ IdentityService = (*hubid.Service)(nil)

// Warning codes of the identity operations that are not in the ops package.
const (
	// WarnFilesPending says the change is durable and active but a file
	// rename or directory sync is pending; further identity changes answer
	// conflict until a retry succeeds.
	WarnFilesPending = "files_pending"
)

// IdentityOps runs the Hub identity operations of the catalog on a
// [IdentityService] and maps every service error to an API error. The REST
// handlers and the local admin socket share it, so both answer alike.
type IdentityOps struct {
	Svc IdentityService
	Log interface {
		Error(msg string, args ...any)
	}
}

func issueOptions(m ops.IssueMembers) hubid.IssueOptions {
	return hubid.IssueOptions{ValidityDays: m.ValidityDays, SerialFloor: m.SerialFloor, AllowFarFloor: m.AllowFarFloor}
}

// SetPassphrase runs hub_identity_set_passphrase.
func (o IdentityOps) SetPassphrase(ctx context.Context, actor hubid.Actor, a ops.HubIdentitySetPassphraseArgs) (ops.HubIdentityResult, error) {
	res, err := o.Svc.SetPassphrase(ctx, actor, a.Passphrase, a.Confirm, issueOptions(a.IssueMembers))
	return o.result(res, err, nil)
}

// Reissue runs hub_identity_reissue.
func (o IdentityOps) Reissue(ctx context.Context, actor hubid.Actor, a ops.HubIdentityReissueArgs) (ops.HubIdentityResult, error) {
	res, err := o.Svc.Reissue(ctx, actor, a.Passphrase, issueOptions(a.IssueMembers))
	return o.result(res, err, nil)
}

// RotateMaster runs hub_identity_rotate.
func (o IdentityOps) RotateMaster(ctx context.Context, actor hubid.Actor, a ops.HubIdentityRotateArgs) (ops.HubIdentityResult, error) {
	res, err := o.Svc.RotateMaster(ctx, actor, a.Passphrase, a.NewPassphrase, a.Confirm, issueOptions(a.IssueMembers))
	warn := []ops.Warning{{Code: ops.WarnInvitationsRevoked, Message: "all outstanding invitation keys were revoked; create new ones"}}
	return o.result(res, err, warn)
}

// ChangePassphrase runs hub_identity_change_passphrase.
func (o IdentityOps) ChangePassphrase(ctx context.Context, actor hubid.Actor, a ops.HubIdentityChangePassphraseArgs) (ops.HubIdentityResult, error) {
	res, err := o.Svc.ChangePassphrase(ctx, actor, a.Passphrase, a.NewPassphrase, a.Confirm)
	return o.result(res, err, nil)
}

// Status runs hub_identity_status.
func (o IdentityOps) Status(ctx context.Context) (ops.HubIdentityStatusResult, error) {
	st, err := o.Svc.Status(ctx)
	if err != nil {
		return ops.HubIdentityStatusResult{}, o.fail(err)
	}
	return statusResult(st), nil
}

func ts(t time.Time) *ops.Timestamp {
	if t.IsZero() || t.UnixMilli() <= 0 {
		return nil
	}
	v := ops.Timestamp(t)
	return &v
}

func scopeResult(s hubid.ScopeStatus, now time.Time) ops.IdentityScopeStatus {
	out := ops.IdentityScopeStatus{Failures: s.Failures, Locked: s.Locked, AttemptsThisHour: s.AttemptsThisHour}
	if s.Locked {
		out.LockedUntil = ts(s.LockedUntil)
		if wait := s.LockedUntil.Sub(now); wait > 0 {
			out.RetryAfterSecs = int64((wait + time.Second - 1) / time.Second)
		}
	}
	return out
}

func statusResult(st hubid.Status) ops.HubIdentityStatusResult {
	now := time.Now()
	label := st.State.String()
	state := "B?"
	if i := strings.IndexByte(label, ' '); i > 0 {
		state = label[:i]
	}
	out := ops.HubIdentityStatusResult{
		State: state, StateLabel: label, PassphraseSet: st.PassphraseSet, Serving: st.Serving,
		RootFingerprint: st.RootFingerprint, OpFingerprint: st.OpFingerprint, Serial: st.Serial,
		NotBefore: ts(st.NotBefore), NotAfter: ts(st.NotAfter), DaysLeft: st.DaysLeft,
		Warning: string(st.Warning), Expired: st.Expired, MigratedFrom06: st.MigratedFrom06,
		LastOp: st.LastOp, LastOpAt: ts(st.LastOpAt),
		Web: scopeResult(st.Web, now), CLI: scopeResult(st.CLI, now), Generation: st.Generation,
	}
	return out
}

// result maps a service result and error. A change that is durable and active
// but whose file step is pending is a success with a warning: reporting an
// error would invite a second operation.
func (o IdentityOps) result(res hubid.Result, err error, warn []ops.Warning) (ops.HubIdentityResult, error) {
	out := ops.HubIdentityResult{Warnings: []ops.Warning{}}
	if err != nil {
		if !errors.Is(err, hubid.ErrFilesPending) || res.Kind == "" {
			return out, o.fail(err)
		}
		out.Warnings = append(out.Warnings, ops.Warning{Code: WarnFilesPending,
			Message: "the change is active, but a file step is pending; repeat the command to finish it"})
	}
	out.Kind, out.Serial = res.Kind, res.Serial
	out.NotBefore, out.NotAfter = ops.Timestamp(res.NotBefore), ops.Timestamp(res.NotAfter)
	out.RootFingerprint, out.OpFingerprint, out.Generation = res.RootFingerprint, res.OpFingerprint, res.Generation
	out.Warnings = append(out.Warnings, warn...)
	return out, nil
}

// fail maps a service error to an API error. Messages are the generic texts
// of section 10.5; none contains a passphrase, its length or a cause.
func (o IdentityOps) fail(err error) error {
	var he *hubid.Error
	if !errors.As(err, &he) {
		o.logError("hub identity operation failed", err)
		return internalError()
	}
	switch he.Code {
	case hubid.CodeConflict:
		return NewError(proto.CodeConflict, conflictText(he))
	case hubid.CodeInvalidArgument:
		return NewError(proto.CodeInvalidArgument, safeText(he.Msg))
	case hubid.CodePassphraseInvalid:
		return NewError(proto.CodePassphraseInvalid, "The passphrase was not accepted.")
	case hubid.CodeRateLimited:
		e := NewError(proto.CodeRateLimited, "Too many passphrase attempts; try again later.")
		kind := "locked"
		if he.Allowance {
			kind = "allowance"
			e.Message = "The hourly limit of identity operations is used up; try again later."
		}
		d := map[string]any{"limit": kind}
		if !he.RetryAt.IsZero() {
			d["retry_at"] = he.RetryAt.UTC().Format("2006-01-02T15:04:05.000Z")
			if wait := time.Until(he.RetryAt); wait > 0 {
				e.RetryAfter = wait
			}
		}
		e.Details = d
		return e
	case hubid.CodeNotDurable:
		return NewError(proto.CodeConflict, "The change could not be confirmed as durable; repeat the same request to complete it.").
			WithDetails(map[string]any{"repeat": true})
	case hubid.CodeFilesPending:
		return NewError(proto.CodeConflict, "A previous identity change still has a file step pending; repeat it to finish.")
	}
	o.logError("hub identity operation failed", err)
	return internalError()
}

func (o IdentityOps) logError(msg string, err error) {
	if o.Log != nil {
		// Only the error class and message: the service never puts a
		// passphrase into an error.
		o.Log.Error(msg, "error", err)
	}
}

// conflictText is the message of a conflict; the service's own text names the
// state ("a sealed root exists; use re-issue certificate") and holds no secret.
func conflictText(he *hubid.Error) string {
	if he.Msg == "" {
		return "The Hub identity is in a state that does not allow this operation."
	}
	return safeText(he.Msg)
}

func safeText(s string) string {
	if s == "" || len(s) > 200 {
		return "The request was refused."
	}
	return s
}

// IdentityGate holds the transport facts the identity operations depend on.
type IdentityGate struct {
	// EffectiveHTTPS reports whether the browser's request arrived over HTTPS
	// by the Hub's trusted-proxy rules (or on the management listener). It is
	// required: without it every operation is refused with tls_required.
	EffectiveHTTPS func(*http.Request) bool
	// Scheme returns the scheme the browser used for the request ("http" on
	// the cleartext management listener, which EffectiveHTTPS still accepts);
	// the Origin header must carry it. When nil it follows EffectiveHTTPS.
	Scheme func(*http.Request) string
	// TrustedOrigins are extra origins ("https://hub.example.com") that count
	// as the Hub's own, for a Hub behind a proxy that rewrites the Host header.
	TrustedOrigins []string
	// Now is the clock; nil means time.Now.
	Now func() time.Time
}

func (g IdentityGate) now() time.Time {
	if g.Now != nil {
		return g.Now()
	}
	return time.Now()
}

// Check applies, in this order, the browser-side rules of contract section 8.1
// that follow the role check: fresh re-authentication (item 2), the JSON body
// and the same-origin headers (item 4; the synchronizer token was checked
// when the session was authenticated), and effective HTTPS (item 5). It
// returns an API error, and reads no passphrase.
func (g IdentityGate) Check(c *Call) error {
	sess := c.Identity.Session
	if c.Identity.Via != ViaSession || sess == nil || sess.ActAs != nil {
		return NewError(proto.CodeForbidden, "this operation is available only to a signed-in browser session")
	}
	if !sess.Fresh(g.now()) {
		e := NewError(proto.CodeReauthRequired, "Sign in again to confirm it is you; the last check is more than five minutes old.")
		e.Details = map[string]any{"method": reauthMethodKind(sess.AuthMethod)}
		return e
	}
	r := c.Request
	if r == nil {
		return NewError(proto.CodeForbidden, "the request cannot be checked")
	}
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
		return NewError(proto.CodeForbidden, "send the request as application/json")
	}
	if !g.sameOrigin(r) {
		return NewError(proto.CodeForbidden, "the request does not come from this site")
	}
	if g.EffectiveHTTPS == nil || !g.EffectiveHTTPS(r) {
		return NewError(proto.CodeTLSRequired, "A passphrase is accepted only over HTTPS. Open the Hub through its HTTPS address.")
	}
	return nil
}

// reauthMethodKind tells the page which re-authentication to offer.
func reauthMethodKind(method string) string {
	switch {
	case strings.HasPrefix(method, session.AuthMethodOIDCPrefix):
		return "oidc"
	default:
		return "local"
	}
}

// sameOrigin requires an Origin header that names this Hub (the request Host
// or a trusted origin), and, when the browser sends Sec-Fetch-Site, the value
// same-origin. A request without an Origin header is not a browser form post
// of this site and is refused.
func (g IdentityGate) sameOrigin(r *http.Request) bool {
	if v := r.Header.Get("Sec-Fetch-Site"); v != "" && v != "same-origin" {
		return false
	}
	origin := r.Header.Get("Origin")
	if origin == "" || origin == "null" {
		return false
	}
	got, ok := canonicalOrigin(origin)
	if !ok {
		return false
	}
	// The Origin must equal, as a whole (scheme, host and port), the origin of
	// this request as the Hub sees it (effective scheme plus Host) or the
	// configured public URL.
	scheme := "http"
	switch {
	case g.Scheme != nil:
		scheme = g.Scheme(r)
	case g.EffectiveHTTPS != nil && g.EffectiveHTTPS(r):
		scheme = "https"
	}
	if own, ok := canonicalOrigin(scheme + "://" + r.Host); ok && own == got {
		return true
	}
	for _, t := range g.TrustedOrigins {
		if want, ok := canonicalOrigin(t); ok && want == got {
			return true
		}
	}
	return false
}

// canonicalOrigin returns "scheme://host[:port]" in lower case with the
// default port of the scheme removed. It accepts only http and https
// origins without user information, path, query or fragment (a trailing slash
// is tolerated).
func canonicalOrigin(raw string) (string, bool) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return "", false
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "https" && scheme != "http" {
		return "", false
	}
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return "", false
	}
	port := u.Port()
	if (scheme == "https" && port == "443") || (scheme == "http" && port == "80") {
		port = ""
	}
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if port != "" {
		host += ":" + port
	}
	return scheme + "://" + host, true
}

// RegisterHubIdentityHandlers registers the handlers of the five Hub identity
// operations on r. The dispatcher has already authenticated the caller (a
// browser session with the CSRF token, never an API key or act-as session) and
// required the manage action on the caller's own user resource, which only an
// administrator holds. The handlers add the gate (fresh sign-in, same origin,
// HTTPS) and then call the service, which owns the passphrase rules, the
// lockout and the allowance. hub_identity_status needs no gate: it reads the
// database and shows no secret.
func RegisterHubIdentityHandlers(r *Registry, svc IdentityService, gate IdentityGate, log interface{ Error(string, ...any) }) error {
	if svc == nil {
		return fmt.Errorf("api: RegisterHubIdentityHandlers needs an identity service")
	}
	io := IdentityOps{Svc: svc, Log: log}
	actor := func(c *Call) hubid.Actor { return hubid.Actor{ID: c.Identity.Actor, Channel: hubid.ChannelUI} }
	gated := func(c *Call) error { return gate.Check(c) }

	if err := HandleTyped(r, ops.OpHubIdentityStatus, func(ctx context.Context, c *Call, _ ops.HubIdentityStatusArgs) (ops.HubIdentityStatusResult, error) {
		if err := requireHubWide(c); err != nil {
			return ops.HubIdentityStatusResult{}, err
		}
		return io.Status(ctx)
	}); err != nil {
		return err
	}
	if err := HandleTyped(r, ops.OpHubIdentitySetPassphrase, func(ctx context.Context, c *Call, a ops.HubIdentitySetPassphraseArgs) (ops.HubIdentityResult, error) {
		if err := gated(c); err != nil {
			return ops.HubIdentityResult{}, err
		}
		return io.SetPassphrase(ctx, actor(c), a)
	}); err != nil {
		return err
	}
	if err := HandleTyped(r, ops.OpHubIdentityReissue, func(ctx context.Context, c *Call, a ops.HubIdentityReissueArgs) (ops.HubIdentityResult, error) {
		if err := gated(c); err != nil {
			return ops.HubIdentityResult{}, err
		}
		return io.Reissue(ctx, actor(c), a)
	}); err != nil {
		return err
	}
	if err := HandleTyped(r, ops.OpHubIdentityRotate, func(ctx context.Context, c *Call, a ops.HubIdentityRotateArgs) (ops.HubIdentityResult, error) {
		if err := gated(c); err != nil {
			return ops.HubIdentityResult{}, err
		}
		return io.RotateMaster(ctx, actor(c), a)
	}); err != nil {
		return err
	}
	return HandleTyped(r, ops.OpHubIdentityChangePassphrase, func(ctx context.Context, c *Call, a ops.HubIdentityChangePassphraseArgs) (ops.HubIdentityResult, error) {
		if err := gated(c); err != nil {
			return ops.HubIdentityResult{}, err
		}
		return io.ChangePassphrase(ctx, actor(c), a)
	})
}

// requireHubWide refuses an API key that is limited to some resources: the
// identity status is Hub-wide.
func requireHubWide(c *Call) error {
	p := c.Principal
	if p.Kind != authz.KindAPIKey {
		return nil
	}
	if p.Scope != nil {
		id := store.NewID() // a user no chosen-users scope can list
		probe := authz.ResolvedResource{Kind: authz.ResourceUser, UserID: id, OwnerID: id}
		if slices.Contains(p.Scope.Actions(probe), ops.ActionManage) {
			return nil
		}
	}
	return NewError(proto.CodeForbidden, "this API key is limited to some resources; the Hub identity takes an everything scope")
}
