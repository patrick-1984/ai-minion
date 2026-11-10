package api

import (
	"fmt"
	"net/http"
	"net/url"
	"path"
	"reflect"
	"regexp"
	"strings"

	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// Prefix is the root of the REST API (AD-18).
const Prefix = "/api/v1/"

// Alias is a resource-style route for an operation, for example
// GET /api/v1/audit for audit_list. It runs through exactly the same
// pipeline as POST /api/v1/ops/<name>.
type Alias struct {
	// Method is the HTTP method: GET or POST for a read operation, any of
	// POST, PUT, PATCH and DELETE for a write operation.
	Method string
	// Pattern is a net/http route path under /api/v1/ (not /api/v1/ops/),
	// for example "/api/v1/workers/{worker_id}/share". Each {wildcard} fills
	// the argument member of the same name, or the dotted member named in
	// Params. {worker_id} also routes a Worker operation whose arguments name
	// no Worker themselves (sessions_list, session_create): the Worker is
	// then decided on as for the canonical route.
	Pattern string
	// Op is the catalog operation name.
	Op string
	// Params maps a wildcard name to the dotted JSON path of the argument it
	// fills, for example {"worker_id": "target.worker_id"}.
	Params map[string]string
}

var wildcard = regexp.MustCompile(`\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// Router mounts the catalog-driven routes on a mux.
type Router struct {
	d   *Dispatcher
	mux *http.ServeMux
}

// NewRouter mounts, on mux:
//
//   - POST /api/v1/ops/<name> for every operation of the catalog that has a
//     handler, and GET for operations that are not writes (the arguments are
//     then the query string; nested members use dots, target.worker_id=...);
//   - every other path under /api/v1/ answers an enveloped not_found, so that
//     no API error has another shape. More specific routes that other modules
//     register on the same mux win over it.
//
// An operation without a handler answers unsupported_op. Handlers can be
// registered, and aliases added, at any time before the Hub serves.
func NewRouter(d *Dispatcher, mux *http.ServeMux) (*Router, error) {
	if d == nil || d.Catalog == nil || d.Handlers == nil || d.Auth == nil || d.Authz == nil {
		return nil, fmt.Errorf("api: a Dispatcher needs Catalog, Handlers, Auth and Authz")
	}
	rt := &Router{d: d, mux: mux}
	err := register(func() {
		mux.HandleFunc(Prefix+"ops/{name}", func(w http.ResponseWriter, r *http.Request) {
			d.serve(w, r, route{op: r.PathValue("name")})
		})
		mux.HandleFunc(Prefix, func(w http.ResponseWriter, r *http.Request) {
			d.armBody(w, r)
			WriteError(w, NewError(proto.CodeNotFound, "no such API route"))
		})
	})
	if err != nil {
		return nil, err
	}
	return rt, nil
}

// Handler wraps the handler of the mux (next) so that the body deadline is
// in force for the answers the mux gives before any route runs. A ServeMux
// redirects a path that is not clean (a repeated slash, a dot segment) and
// the bare prefix /api/v1 itself without calling a handler, and net/http then
// reads the rest of an unread body before it sends the redirect: without the
// deadline a client that announces a body and stops would hold the
// connection for ever. Every other request is left to its route, which arms
// the deadline itself.
func (rt *Router) Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if muxRedirects(r) {
			rt.d.armBody(w, r)
		}
		next.ServeHTTP(w, r)
	})
}

// muxRedirects reports whether a ServeMux answers r with a redirect for the
// shape of its path (as ServeMux does: CONNECT is never cleaned) or because
// it is the bare API prefix, whose route needs the trailing slash.
func muxRedirects(r *http.Request) bool {
	p := r.URL.EscapedPath()
	if p == strings.TrimSuffix(Prefix, "/") {
		return true
	}
	return r.Method != http.MethodConnect && cleanPath(p) != p
}

// cleanPath is the path cleaning of net/http's ServeMux: a rooted, clean path
// that keeps a trailing slash.
func cleanPath(p string) string {
	if p == "" {
		return "/"
	}
	if p[0] != '/' {
		p = "/" + p
	}
	np := path.Clean(p)
	if p[len(p)-1] == '/' && np != "/" {
		if len(p) == len(np)+1 && strings.HasPrefix(p, np) {
			np = p
		} else {
			np += "/"
		}
	}
	return np
}

// register runs fn and turns a pattern conflict (a panic of the mux) into an
// error.
func register(fn func()) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("api: cannot register the route: %v", p)
		}
	}()
	fn()
	return nil
}

// AddAlias validates and mounts a resource alias. It fails for an operation
// that is not in the catalog, a method that does not fit the operation (GET on
// a write, DELETE on a read), a pattern outside /api/v1/ or inside
// /api/v1/ops/, a wildcard that fills no argument member, or a route that
// conflicts with an existing one.
func (rt *Router) AddAlias(a Alias) error {
	op, ok := rt.d.Catalog.Lookup(a.Op)
	if !ok {
		return fmt.Errorf("api: alias %s %s: %q is not an operation of the catalog", a.Method, a.Pattern, a.Op)
	}
	if err := validateAlias(a, op); err != nil {
		return err
	}
	return register(func() {
		rt.mux.HandleFunc(a.Method+" "+a.Pattern, func(w http.ResponseWriter, r *http.Request) {
			vals := url.Values{}
			for _, m := range wildcard.FindAllStringSubmatch(a.Pattern, -1) {
				path := m[1]
				if p, ok := a.Params[m[1]]; ok {
					path = p
				}
				vals.Set(path, r.PathValue(m[1]))
			}
			rt.d.serve(w, r, route{op: a.Op, path: vals, methods: []string{a.Method}})
		})
	})
}

// routingOnlyWorker reports whether path is the worker_id that routes a Worker
// operation whose arguments have neither a worker_id member nor a Target (see
// [Dispatcher]). An operation whose arguments name a Worker themselves needs
// an explicit Params mapping, so that the path can never disagree with them.
func routingOnlyWorker(op ops.Op, path string) bool {
	return path == "worker_id" && op.Scope == ops.ScopeWorker && !namesWorker(op.Args)
}

// namesWorker reports whether an argument struct type carries a Target or a
// worker_id member.
func namesWorker(t reflect.Type) bool {
	t = derefType(t)
	if t.Kind() != reflect.Struct {
		return false
	}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		ft := derefType(f.Type)
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		switch {
		case ft == targetType:
			return true
		case f.Anonymous && name == "" && ft.Kind() == reflect.Struct:
			if namesWorker(ft) {
				return true
			}
		case name == "worker_id" && f.IsExported():
			return true
		}
	}
	return false
}

func validateAlias(a Alias, op ops.Op) error {
	if !strings.HasPrefix(a.Pattern, Prefix) || strings.HasPrefix(a.Pattern, Prefix+"ops/") || a.Pattern == Prefix+"ops" {
		return fmt.Errorf("api: alias %s %s: the pattern must be under %s and not under %sops/", a.Method, a.Pattern, Prefix, Prefix)
	}
	switch a.Method {
	case http.MethodGet:
		if op.Write || op.Mutates {
			return fmt.Errorf("api: alias GET %s: %s is a write operation (it changes state); use POST, PUT, PATCH or DELETE", a.Pattern, op.Name)
		}
	case http.MethodPost:
	case http.MethodPut, http.MethodPatch, http.MethodDelete:
		if !op.Write {
			return fmt.Errorf("api: alias %s %s: %s is a read operation; use GET or POST", a.Method, a.Pattern, op.Name)
		}
	default:
		return fmt.Errorf("api: alias %s %s: unsupported method", a.Method, a.Pattern)
	}
	for _, m := range wildcard.FindAllStringSubmatch(a.Pattern, -1) {
		path := m[1]
		if p, ok := a.Params[m[1]]; ok {
			path = p
		}
		if _, ok := fieldTypeAt(op.Args, strings.Split(path, ".")); !ok && !routingOnlyWorker(op, path) {
			return fmt.Errorf("api: alias %s %s: {%s} fills %q, which is not an argument of %s", a.Method, a.Pattern, m[1], path, op.Name)
		}
	}
	for w := range a.Params {
		if !strings.Contains(a.Pattern, "{"+w+"}") {
			return fmt.Errorf("api: alias %s %s: Params names {%s}, which is not in the pattern", a.Method, a.Pattern, w)
		}
	}
	return nil
}
