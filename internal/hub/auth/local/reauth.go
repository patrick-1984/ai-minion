package local

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/auth/session"
	"github.com/patrick-1984/ai-minion/internal/hub/httpbody"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// AuditActionReauthFailed is the audit action of a failed re-authentication
// (contract section 8.5). It never counts as a passphrase failure.
const AuditActionReauthFailed = "hub_identity.reauth_failed"

// maxReauthBody bounds the body of a re-authentication request.
const maxReauthBody = 4 << 10

// reauthResponse is the success body of POST /auth/reauth.
type reauthResponse struct {
	AuthMethod string        `json:"auth_method"`
	AuthAt     ops.Timestamp `json:"auth_at"`
	ValidUntil ops.Timestamp `json:"valid_until"`
}

// Reauth handles POST /auth/reauth (contract section 8.1 item 2, local
// method): the account password of the session's own user is verified again,
// and only then are auth_at and auth_method of that same session updated. It
// sits behind the session middleware, so the session cookie and the CSRF
// token are required. A session that signed in through an identity provider
// cannot use it: the answer is reauth_unavailable and nothing changes.
func (h *Handler) Reauth(w http.ResponseWriter, r *http.Request) {
	sess, ok := session.FromContext(r.Context())
	if !ok {
		session.WriteError(w, proto.CodeUnauthenticated, "sign in first", nil)
		return
	}
	httpbody.Arm(w, r, h.BodyTimeout)
	r.Body = http.MaxBytesReader(w, r.Body, maxReauthBody)
	req, err := parseLogin(r)
	if err != nil || req.Password == "" {
		session.WriteError(w, proto.CodeInvalidArgument, "send the password", nil)
		return
	}
	if sess.ActAs != nil || (sess.AuthMethod != "" && sess.AuthMethod != session.AuthMethodLocal) {
		h.reauthFailed(r, sess, "unavailable")
		session.WriteError(w, proto.CodeReauthUnavailable, "this session did not sign in with a local password; re-authenticate with your identity provider", nil)
		return
	}
	user, err := h.Auth.Authenticate(r.Context(), Credentials{Identifier: sess.Username, Password: req.Password, IP: h.ClientIP(r)})
	var limited *RateLimitedError
	switch {
	case errors.As(err, &limited):
		secs := int(limited.RetryAfter/time.Second) + 1
		w.Header().Set("Retry-After", strconv.Itoa(secs))
		h.reauthFailed(r, sess, "rate_limited")
		session.WriteError(w, proto.CodeRateLimited, "too many attempts; retry later", map[string]any{"retry_after_seconds": secs})
		return
	case errors.Is(err, ErrInvalidCredentials):
		h.reauthFailed(r, sess, "credentials")
		session.WriteError(w, proto.CodeReauthRequired, "the password was not accepted", nil)
		return
	case err != nil:
		h.fail(w, "re-authenticate", err)
		return
	}
	if user.ID != sess.UserID {
		// The identifier named another account than the session's: never raise.
		h.reauthFailed(r, sess, "credentials")
		session.WriteError(w, proto.CodeReauthRequired, "the password was not accepted", nil)
		return
	}
	at, err := h.Sessions.MarkAuthenticated(r.Context(), sess.ID, session.AuthMethodLocal)
	if err != nil {
		h.fail(w, "record the authentication", err)
		return
	}
	session.WriteJSON(w, http.StatusOK, reauthResponse{
		AuthMethod: session.AuthMethodLocal,
		AuthAt:     ops.Timestamp(at),
		ValidUntil: ops.Timestamp(at.Add(session.ReauthWindow)),
	})
}

// reauthFailed audits one failed re-authentication; the reason is a short
// class, never a credential.
func (h *Handler) reauthFailed(r *http.Request, sess *session.Session, reason string) {
	h.audit(r.Context(), sess.UserID, "method=local reason="+reason, AuditActionReauthFailed, OutcomeFailed)
}
