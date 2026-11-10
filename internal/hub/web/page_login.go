package web

import (
	"net/http"
	"net/url"

	"github.com/patrick-1984/ai-minion/internal/hub/auth/session"
	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// LoginData is what login.html sees as .Data.
type LoginData struct {
	// Next is the path to open after signing in; always a path on this Hub.
	Next string
	// Providers are the registered external sign-in methods.
	Providers []LoginProvider
}

// PasswordData is what change_password.html sees as .Data.
type PasswordData struct {
	// MustChange is true when the account has to choose a new password before
	// it can use anything else.
	MustChange bool
	// Action is the URL the form posts its JSON to ([PasswordChangeURL]).
	Action string
}

// PasswordChangeURL is the operation endpoint the change-password form posts
// to (Story 3.10's `password_change`). The form sends JSON with the fields
// "old_password" and "new_password".
const PasswordChangeURL = ops.RESTPrefix + "password_change"

// redirect sends the browser to target. An htmx request cannot follow a 3xx
// to a whole page without swapping it into a fragment, so it gets HX-Redirect
// instead.
func redirect(w http.ResponseWriter, r *http.Request, target string) {
	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Redirect", target)
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

func loginTarget(r *http.Request) string {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return pathLogin
	}
	next := r.URL.RequestURI()
	if r.Header.Get("HX-Request") == "true" {
		// The request URL of an htmx call is a fragment; the page the user is
		// on is in HX-Current-URL. Keep only its path and query.
		next = ""
		if u, err := url.Parse(r.Header.Get("HX-Current-URL")); err == nil && u.Path != "" {
			next = u.RequestURI()
		}
	}
	if !sameOriginPath(next) || next == "/" {
		return pathLogin
	}
	return pathLogin + "?next=" + url.QueryEscape(next)
}

// gate wraps a handler with the checks every signed-in page needs, in this
// order: a session (else the login page), the must-change rule (else the
// change-password page, unless exempt is set for that page itself), and the
// role filter (else 403). A caller that is not signed in learns nothing about
// which pages exist.
func (s *Server) gate(roles []authz.Role, exempt bool, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sess, ok := session.FromContext(r.Context())
		if !ok {
			if r.Header.Get("HX-Request") != "true" && r.Method != http.MethodGet && r.Method != http.MethodHead {
				session.WriteError(w, proto.CodeUnauthenticated, "sign in first", nil)
				return
			}
			redirect(w, r, loginTarget(r))
			return
		}
		if sess.MustChangePassword && !exempt {
			redirect(w, r, pathPassword)
			return
		}
		if !hasRole(roles, sess.Role) {
			s.renderStatus(w, r, http.StatusForbidden, "You are not allowed to open this page.")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// handleLogin serves GET /login. A signed-in browser goes on to where it was
// headed; everybody else gets the form.
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	next := safeNext(r.URL.Query().Get("next"))
	if sess, ok := session.FromContext(r.Context()); ok {
		if sess.MustChangePassword {
			redirect(w, r, pathPassword)
			return
		}
		redirect(w, r, next)
		return
	}
	s.render(w, r, http.StatusOK, loginFile, "Sign in", LoginData{Next: next, Providers: s.snap.providers})
}

// handleChangePassword serves GET /account/password.
func (s *Server) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	sess, _ := session.FromContext(r.Context())
	s.render(w, r, http.StatusOK, passwordFile, "Change password", PasswordData{MustChange: sess != nil && sess.MustChangePassword, Action: PasswordChangeURL})
}

// handleHome serves "/" until a story registers a page there (Story 5.4).
func (s *Server) handleHome(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, http.StatusOK, "", "Home", nil)
}

// handleNotFound answers every path nothing else claims. Anonymous callers are
// sent to the login page and must-change accounts to the change-password page,
// like for every other page.
func (s *Server) handleNotFound(w http.ResponseWriter, r *http.Request) {
	s.gate(nil, false, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.renderStatus(w, r, http.StatusNotFound, "That page does not exist.")
	})).ServeHTTP(w, r)
}
