// Package oidc implements OIDC login with role rules (AD-10, Story 6.2). It is
// the only package that imports go-oidc and oauth2 (AD-1).
//
// Owner: Story 6.2.
//
// The package is split into small parts with ports between them:
//
//   - provider.go: the [Config] of a provider, discovery, the HTTP client
//     (deadline, private CA file, the explicit insecure flag), the
//     authorization-code URL with PKCE (S256), state and nonce, the code
//     exchange and the verification of the ID token.
//   - roles.go: [Rules], the claim-to-role evaluation. It reads only the
//     configured claim; a missing or empty claim, or no matching value, yields
//     no roles, and no roles means no access.
//   - login.go: the [Service] with the two HTTP endpoints. The callback turns a
//     verified identity, (issuer, subject), into an account and its source-idp
//     roles in one transaction, then into a browser session.
//
// Persistence is the store's: [Store] and [ConfigSource] are the ports, and the
// tests run the whole flow against an in-process fake issuer over TLS with a
// private CA.
//
// Every network call has a deadline (10 s by default). A failure ends the
// sign-in with an English error page and changes no roles. Secrets (the client
// secret, the refresh token) are redacted in logs; the refresh token is stored
// encrypted.
package oidc
