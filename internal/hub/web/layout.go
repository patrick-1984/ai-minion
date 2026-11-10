package web

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/patrick-1984/ai-minion/internal/hub/auth/session"
	"github.com/patrick-1984/ai-minion/internal/hub/authz"
)

// View is the value every template sees. The layout uses everything except
// Data; the page's "content" block uses Data.
type View struct {
	Title string
	// Path is the request path, for the active navigation entry.
	Path     string
	SignedIn bool
	Username string
	Role     string
	// CSRF is the token that htmx requests send in X-Minion-CSRF; empty when
	// nobody is signed in.
	CSRF string
	// Nav is the flat list of entries the user may see; NavGroups is the same
	// list grouped for the left rail (workspace, access, administration).
	Nav       []NavLink
	NavGroups []NavGroup
	// Crumbs are the breadcrumb ancestors and the current page; the last one
	// has no Path.
	Crumbs []Crumb
	// RoleLabel is the role as people read it ("Administrator" or "User") and
	// Initials the avatar text of the signed-in user.
	RoleLabel string
	Initials  string
	Banners   []BannerView
	Flashes   []Flash
	// Status and Message describe an error page (404, 403, 500); both are
	// zero for a normal page.
	Status  int
	Message string
	// Embed is set for a page that is shown inside the inspector of another page
	// of this Hub (see [FrameMode]): the compact layout without rail, topbar and
	// banners. It is a layout choice, never a permission.
	Embed bool
	// Inspector is set for a page that hosts the inspector frame.
	Inspector bool
	// Data is the page's own value.
	Data any
}

// NavLink is one rendered navigation entry.
type NavLink struct {
	Label  string
	Path   string
	Icon   string
	Active bool
}

// NavGroup is a labelled block of navigation entries.
type NavGroup struct {
	ID    string
	Label string
	// Path is the page the label links to; empty when the label is plain text.
	Path  string
	Links []NavLink
}

// Crumb is one step of the breadcrumb. The current page has no Path.
type Crumb struct {
	Label string
	Path  string
}

// Crumbed is implemented by the Data of a page that knows its own ancestors,
// for example a session below the session list. Without it the breadcrumb is
// derived from the navigation.
type Crumbed interface {
	Crumbs() []Crumb
}

// Navigation groups of the left rail.
const (
	NavGroupWorkspace = "workspace"
	NavGroupAccess    = "access"
	NavGroupAdmin     = "admin"
)

var navGroupOrder = []string{NavGroupWorkspace, NavGroupAccess, NavGroupAdmin}

var navGroupLabels = map[string]string{
	NavGroupWorkspace: "Workspace",
	NavGroupAccess:    "Access",
	NavGroupAdmin:     "Administration",
}

// pathAdminOverview is the page behind the Administration heading.
const pathAdminOverview = "/admin"

// BannerView is a [Banner] after validation, ready to render.
type BannerView struct {
	Kind   string
	Text   string
	Action *BannerAction
}

// FlashLevel is the severity of a flash message.
type FlashLevel string

// Flash levels.
const (
	FlashInfo    FlashLevel = "info"
	FlashWarning FlashLevel = "warning"
	FlashDanger  FlashLevel = "danger"
)

// Flash is a one-time message shown in the flash area of the next page.
type Flash struct {
	Level FlashLevel `json:"l"`
	Text  string     `json:"t"`
}

// secureKey is the context key of the Secure decision of the request.
type secureKey struct{}

const (
	flashCookie   = "minion_flash"
	maxFlashes    = 4
	maxFlashRunes = 300
	maxFlashBytes = 2048
)

func validLevel(l FlashLevel) FlashLevel {
	switch l {
	case FlashWarning, FlashDanger:
		return l
	default:
		return FlashInfo
	}
}

// AddFlash queues a message for the next page the browser loads: it adds to
// the flash cookie of r and sets it on w, so call it before writing the
// response. The Secure attribute of the cookie follows the Server's Secure
// setting for this request, which [Server.ServeHTTP] puts into the request
// context; a request that did not pass through a Server gets Secure (the safe
// default). The cookie carries only text that html/template escapes on
// display, never a secret.
func AddFlash(w http.ResponseWriter, r *http.Request, level FlashLevel, text string) {
	secure := true
	if v, ok := r.Context().Value(secureKey{}).(bool); ok {
		secure = v
	}
	list := pendingFlashes(w, r)
	list = append(list, Flash{Level: validLevel(level), Text: truncateRunes(text, maxFlashRunes)})
	if len(list) > maxFlashes {
		list = list[len(list)-maxFlashes:]
	}
	raw, err := json.Marshal(list)
	if err != nil {
		return
	}
	enc := base64.RawURLEncoding.EncodeToString(raw)
	if len(enc) > maxFlashBytes {
		return
	}
	dropFlashCookies(w)
	http.SetCookie(w, &http.Cookie{Name: flashCookie, Value: enc, Path: "/", MaxAge: 60,
		HttpOnly: true, Secure: secure, SameSite: http.SameSiteLaxMode})
}

// pendingFlashes returns the flashes AddFlash has to extend: the ones already
// set on w by an earlier AddFlash of the same response, else the ones the
// browser sent. Without this a second call would start again from the request
// cookie and the browser would keep only the last message.
func pendingFlashes(w http.ResponseWriter, r *http.Request) []Flash {
	cookies := (&http.Response{Header: w.Header()}).Cookies()
	for i := len(cookies) - 1; i >= 0; i-- {
		if cookies[i].Name != flashCookie {
			continue
		}
		if cookies[i].MaxAge < 0 {
			return nil
		}
		return decodeFlashes(cookies[i].Value)
	}
	return readFlashes(r)
}

// dropFlashCookies removes the flash Set-Cookie lines already on w so the
// combined cookie replaces them instead of adding a duplicate.
func dropFlashCookies(w http.ResponseWriter) {
	h := w.Header()
	lines := h.Values("Set-Cookie")
	kept := make([]string, 0, len(lines))
	for _, l := range lines {
		if strings.HasPrefix(l, flashCookie+"=") {
			continue
		}
		kept = append(kept, l)
	}
	if len(kept) == len(lines) {
		return
	}
	h.Del("Set-Cookie")
	for _, l := range kept {
		h.Add("Set-Cookie", l)
	}
}

func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

// readFlashes decodes the flash cookie; a malformed cookie yields nothing.
func readFlashes(r *http.Request) []Flash {
	c, err := r.Cookie(flashCookie)
	if err != nil {
		return nil
	}
	return decodeFlashes(c.Value)
}

// decodeFlashes decodes the value of the flash cookie.
func decodeFlashes(value string) []Flash {
	if value == "" || len(value) > maxFlashBytes {
		return nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return nil
	}
	var list []Flash
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil
	}
	if len(list) > maxFlashes {
		list = list[:maxFlashes]
	}
	for i := range list {
		list[i].Level = validLevel(list[i].Level)
		list[i].Text = truncateRunes(list[i].Text, maxFlashRunes)
	}
	return list
}

// sameOriginPath reports whether p is a path on this Hub: it starts with a
// single "/" and has no backslash, scheme or control character. It is the
// check for every redirect target and link that comes from outside the layout.
func sameOriginPath(p string) bool {
	if !strings.HasPrefix(p, "/") || strings.HasPrefix(p, "//") || strings.ContainsAny(p, "\\") {
		return false
	}
	for _, r := range p {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	u, err := url.Parse(p)
	return err == nil && u.Scheme == "" && u.Host == ""
}

// safeNext returns next when it is a path on this Hub, else "/".
func safeNext(next string) string {
	if sameOriginPath(next) {
		return next
	}
	return "/"
}

func hasRole(roles []authz.Role, role authz.Role) bool {
	return len(roles) == 0 || slices.Contains(roles, role)
}

// navGroupOf returns the group of an entry: the one it names, else
// administration for an entry only admins see, else the workspace.
func navGroupOf(n NavItem) string {
	if _, ok := navGroupLabels[n.Group]; ok {
		return n.Group
	}
	if len(n.Roles) > 0 && !slices.Contains(n.Roles, authz.RoleUser) {
		return NavGroupAdmin
	}
	return NavGroupWorkspace
}

// navFor returns the navigation entries a user with role may see, with the
// entry of the current path marked active. Nobody sees the navigation without
// a session. Hiding an entry is a convenience: every page checks the role on
// its own (see gate).
func (s *Server) navFor(sess *session.Session, p string) []NavLink {
	if sess == nil {
		return nil
	}
	var out []NavLink
	for _, n := range s.snap.nav {
		if !hasRole(n.Roles, sess.Role) {
			continue
		}
		out = append(out, NavLink{Label: n.Label, Path: n.Path, Icon: n.Icon, Active: navActive(n.Path, p)})
	}
	markActive(out)
	return out
}

// markActive keeps only the most specific entry active: /admin/users/ab is
// the page of "Users", not also of an overview at /admin.
func markActive(links []NavLink) {
	best := -1
	for i, l := range links {
		if l.Active && (best < 0 || len(l.Path) > len(links[best].Path)) {
			best = i
		}
	}
	for i := range links {
		links[i].Active = i == best
	}
}

// navGroupsFor groups the entries a user may see for the left rail. A group
// without entries is left out, so an ordinary user never sees the heading of
// the administration area.
func (s *Server) navGroupsFor(sess *session.Session, p string) []NavGroup {
	if sess == nil {
		return nil
	}
	byGroup := map[string][]NavLink{}
	var all []NavLink
	var groupOf []string
	for _, n := range s.snap.nav {
		if !hasRole(n.Roles, sess.Role) {
			continue
		}
		all = append(all, NavLink{Label: n.Label, Path: n.Path, Icon: n.Icon, Active: navActive(n.Path, p)})
		groupOf = append(groupOf, navGroupOf(n))
	}
	markActive(all)
	for i, l := range all {
		byGroup[groupOf[i]] = append(byGroup[groupOf[i]], l)
	}
	var out []NavGroup
	for _, id := range navGroupOrder {
		if len(byGroup[id]) == 0 {
			continue
		}
		g := NavGroup{ID: id, Label: navGroupLabels[id], Links: byGroup[id]}
		if id == NavGroupAdmin && sess.Role == authz.RoleAdmin && s.hasPage(pathAdminOverview) {
			g.Path = pathAdminOverview
		}
		out = append(out, g)
	}
	return out
}

// hasPage reports whether a GET page is registered at path p.
func (s *Server) hasPage(p string) bool {
	for _, pg := range s.snap.pages {
		if pg.Pattern == "GET "+p {
			return true
		}
	}
	return false
}

// crumbsFor builds the breadcrumb of a page. Every ancestor is a real page: the
// control center, and the administration overview for an administration page.
func (s *Server) crumbsFor(sess *session.Session, p, title string, data any) []Crumb {
	if sess == nil {
		return nil
	}
	if c, ok := data.(Crumbed); ok {
		if list := c.Crumbs(); len(list) > 0 {
			return list
		}
	}
	if p == "/" {
		return []Crumb{{Label: "Control center"}}
	}
	out := []Crumb{{Label: "Control center", Path: "/"}}
	for _, n := range s.snap.nav {
		if navActive(n.Path, p) && navGroupOf(n) == NavGroupAdmin && s.hasPage(pathAdminOverview) && p != pathAdminOverview {
			out = append(out, Crumb{Label: "Administration", Path: pathAdminOverview})
			break
		}
	}
	if title == "" {
		title = "Page"
	}
	return append(out, Crumb{Label: title})
}

// roleLabel is the role as the interface says it.
func roleLabel(r authz.Role) string {
	if r == authz.RoleAdmin {
		return "Administrator"
	}
	return "User"
}

// initialsOf returns up to two upper-case letters for the avatar: the first
// letters of the first two words, else the first two letters.
func initialsOf(name string) string {
	words := strings.FieldsFunc(name, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
	var out []rune
	for _, w := range words {
		out = append(out, unicode.ToUpper([]rune(w)[0]))
		if len(out) == 2 {
			return string(out)
		}
	}
	if len(words) == 1 {
		if rs := []rune(words[0]); len(rs) > 1 {
			out = append(out, unicode.ToUpper(rs[1]))
		}
	}
	return string(out)
}

// navActive reports whether the request path p belongs to the entry at np:
// the entry's own path, or anything below it (the root entry matches only "/").
func navActive(np, p string) bool {
	if np == "/" {
		return p == "/"
	}
	return p == np || strings.HasPrefix(p, strings.TrimSuffix(np, "/")+"/")
}

func (s *Server) bannersFor(r *http.Request, sess *session.Session) []BannerView {
	if sess == nil {
		return nil
	}
	var out []BannerView
	for _, f := range s.snap.banners {
		for _, b := range f(r) {
			bv := BannerView{Kind: "info", Text: b.Text}
			if b.Kind == "warning" || b.Kind == "danger" {
				bv.Kind = b.Kind
			}
			if b.Action != nil && sameOriginPath(b.Action.URL) && strings.TrimSpace(b.Action.Label) != "" {
				a := *b.Action
				bv.Action = &a
			}
			out = append(out, bv)
		}
	}
	return out
}

// FrameMode says how a response may be framed (the session inspector, see
// docs/design/workbench/IMPLEMENTATION.md).
type FrameMode int

const (
	// FrameNone is every page that takes no part in the inspector: it may not be
	// framed by anyone and frames nothing (CSP frame-ancestors 'none' and
	// frame-src 'none', X-Frame-Options DENY).
	FrameNone FrameMode = iota
	// FrameHost is a list page that hosts the inspector: it may frame pages of
	// this origin (frame-src 'self') and is still never framed itself.
	FrameHost
	// FrameEmbed is the compact session view inside the inspector: only a page of
	// this origin may frame it (frame-ancestors 'self', X-Frame-Options
	// SAMEORIGIN); it frames nothing. The "embed" query flag only chooses this
	// response; every route still authenticates and authorises as for the full page.
	FrameEmbed
)

// embedFlag is the query parameter that asks an embeddable page for its compact view.
const embedFlag = "embed"

// frameModeOf is the framing of a page for one request: an embeddable page is
// embedded when the request asks (embed=1 exactly), a page that hosts the
// inspector is a host, every other page is neither.
func frameModeOf(p Page, r *http.Request) FrameMode {
	switch {
	case p.Embeddable && r.Method == http.MethodGet && r.URL.Query().Get(embedFlag) == "1":
		return FrameEmbed
	case p.Inspector:
		return FrameHost
	}
	return FrameNone
}

// newView builds the view of one page render. It consumes the flash cookie:
// the messages are shown once. An embedded view leaves the cookie alone: its
// messages belong to the page the person sees, not to a frame inside it.
func (s *Server) newView(w http.ResponseWriter, r *http.Request, title string, data any, mode FrameMode) View {
	v := View{Title: title, Path: r.URL.Path, Data: data, Embed: mode == FrameEmbed, Inspector: mode == FrameHost}
	sess, _ := session.FromContext(r.Context())
	if sess != nil {
		v.SignedIn, v.Username, v.Role, v.CSRF = true, sess.Username, string(sess.Role), sess.CSRFToken
		v.RoleLabel, v.Initials = roleLabel(sess.Role), initialsOf(sess.Username)
	}
	v.Nav = s.navFor(sess, r.URL.Path)
	v.NavGroups = s.navGroupsFor(sess, r.URL.Path)
	v.Crumbs = s.crumbsFor(sess, r.URL.Path, title, data)
	if mode == FrameEmbed {
		return v
	}
	v.Banners = s.bannersFor(r, sess)
	if fl := readFlashes(r); len(fl) > 0 {
		v.Flashes = fl
	}
	if _, err := r.Cookie(flashCookie); err == nil {
		http.SetCookie(w, &http.Cookie{Name: flashCookie, Value: "", Path: "/", MaxAge: -1,
			HttpOnly: true, Secure: s.secure(r), SameSite: http.SameSiteLaxMode})
	}
	return v
}

// setPageHeaders sets the headers of every HTML response: no caching (the page
// shows account data), a content security policy that allows only this
// origin's scripts and styles, and the usual framing and sniffing protections.
// Framing is closed for every page except the two of the session inspector
// ([FrameMode]): the list pages may frame this origin, and the compact session
// view may be framed by this origin; nothing is ever open to another origin.
func setPageHeaders(w http.ResponseWriter, mode FrameMode) {
	ancestors, frameSrc, xfo := "'none'", "'none'", "DENY"
	switch mode {
	case FrameHost:
		frameSrc = "'self'"
	case FrameEmbed:
		ancestors, xfo = "'self'", "SAMEORIGIN"
	}
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; "+
		"frame-src "+frameSrc+"; frame-ancestors "+ancestors+"; base-uri 'none'; form-action 'self'")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", xfo)
	h.Set("Referrer-Policy", "same-origin")
}

// render writes a full page: the page's template inside the layout. The page
// is rendered into a buffer first, so a template failure becomes a clean 500
// instead of a half page.
func (s *Server) render(w http.ResponseWriter, r *http.Request, status int, file, title string, data any) {
	s.renderFrame(w, r, status, file, title, data, FrameNone)
}

func (s *Server) renderFrame(w http.ResponseWriter, r *http.Request, status int, file, title string, data any, mode FrameMode) {
	v := s.newView(w, r, title, data, mode)
	s.renderView(w, status, file, v, mode)
}

func (s *Server) renderView(w http.ResponseWriter, status int, file string, v View, mode FrameMode) {
	tpl := s.templates[file]
	if tpl == nil {
		tpl = s.templates[""]
	}
	var buf bytes.Buffer
	if err := tpl.ExecuteTemplate(&buf, layoutFile, v); err != nil {
		s.log.Error("render failed", "template", file, "error", err)
		setPageHeaders(w, FrameNone)
		http.Error(w, "the page could not be rendered", http.StatusInternalServerError)
		return
	}
	setPageHeaders(w, mode)
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}

// renderStatus writes an error page with the layout and a message.
func (s *Server) renderStatus(w http.ResponseWriter, r *http.Request, status int, message string) {
	s.renderStatusFrame(w, r, status, message, FrameNone)
}

// renderStatusFrame is renderStatus for a page that may be embedded: the error
// of an embedded view is a plain message in the same compact layout, with no
// link that would move the frame to another page.
func (s *Server) renderStatusFrame(w http.ResponseWriter, r *http.Request, status int, message string, mode FrameMode) {
	if mode == FrameHost {
		mode = FrameNone // an error page hosts nothing
	}
	v := s.newView(w, r, http.StatusText(status), nil, mode)
	v.Status, v.Message = status, message
	s.renderView(w, status, "", v, mode)
}

// pageHandler turns a [Page] into a handler.
func (s *Server) pageHandler(p Page) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mode := frameModeOf(p, r)
		var data any
		if p.Data != nil {
			var err error
			if data, err = p.Data(r); err != nil {
				var se *StatusError
				if errors.As(err, &se) {
					s.renderStatusFrame(w, r, se.Code, se.Message, mode)
					return
				}
				s.log.Error("page data failed", "pattern", p.Pattern, "error", err)
				s.renderStatusFrame(w, r, http.StatusInternalServerError, "The page could not be built. Try again.", mode)
				return
			}
		}
		s.renderFrame(w, r, http.StatusOK, p.Template, p.Title, data, mode)
	})
}
