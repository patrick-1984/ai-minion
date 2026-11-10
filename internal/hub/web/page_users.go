package web

// Administration pages, part 1: the plumbing the four admin pages share
// (Story 6.8) and the users page. The pages never touch a store. Every read and
// every change goes through the operation catalog over the REST pipeline
// (AD-18) as the signed-in admin, so authorisation, act-as rules, guards, the
// audit trail (AD-20) and request ids are exactly those of the API and the CLI.
// The htmx forms of these pages post to routes in this package; each route
// turns the form into one operation call.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/patrick-1984/ai-minion/internal/hub/auth/session"
	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/hub/httpbody"
	"github.com/patrick-1984/ai-minion/internal/ops"
)

// Paths of the administration pages.
const (
	pathAdminUsers    = "/admin/users"
	pathAdminSettings = "/admin/settings"
	pathAdminOIDC     = "/admin/oidc"
	pathAdminAudit    = "/admin/audit"
)

// Navigation order of the admin entries: after the entries of the user pages.
const adminNavOrder = 800

// maxAdminFormBytes bounds the body of an admin form post.
const maxAdminFormBytes = 64 << 10

// OpCaller is the port of the admin pages to the operation catalog. It runs one
// operation as the user of request r (the signed-in admin) and decodes the
// result into result, which may be nil. A failure is an [*OpError].
type OpCaller interface {
	CallOp(r *http.Request, op string, args, result any) error
}

// OpError is the failure of an operation: the HTTP status and the AD-18
// envelope of the answer. Message is safe to show; the API never puts secrets
// or causes into it.
type OpError struct {
	Status  int
	Code    string
	Message string
	// Field is the argument that was refused, when the API named one.
	Field string
}

// Error implements error.
func (e *OpError) Error() string {
	return fmt.Sprintf("web: operation failed (%d %s): %s", e.Status, e.Code, e.Message)
}

// adminOps holds the OpCaller of the admin pages. It is set once at startup by
// the web module ([SetAdminOps]); the pages are registered earlier, from init.
var adminOps atomic.Pointer[opCallerBox]

type opCallerBox struct{ c OpCaller }

// SetAdminOps connects the admin pages to the operation catalog. The Hub's web
// module calls it with [NewHandlerCaller] on the root mux. Until then every
// admin page answers 503. A nil caller disconnects the pages.
func SetAdminOps(c OpCaller) {
	if c == nil {
		adminOps.Store(nil)
		return
	}
	adminOps.Store(&opCallerBox{c: c})
}

// DisconnectAdminOps disconnects the admin pages when c is still the connected
// caller. The Hub's web module calls it when it stops, so a later Hub in the
// same process is never disconnected by an earlier one.
func DisconnectAdminOps(c OpCaller) {
	if b := adminOps.Load(); b != nil && b.c == c {
		adminOps.CompareAndSwap(b, nil)
	}
}

func adminCaller() (OpCaller, error) {
	b := adminOps.Load()
	if b == nil {
		return nil, &StatusError{Code: http.StatusServiceUnavailable, Message: "The administration pages are not connected to the Hub yet."}
	}
	return b.c, nil
}

// HandlerCaller is an [OpCaller] that runs operations through an http.Handler
// that serves the REST API, in process: no socket, no second login. It
// forwards the session cookie, the CSRF token of the session and the client
// address headers of the incoming request, so the API authenticates and
// authorises the call exactly as it would the browser's own request.
type HandlerCaller struct{ h http.Handler }

// NewHandlerCaller returns a caller on h, which must serve ops.RESTPrefix (the
// Hub's root mux does).
func NewHandlerCaller(h http.Handler) *HandlerCaller { return &HandlerCaller{h: h} }

// forwardedHeaders are the request headers that decide the client address and
// the scheme in the Hub's trusted-proxy rules.
var forwardedHeaders = []string{"X-Forwarded-For", "X-Forwarded-Proto", "X-Forwarded-Host", "X-Real-Ip", "Forwarded"}

// maxOpResponseBytes bounds the answer of one operation.
const maxOpResponseBytes = 8 << 20

// CallOp implements [OpCaller].
func (c *HandlerCaller) CallOp(r *http.Request, op string, args, result any) error {
	sess, ok := session.FromContext(r.Context())
	if !ok {
		return &OpError{Status: http.StatusUnauthorized, Code: "unauthenticated", Message: "sign in first"}
	}
	if args == nil {
		args = struct{}{}
	}
	body, err := json.Marshal(args)
	if err != nil {
		return fmt.Errorf("web: encode the arguments of %s: %w", op, err)
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, ops.RESTPrefix+op, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("web: build the request of %s: %w", op, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set(session.CSRFHeader, sess.CSRFToken)
	if v := r.Header.Get("Cookie"); v != "" {
		req.Header.Set("Cookie", v)
	}
	for _, h := range forwardedHeaders {
		for _, v := range r.Header.Values(h) {
			req.Header.Add(h, v)
		}
	}
	req.Host, req.RemoteAddr, req.TLS = r.Host, r.RemoteAddr, r.TLS

	rec := &capture{header: http.Header{}, limit: maxOpResponseBytes}
	c.h.ServeHTTP(rec, req)
	if rec.status == 0 {
		rec.status = http.StatusOK
	}
	if rec.over {
		return fmt.Errorf("web: the answer of %s is larger than %d bytes", op, maxOpResponseBytes)
	}
	if rec.status/100 != 2 {
		return decodeOpError(rec.status, rec.body.Bytes())
	}
	if result == nil {
		return nil
	}
	if err := json.Unmarshal(rec.body.Bytes(), result); err != nil {
		return fmt.Errorf("web: decode the result of %s: %w", op, err)
	}
	return nil
}

// decodeOpError reads the AD-18 envelope; an answer that is none becomes a
// generic failure that carries only the status.
func decodeOpError(status int, body []byte) *OpError {
	var env struct {
		Error struct {
			Code    string         `json:"code"`
			Message string         `json:"message"`
			Details map[string]any `json:"details"`
		} `json:"error"`
	}
	e := &OpError{Status: status, Code: "internal", Message: "The Hub could not complete the request."}
	if json.Unmarshal(body, &env) != nil || env.Error.Code == "" {
		return e
	}
	e.Code, e.Message = env.Error.Code, env.Error.Message
	if f, ok := env.Error.Details["field"].(string); ok {
		e.Field = f
	}
	return e
}

// capture is a minimal in-memory http.ResponseWriter.
type capture struct {
	header http.Header
	status int
	body   bytes.Buffer
	limit  int
	over   bool
}

func (c *capture) Header() http.Header { return c.header }

func (c *capture) WriteHeader(code int) {
	if c.status == 0 {
		c.status = code
	}
}

func (c *capture) Write(p []byte) (int, error) {
	if c.status == 0 {
		c.status = http.StatusOK
	}
	if c.body.Len()+len(p) > c.limit {
		c.over = true
		return len(p), nil
	}
	return c.body.Write(p)
}

// requireAdmin answers 404 (not 403) for everybody who is not an admin acting
// as themselves, so a user learns nothing about the admin pages. While an
// admin acts as another user, the API treats the session as that user, so the
// pages are closed too.
func requireAdmin(r *http.Request) error {
	sess, ok := session.FromContext(r.Context())
	if !ok || sess.Role != authz.RoleAdmin || sess.ActAs != nil {
		return ErrNotFound()
	}
	return nil
}

// adminPageData runs fn as the data function of an admin page: the admin
// check, the connected caller, and the mapping of a failed call to a page
// status.
func adminPageData(fn func(r *http.Request, c OpCaller) (any, error)) func(r *http.Request) (any, error) {
	return func(r *http.Request) (any, error) {
		if err := requireAdmin(r); err != nil {
			return nil, err
		}
		c, err := adminCaller()
		if err != nil {
			return nil, err
		}
		data, err := fn(r, c)
		if err != nil {
			return nil, pageError(err)
		}
		return data, nil
	}
}

// pageError maps a failed operation to the status of the page: a refusal looks
// like a missing page, anything else says that the Hub failed.
func pageError(err error) error {
	var se *StatusError
	if errors.As(err, &se) {
		return se
	}
	var oe *OpError
	if errors.As(err, &oe) {
		switch oe.Status {
		case http.StatusForbidden, http.StatusUnauthorized, http.StatusNotFound:
			return ErrNotFound()
		}
		return &StatusError{Code: http.StatusBadGateway, Message: "The Hub could not load this page: " + opMessage(oe)}
	}
	return err
}

// opMessage is the sentence shown for a failed operation: the API message,
// with the refused field in front when there is one ("username must ...").
func opMessage(e *OpError) string {
	if e.Field != "" {
		return strings.ReplaceAll(e.Field, "_", " ") + " " + e.Message
	}
	return e.Message
}

// failureText is the sentence shown for any error of a form post.
func failureText(err error) string {
	var oe *OpError
	if errors.As(err, &oe) {
		return opMessage(oe)
	}
	return "The Hub could not complete the request."
}

// adminFormBodyTimeout is how long an admin form may take to arrive; a test
// shortens it.
var adminFormBodyTimeout = httpbody.DefaultTimeout

// adminRoute wraps the handler of an admin form post: admin only (404 for
// anyone else), a connected caller, a bounded form body.
func adminRoute(fn func(w http.ResponseWriter, r *http.Request, c OpCaller)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requireAdmin(r) != nil {
			http.NotFound(w, r)
			return
		}
		c, err := adminCaller()
		if err != nil {
			writeResultError(w, "The administration pages are not connected to the Hub yet.")
			return
		}
		// The form is read under a body deadline of its own (the Hub guards every
		// request with a body before routing, see internal/hub/httpbody; this
		// keeps the route safe behind any server) and under a size limit.
		httpbody.Arm(w, r, adminFormBodyTimeout)
		r.Body = http.MaxBytesReader(w, r.Body, maxAdminFormBytes)
		if err := r.ParseForm(); err != nil {
			writeResultError(w, "The form could not be read.")
			return
		}
		// ParseForm reads nothing of a content type it does not know, and a
		// declared body may be longer than the form: read to the end under the
		// deadline, which only the end of the body lifts.
		if err := httpbody.Drain(r); err != nil {
			writeResultError(w, "The form could not be read.")
			return
		}
		fn(w, r, c)
	})
}

// resultTemplate renders the error line that htmx swaps into the result area
// of a form. htmx does not swap 4xx answers, so the line comes with status 200.
var resultTemplate = template.Must(template.New("result").Parse(`<p class="form-error" role="alert">{{.}}</p>`))

func writeResultError(w http.ResponseWriter, text string) {
	var buf bytes.Buffer
	if err := resultTemplate.Execute(&buf, text); err != nil {
		http.Error(w, "the answer could not be rendered", http.StatusInternalServerError)
		return
	}
	writeFragment(w, buf.Bytes())
}

func writeFragment(w http.ResponseWriter, b []byte) {
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write(b)
}

// writeFragmentTemplate renders the named fragment of a page template file.
func writeFragmentTemplate(w http.ResponseWriter, t *template.Template, name string, data any) {
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, name, data); err != nil {
		http.Error(w, "the answer could not be rendered", http.StatusInternalServerError)
		return
	}
	writeFragment(w, buf.Bytes())
}

// fragments parses one embedded page template file on its own, so that a form
// route can render a fragment the file defines next to its "content" block.
func fragments(file string) func() *template.Template {
	return sync.OnceValue(func() *template.Template {
		return template.Must(template.ParseFS(templateFiles, "templates/"+file))
	})
}

// done ends a successful form post: the flash message goes to the page that
// the browser reloads (HX-Refresh), with one more line per warning.
func done(w http.ResponseWriter, r *http.Request, text string, res ops.WriteResult) {
	AddFlash(w, r, FlashInfo, text)
	for _, wn := range res.Warnings {
		AddFlash(w, r, FlashWarning, wn.Message)
	}
	w.Header().Set("HX-Refresh", "true")
	w.WriteHeader(http.StatusOK)
}

// writeOp runs a write operation and ends the request: the flash message and a
// reload on success, the error line in the result area otherwise. A result
// whose outcome is not "delivered" is a failure too.
func writeOp(w http.ResponseWriter, r *http.Request, c OpCaller, op string, args any, success string) {
	var res ops.WriteResult
	if err := c.CallOp(r, op, args, &res); err != nil {
		writeResultError(w, failureText(err))
		return
	}
	if res.Outcome != "" && res.Outcome != ops.OutcomeDelivered {
		writeResultError(w, "The change was not applied"+reasonSuffix(res.Reason)+".")
		return
	}
	done(w, r, success, res)
}

func reasonSuffix(reason string) string {
	if reason == "" {
		return ""
	}
	return " (" + strings.ReplaceAll(reason, "_", " ") + ")"
}

// newWriteArgs returns the request id every write operation needs.
func newWriteArgs() ops.WriteArgs { return ops.WriteArgs{RequestID: ops.NewUUIDv7()} }

// -- users page --------------------------------------------------------------

// Operation names of the linked identities (Story 6.4). They are not part of
// the catalog of this build; the page shows a note while the Hub answers
// unsupported_op.
const (
	opIdentityList   = "identity_list"
	opIdentityDetach = "identity_detach"
)

// Act-as durations offered on the user page, in minutes.
var actAsChoices = []int{15, 30, 60, 120, 240, 480}

// UsersPage is what users.html sees as .Data: the list of accounts, or, when
// Detail is set, the page of one account.
type UsersPage struct {
	Users  []ops.UserInfo
	Detail *UserPage
}

// UserPage is the page of one account.
type UserPage struct {
	User ops.UserInfo
	// LocalAdmin and IdPAdmin say where the admin role comes from. Only the
	// local source can be changed here.
	LocalAdmin bool
	IdPAdmin   bool
	// CanReset is true for an active account that has a local password.
	CanReset bool
	// Identities are the linked sign-in identities; IdentitiesNote explains
	// why the list is empty or missing.
	Identities     []IdentityInfo
	IdentitiesNote string
	ActAsChoices   []int
}

// IdentityInfo is one linked external identity (issuer and subject).
type IdentityInfo struct {
	ID       string `json:"id"`
	Provider string `json:"provider_id"`
	Name     string `json:"provider_name"`
	Issuer   string `json:"issuer"`
	Subject  string `json:"subject"`
	Email    string `json:"email"`
	LinkedAt string `json:"created_at"`
}

// Label is the provider as shown: its name, else its id.
func (i IdentityInfo) Label() string {
	if i.Name != "" {
		return i.Name
	}
	return i.Provider
}

// userPageOf builds the page value of one account.
func userPageOf(u ops.UserInfo) UserPage {
	p := UserPage{User: u, ActAsChoices: actAsChoices, CanReset: u.Active && u.HasPassword}
	for _, s := range u.AdminSources {
		switch s {
		case ops.RoleSourceLocal:
			p.LocalAdmin = true
		case ops.RoleSourceIdP:
			p.IdPAdmin = true
		}
	}
	return p
}

func listUsers(r *http.Request, c OpCaller) ([]ops.UserInfo, error) {
	var res ops.UsersListResult
	if err := c.CallOp(r, ops.OpUsersList, ops.UsersListArgs{}, &res); err != nil {
		return nil, err
	}
	return res.Users, nil
}

func usersListData(r *http.Request, c OpCaller) (any, error) {
	users, err := listUsers(r, c)
	if err != nil {
		return nil, err
	}
	return UsersPage{Users: users}, nil
}

func userDetailData(r *http.Request, c OpCaller) (any, error) {
	id := r.PathValue("id")
	users, err := listUsers(r, c)
	if err != nil {
		return nil, err
	}
	for _, u := range users {
		if u.ID != id {
			continue
		}
		p := userPageOf(u)
		p.Identities, p.IdentitiesNote = loadIdentities(r, c, id)
		return UsersPage{Detail: &p}, nil
	}
	return nil, ErrNotFound()
}

// loadIdentities lists the linked identities of an account. A Hub without the
// operation, or a failing call, leaves the page working with a note.
func loadIdentities(r *http.Request, c OpCaller, userID string) ([]IdentityInfo, string) {
	var res struct {
		Identities []IdentityInfo `json:"identities"`
	}
	err := c.CallOp(r, opIdentityList, map[string]string{"user_id": userID}, &res)
	var oe *OpError
	switch {
	case err == nil:
		if len(res.Identities) == 0 {
			return nil, "No external identity is linked to this account."
		}
		return res.Identities, ""
	case errors.As(err, &oe) && oe.Code == "unsupported_op":
		return nil, "Linked identities are not available on this Hub."
	default:
		return nil, "The linked identities could not be loaded."
	}
}

var userFragments = fragments("users.html")

// secretResult is what the user_secret fragment of users.html renders: a
// temporary password, once. The answer is not cached (no-store) and the
// password is not kept anywhere after it.
type secretResult struct {
	Username string
	Password string
	// Created is true after create, false after a reset.
	Created bool
}

func handleUserCreate(w http.ResponseWriter, r *http.Request, c OpCaller) {
	args := ops.UserCreateArgs{
		WriteArgs: newWriteArgs(),
		Username:  strings.TrimSpace(r.PostFormValue("username")),
		Email:     strings.TrimSpace(r.PostFormValue("email")),
		Role:      r.PostFormValue("role"),
	}
	var res struct {
		User              ops.UserInfo `json:"user"`
		TemporaryPassword string       `json:"temporary_password"`
		Outcome           ops.Outcome  `json:"outcome"`
	}
	if err := c.CallOp(r, ops.OpUserCreate, args, &res); err != nil {
		writeResultError(w, failureText(err))
		return
	}
	writeFragmentTemplate(w, userFragments(), "user_secret", secretResult{
		Username: res.User.Username, Password: res.TemporaryPassword, Created: true,
	})
}

func handleUserReset(w http.ResponseWriter, r *http.Request, c OpCaller) {
	args := ops.UserResetPasswordArgs{WriteArgs: newWriteArgs(), UserID: r.PathValue("id")}
	var res struct {
		TemporaryPassword string `json:"temporary_password"`
	}
	if err := c.CallOp(r, ops.OpUserResetPassword, args, &res); err != nil {
		writeResultError(w, failureText(err))
		return
	}
	name := r.PostFormValue("username")
	writeFragmentTemplate(w, userFragments(), "user_secret", secretResult{Username: name, Password: res.TemporaryPassword})
}

func handleUserRole(w http.ResponseWriter, r *http.Request, c OpCaller) {
	role := r.PostFormValue("role")
	args := ops.UserSetRoleArgs{WriteArgs: newWriteArgs(), UserID: r.PathValue("id"), Role: role}
	text := "The local admin role was removed."
	if role == ops.RoleAdmin {
		text = "The account is now a local admin."
	}
	writeOp(w, r, c, ops.OpUserSetRole, args, text)
}

func handleUserDeactivate(w http.ResponseWriter, r *http.Request, c OpCaller) {
	args := ops.UserDeactivateArgs{WriteArgs: newWriteArgs(), UserID: r.PathValue("id")}
	writeOp(w, r, c, ops.OpUserDeactivate, args, "The account is deactivated.")
}

func handleActAsStart(w http.ResponseWriter, r *http.Request, c OpCaller) {
	minutes := 0
	if v := strings.TrimSpace(r.PostFormValue("minutes")); v != "" {
		if _, err := fmt.Sscanf(v, "%d", &minutes); err != nil {
			writeResultError(w, "The duration must be a number of minutes.")
			return
		}
	}
	args := ops.ActAsStartArgs{WriteArgs: newWriteArgs(), UserID: r.PathValue("id"), DurationMinutes: minutes}
	var res ops.WriteResult
	if err := c.CallOp(r, ops.OpActAsStart, args, &res); err != nil {
		writeResultError(w, failureText(err))
		return
	}
	// The banner of the act-as session is shown on the next page.
	redirect(w, r, "/")
}

func handleIdentityDetach(w http.ResponseWriter, r *http.Request, c OpCaller) {
	args := struct {
		ops.WriteArgs
		UserID     string `json:"user_id"`
		IdentityID string `json:"identity_id"`
	}{newWriteArgs(), r.PathValue("id"), r.PathValue("identity")}
	writeOp(w, r, c, opIdentityDetach, args, "The identity is detached.")
}

func init() {
	RegisterPage(Page{Pattern: pathAdminUsers, Title: "Users", Template: "users.html", Data: adminPageData(usersListData)})
	RegisterPage(Page{Pattern: pathAdminUsers + "/{id}", Title: "User", Template: "users.html", Data: adminPageData(userDetailData)})
	RegisterNav(NavItem{Label: "Users", Path: pathAdminUsers, Order: adminNavOrder, Roles: []authz.Role{authz.RoleAdmin}, Group: NavGroupAdmin, Icon: "i-users"})
	for _, rt := range []struct {
		pattern string
		fn      func(http.ResponseWriter, *http.Request, OpCaller)
	}{
		{"POST " + pathAdminUsers, handleUserCreate},
		{"POST " + pathAdminUsers + "/{id}/reset-password", handleUserReset},
		{"POST " + pathAdminUsers + "/{id}/role", handleUserRole},
		{"POST " + pathAdminUsers + "/{id}/deactivate", handleUserDeactivate},
		{"POST " + pathAdminUsers + "/{id}/act-as", handleActAsStart},
		{"POST " + pathAdminUsers + "/{id}/identities/{identity}/detach", handleIdentityDetach},
	} {
		RegisterRoute(Route{Pattern: rt.pattern, Handler: adminRoute(rt.fn)})
	}
}
