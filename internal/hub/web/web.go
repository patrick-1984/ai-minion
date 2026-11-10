package web

import (
	"bytes"
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"path"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/auth/session"
	"github.com/patrick-1984/ai-minion/internal/hub/authz"
)

//go:embed templates/*.html
var templateFiles embed.FS

//go:embed static/*.js static/*.css static/*.svg
var staticFiles embed.FS

// EmbeddedTemplates returns the embedded templates, rooted at the templates
// directory. Tests use it to build a template set with extra files.
func EmbeddedTemplates() fs.FS {
	sub, err := fs.Sub(templateFiles, "templates")
	if err != nil {
		panic(err) // the directory is embedded at build time
	}
	return sub
}

// EmbeddedStatic returns the embedded static assets, rooted at the static
// directory.
func EmbeddedStatic() fs.FS {
	sub, err := fs.Sub(staticFiles, "static")
	if err != nil {
		panic(err)
	}
	return sub
}

// Paths the shell owns. Other stories cannot register them.
const (
	pathLogin    = "/login"
	pathPassword = "/account/password"
	pathStatic   = "/static/"
)

// reservedPrefixes are path prefixes that belong to the REST framework, the
// auth endpoints and the static assets; a page or route may not claim them.
var reservedPrefixes = []string{"/static/", "/api/", "/auth/", "/login", "/account/password", "/healthz", "/readyz"}

var templateName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]*\.html$`)

var providerID = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

// Page is a server-rendered page inside the layout.
type Page struct {
	// Pattern is a net/http route pattern. It must be a GET pattern, with or
	// without the "GET " prefix, for example "/sessions" or "GET /workers/{id}".
	Pattern string
	// Title is the page title shown in the browser tab and available to the
	// template as .Title.
	Title string
	// Template names a file under templates/ that defines the "content"
	// block, for example "sessions.html". Empty renders the layout's default
	// content. Files that start with "_" are partials available to every page.
	Template string
	// Roles limits the page to these roles. Empty means every signed-in user.
	Roles []authz.Role
	// Data builds the value the template sees as .Data. It may be nil. It may
	// return a [*StatusError] to answer with another status.
	Data func(r *http.Request) (any, error)
	// Embeddable lets the page be shown in the session inspector of another page
	// of this Hub: a GET with embed=1 renders the compact layout and is answered
	// with frame-ancestors 'self' / X-Frame-Options SAMEORIGIN. Nothing else
	// changes: the page still authenticates and authorises every request.
	Embeddable bool
	// Inspector marks a list page that hosts the inspector frame: its CSP allows
	// frames of this origin (frame-src 'self'). Every other page forbids frames.
	Inspector bool
}

// Route is a handler that is not a full page: an htmx fragment, a form post, an
// SSE stream. It is served behind the same session, must-change and role
// checks as a page; the response is the handler's own.
type Route struct {
	// Pattern is a net/http route pattern, with a method for anything but GET.
	Pattern string
	// Roles limits the route to these roles. Empty means every signed-in user.
	Roles   []authz.Role
	Handler http.Handler
}

// NavItem is one entry of the navigation bar.
type NavItem struct {
	Label string
	// Path is an absolute path on this Hub, for example "/sessions".
	Path string
	// Order sorts the entries, ascending; ties are broken by label.
	Order int
	// Roles limits the entry to these roles. Empty means every signed-in user.
	Roles []authz.Role
	// Group is the block of the left rail the entry belongs to: [NavGroupWorkspace],
	// [NavGroupAccess] or [NavGroupAdmin]. Empty derives it from Roles (an entry
	// only admins see belongs to the administration, any other to the workspace).
	Group string
	// Icon is the id of an icon of the shell's sprite (for example "i-home");
	// empty shows none.
	Icon string
}

// LoginProvider is an external sign-in method (OIDC, Story 6.2). The login
// page shows one button per provider that links to Path.
type LoginProvider struct {
	// ID is a short stable identifier: lower-case letters, digits, "-", "_".
	ID string
	// Label is the text after "Sign in with".
	Label string
	// Path is the absolute path on this Hub that starts the sign-in.
	Path  string
	Order int
}

// Banner is a persistent message above the page content, for example the
// act-as banner of Story 6.5.
type Banner struct {
	// Kind is "info", "warning" or "danger"; anything else shows as "info".
	Kind string
	Text string
	// Action is an optional button that posts to a path on this Hub with the
	// CSRF header and reloads the page.
	Action *BannerAction
}

// BannerAction is the button of a [Banner].
type BannerAction struct {
	Label string
	// URL is an absolute path on this Hub; the button POSTs JSON "{}" to it.
	URL string
}

// BannerFunc returns the banners for one request. It runs for every page
// render of a signed-in user, so it must be cheap.
type BannerFunc func(r *http.Request) []Banner

// StatusError makes a page's Data function answer with an HTTP status other
// than 200. Message is shown to the user.
type StatusError struct {
	Code    int
	Message string
}

// Error implements error.
func (e *StatusError) Error() string { return fmt.Sprintf("web: %d %s", e.Code, e.Message) }

// ErrNotFound is a ready-made [*StatusError] for "this page has no such item".
func ErrNotFound() *StatusError {
	return &StatusError{Code: http.StatusNotFound, Message: "That page does not exist."}
}

// Registry collects what the other stories add to the web UI. A [Server]
// reads it once, in [New]; entries added later are not seen by that Server.
type Registry struct {
	mu        sync.Mutex
	pages     []Page
	routes    []Route
	nav       []NavItem
	providers []LoginProvider
	banners   []BannerFunc
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry { return &Registry{} }

// DefaultRegistry is the process-wide registry that [RegisterPage] and the
// other package-level functions fill and that the Hub's web module reads.
var DefaultRegistry = NewRegistry()

// RegisterPage adds a page to [DefaultRegistry]. It is meant for init
// functions of internal/hub/web/page_*.go; invalid input is a programming
// error and panics.
func RegisterPage(p Page) { must(DefaultRegistry.RegisterPage(p)) }

// RegisterRoute adds a route to [DefaultRegistry]; invalid input panics.
func RegisterRoute(rt Route) { must(DefaultRegistry.RegisterRoute(rt)) }

// RegisterNav adds a navigation entry to [DefaultRegistry]; invalid input panics.
func RegisterNav(n NavItem) { must(DefaultRegistry.RegisterNav(n)) }

// RegisterLoginProvider adds a login provider to [DefaultRegistry]; invalid
// input or a duplicate ID panics.
func RegisterLoginProvider(p LoginProvider) { must(DefaultRegistry.RegisterLoginProvider(p)) }

// RegisterBanner adds a banner source to [DefaultRegistry]; a nil func panics.
func RegisterBanner(f BannerFunc) { must(DefaultRegistry.RegisterBanner(f)) }

func must(err error) {
	if err != nil {
		panic(err)
	}
}

// splitPattern separates an optional method from the path of a route pattern.
func splitPattern(pattern string) (method, p string) {
	if i := strings.IndexByte(pattern, ' '); i >= 0 {
		return pattern[:i], strings.TrimSpace(pattern[i+1:])
	}
	return "", pattern
}

func checkPattern(pattern string, getOnly bool) (string, error) {
	method, p := splitPattern(strings.TrimSpace(pattern))
	if p == "/" {
		// A bare "/" is a subtree catch-all in net/http; it would shadow the
		// shell's 404 page for every unknown path.
		return "", fmt.Errorf("web: pattern %q is a catch-all; use \"/{$}\" for the home page", pattern)
	}
	if !strings.HasPrefix(p, "/") {
		return "", fmt.Errorf("web: pattern %q needs an absolute path", pattern)
	}
	if strings.ContainsAny(p, "\\ \t") || strings.HasPrefix(p, "//") {
		return "", fmt.Errorf("web: pattern %q is not a plain path", pattern)
	}
	for _, rp := range reservedPrefixes {
		if p == strings.TrimSuffix(rp, "/") || strings.HasPrefix(p, rp) {
			return "", fmt.Errorf("web: pattern %q is reserved (%s)", pattern, rp)
		}
	}
	if getOnly && method != "" && method != http.MethodGet {
		return "", fmt.Errorf("web: page pattern %q must be a GET pattern; use RegisterRoute for other methods", pattern)
	}
	if method == "" {
		method = http.MethodGet
	}
	return method + " " + p, nil
}

func checkRoles(roles []authz.Role) error {
	for _, r := range roles {
		if r != authz.RoleAdmin && r != authz.RoleUser {
			return fmt.Errorf("web: unknown role %q", r)
		}
	}
	return nil
}

// RegisterPage adds a page. It fails for an invalid pattern, a pattern that
// the shell reserves, an unknown role, a bad template name or a pattern that
// is already registered.
func (r *Registry) RegisterPage(p Page) error {
	pat, err := checkPattern(p.Pattern, true)
	if err != nil {
		return err
	}
	if p.Template != "" && (!templateName.MatchString(p.Template) || p.Template == layoutFile) {
		return fmt.Errorf("web: page %q: %q is not a valid template file name", p.Pattern, p.Template)
	}
	if err := checkRoles(p.Roles); err != nil {
		return err
	}
	p.Pattern = pat
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.hasPattern(pat) {
		return fmt.Errorf("web: pattern %q is registered twice", pat)
	}
	r.pages = append(r.pages, p)
	return nil
}

// RegisterRoute adds a route. See [Registry.RegisterPage] for the failures.
func (r *Registry) RegisterRoute(rt Route) error {
	pat, err := checkPattern(rt.Pattern, false)
	if err != nil {
		return err
	}
	if rt.Handler == nil {
		return fmt.Errorf("web: route %q has no handler", rt.Pattern)
	}
	if err := checkRoles(rt.Roles); err != nil {
		return err
	}
	rt.Pattern = pat
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.hasPattern(pat) {
		return fmt.Errorf("web: pattern %q is registered twice", pat)
	}
	r.routes = append(r.routes, rt)
	return nil
}

func (r *Registry) hasPattern(pat string) bool {
	for _, p := range r.pages {
		if p.Pattern == pat {
			return true
		}
	}
	for _, rt := range r.routes {
		if rt.Pattern == pat {
			return true
		}
	}
	return false
}

// RegisterNav adds a navigation entry.
func (r *Registry) RegisterNav(n NavItem) error {
	if strings.TrimSpace(n.Label) == "" {
		return errors.New("web: a navigation entry needs a label")
	}
	if !sameOriginPath(n.Path) {
		return fmt.Errorf("web: navigation entry %q: %q is not an absolute path on this Hub", n.Label, n.Path)
	}
	if err := checkRoles(n.Roles); err != nil {
		return err
	}
	if _, ok := navGroupLabels[n.Group]; n.Group != "" && !ok {
		return fmt.Errorf("web: navigation entry %q: unknown group %q", n.Label, n.Group)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nav = append(r.nav, n)
	return nil
}

// RegisterLoginProvider adds a login provider.
func (r *Registry) RegisterLoginProvider(p LoginProvider) error {
	if !providerID.MatchString(p.ID) {
		return fmt.Errorf("web: login provider ID %q must match %s", p.ID, providerID)
	}
	if strings.TrimSpace(p.Label) == "" {
		return fmt.Errorf("web: login provider %q needs a label", p.ID)
	}
	if !sameOriginPath(p.Path) {
		return fmt.Errorf("web: login provider %q: %q is not an absolute path on this Hub", p.ID, p.Path)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, o := range r.providers {
		if o.ID == p.ID {
			return fmt.Errorf("web: login provider %q is registered twice", p.ID)
		}
	}
	r.providers = append(r.providers, p)
	return nil
}

// RegisterBanner adds a banner source.
func (r *Registry) RegisterBanner(f BannerFunc) error {
	if f == nil {
		return errors.New("web: a banner source needs a function")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.banners = append(r.banners, f)
	return nil
}

// snapshot is what a Server works from: a sorted copy of the registry.
type snapshot struct {
	pages     []Page
	routes    []Route
	nav       []NavItem
	providers []LoginProvider
	banners   []BannerFunc
}

func (r *Registry) snapshot() snapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := snapshot{
		pages:     slices.Clone(r.pages),
		routes:    slices.Clone(r.routes),
		nav:       slices.Clone(r.nav),
		providers: slices.Clone(r.providers),
		banners:   slices.Clone(r.banners),
	}
	sort.SliceStable(s.nav, func(i, j int) bool {
		if s.nav[i].Order != s.nav[j].Order {
			return s.nav[i].Order < s.nav[j].Order
		}
		return s.nav[i].Label < s.nav[j].Label
	})
	sort.SliceStable(s.providers, func(i, j int) bool {
		if s.providers[i].Order != s.providers[j].Order {
			return s.providers[i].Order < s.providers[j].Order
		}
		return s.providers[i].ID < s.providers[j].ID
	})
	return s
}

// SessionLayer authenticates the browser session of a request and enforces
// the CSRF check for unsafe methods; *session.Middleware implements it.
type SessionLayer interface {
	Wrap(next http.Handler) http.Handler
}

var _ SessionLayer = (*session.Middleware)(nil)

// Config builds a [Server].
type Config struct {
	// Registry is what the other stories registered; nil means [DefaultRegistry].
	Registry *Registry
	// Sessions is required.
	Sessions SessionLayer
	// Templates is the template set, rooted at the templates directory; nil
	// means [EmbeddedTemplates]. It must contain layout.html.
	Templates fs.FS
	// Secure decides the Secure attribute of the flash cookie; nil means true.
	Secure func(r *http.Request) bool
	// Log receives render failures; nil means [slog.Default].
	Log *slog.Logger
	// ContextKey is the key of the inspector's identity context (page_inspector_context.go),
	// at least [MinContextKey] bytes. It is required and has no default: the context must
	// stay the same for the same browser login across restarts of the Hub, so the Hub derives
	// it from its persisted data key (store.SecretBox.Derive). A process-random key would make
	// every restart look like another sign-in.
	ContextKey []byte
}

// MinContextKey is the shortest accepted [Config.ContextKey].
const MinContextKey = 32

// Server serves the web UI. It is safe for concurrent use.
type Server struct {
	sessions  SessionLayer
	snap      snapshot
	templates map[string]*template.Template // "" is the layout with its default content
	handler   http.Handler                  // the page routes behind the session layer
	static    http.Handler
	secure    func(*http.Request) bool
	log       *slog.Logger
	ctxKey    []byte // the key of the inspector identity context
}

// Files of the shell.
const (
	layoutFile   = "layout.html"
	loginFile    = "login.html"
	passwordFile = "change_password.html"
)

// New builds a Server: it parses every template (so a missing or broken page
// template fails at start, not on the first request) and builds the routes.
func New(cfg Config) (*Server, error) {
	if cfg.Sessions == nil {
		return nil, errors.New("web: a session layer is required")
	}
	if len(cfg.ContextKey) < MinContextKey {
		return nil, fmt.Errorf("web: the inspector context key must be at least %d bytes (the Hub derives it from its data key)", MinContextKey)
	}
	reg := cfg.Registry
	if reg == nil {
		reg = DefaultRegistry
	}
	s := &Server{sessions: cfg.Sessions, snap: reg.snapshot(), secure: cfg.Secure, log: cfg.Log, ctxKey: append([]byte(nil), cfg.ContextKey...)}
	if s.secure == nil {
		s.secure = func(*http.Request) bool { return true }
	}
	if s.log == nil {
		s.log = slog.Default()
	}
	tfs := cfg.Templates
	if tfs == nil {
		tfs = EmbeddedTemplates()
	}
	files := []string{loginFile, passwordFile}
	for _, p := range s.snap.pages {
		if p.Template != "" && !slices.Contains(files, p.Template) {
			files = append(files, p.Template)
		}
	}
	var err error
	if s.templates, err = loadTemplates(tfs, files); err != nil {
		return nil, err
	}
	if s.static, err = newStaticHandler(EmbeddedStatic()); err != nil {
		return nil, err
	}
	if s.handler, err = s.buildRoutes(); err != nil {
		return nil, err
	}
	return s, nil
}

// loadTemplates parses the layout and the partials ("_*.html") into a base
// set and clones it once per page file. A page file defines "content"; the
// layout's own "content" block is the default for pages without a file.
func loadTemplates(fsys fs.FS, files []string) (map[string]*template.Template, error) {
	base, err := template.ParseFS(fsys, layoutFile)
	if err != nil {
		return nil, fmt.Errorf("web: load %s: %w", layoutFile, err)
	}
	partials, err := fs.Glob(fsys, "_*.html")
	if err != nil {
		return nil, fmt.Errorf("web: list partials: %w", err)
	}
	for _, p := range partials {
		if base, err = base.ParseFS(fsys, p); err != nil {
			return nil, fmt.Errorf("web: load partial %s: %w", p, err)
		}
	}
	out := map[string]*template.Template{}
	for _, f := range files {
		c, err := base.Clone()
		if err != nil {
			return nil, fmt.Errorf("web: clone layout for %s: %w", f, err)
		}
		if c, err = c.ParseFS(fsys, f); err != nil {
			return nil, fmt.Errorf("web: load template %s: %w", f, err)
		}
		out[f] = c
	}
	out[""] = base
	return out, nil
}

// buildRoutes registers the shell's routes and the registered pages and
// routes. A conflicting pattern is an error, not a panic.
func (s *Server) buildRoutes() (h http.Handler, err error) {
	mux := http.NewServeMux()
	defer func() {
		if p := recover(); p != nil {
			h, err = nil, fmt.Errorf("web: build routes: %v", p)
		}
	}()
	mux.Handle("GET "+pathLogin, http.HandlerFunc(s.handleLogin))
	mux.Handle("GET "+pathPassword, s.gate(nil, true, http.HandlerFunc(s.handleChangePassword)))
	haveHome := false
	for _, p := range s.snap.pages {
		if p.Pattern == "GET /" || p.Pattern == "GET /{$}" {
			haveHome = true
		}
		mux.Handle(p.Pattern, s.gate(p.Roles, false, s.pageHandler(p)))
	}
	for _, rt := range s.snap.routes {
		mux.Handle(rt.Pattern, s.gate(rt.Roles, false, rt.Handler))
	}
	if !haveHome {
		mux.Handle("GET /{$}", s.gate(nil, false, http.HandlerFunc(s.handleHome)))
	}
	mux.Handle("/", http.HandlerFunc(s.handleNotFound))
	return s.sessions.Wrap(mux), nil
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, pathStatic) {
		s.static.ServeHTTP(w, r)
		return
	}
	ctx := context.WithValue(r.Context(), secureKey{}, s.secure(r))
	r = r.WithContext(context.WithValue(ctx, contextKeyKey{}, s.ctxKey))
	s.handler.ServeHTTP(w, r)
}

// newStaticHandler serves the embedded assets. Directory listings are never
// served, every file has a content-derived ETag, and the response may be
// cached but must be revalidated.
func newStaticHandler(fsys fs.FS) (http.Handler, error) {
	type asset struct {
		data []byte
		etag string
	}
	assets := map[string]asset{}
	err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := fs.ReadFile(fsys, p)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(b)
		assets[p] = asset{data: b, etag: `"` + hex.EncodeToString(sum[:8]) + `"`}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("web: read static assets: %w", err)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		name := strings.TrimPrefix(path.Clean("/"+strings.TrimPrefix(r.URL.Path, pathStatic)), "/")
		a, ok := assets[name]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("ETag", a.etag)
		w.Header().Set("Cache-Control", "no-cache")
		http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(a.data))
	}), nil
}
