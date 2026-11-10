// Package session implements browser sessions and CSRF protection (AD-10).
//
// Owner: Story 3.5.
//
// A browser session is an opaque 32-byte random value in the cookie
// "minion_session" (HttpOnly, SameSite=Lax, Secure unless MINION_PUBLIC_URL is
// plain-http loopback). Only its SHA-256 is stored, so a leaked database does
// not give out working cookies. No JWTs are used.
//
// The CSRF token of a session is derived from the cookie value with HMAC-SHA256
// (see [CSRFToken]); it is bound to that session, needs no extra storage and
// is handed to the browser at login. Every request other than GET that
// carries a session cookie must present it in the X-Minion-CSRF header, else
// [Middleware.Wrap] answers forbidden.
//
//   - session.go: the [Manager] (create, resolve, revoke), cookie helpers and
//     the CSRF token;
//   - middleware.go: the HTTP middleware, the request context accessor and the
//     AD-18 error envelope used by the auth endpoints.
package session
