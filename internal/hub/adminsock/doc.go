// Package adminsock is the Hub's local admin socket: a Unix socket in the data
// directory (mode 0600, the Hub's own user) over which the commands of
// `minion hub identity` reach the running Hub (docs/architecture/proposals/
// hub-identity-v2.md, section 8.3). The Hub process holds the rotation mutex
// and does the work; the passphrase never crosses a network or a reverse
// proxy.
//
// The protocol is small HTTP over the socket with the argument and result
// types of the operation catalog (internal/ops). Authorization is the
// operating-system permission on the socket plus the kernel credentials of the
// peer (SO_PEERCRED): only the Hub's own user and root are served, and the
// audit actor is "local:<uid>", never taken from client input. The REST
// authorization, the fresh re-authentication, CSRF and the HTTPS rule of the
// web do not apply: the socket is not reachable from a browser. The commands
// have their own failure counter, lockout and hourly allowance (the "cli"
// scope of internal/hub/hubid).
package adminsock
