// Package ratelimit provides the shared rate limiter used by the Hub and the
// Worker (AD-19). It is part of the shared kernel and imports only the
// standard library.
//
// Owner: Story 1.8.
//
// One Limiter protects every brute-force-prone entry point: login, enroll,
// Worker connect, pairing and failed API-key attempts. Each named policy has
// two token buckets, one per client IP address (IPv4 by /32, IPv6 by /64) and
// one per target (account name, invitation fingerprint, pairing id, key id).
// A request is allowed only when both buckets have a token, and consumes from
// both only then. Logins also get an exponential back-off per account.
//
// Behaviour is deterministic: time comes from an injected Clock. Memory is
// bounded: every table is an LRU with a documented cap (DefaultMaxEntries).
// Only idle entries are evicted; a full table of active entries refuses new
// keys instead of erasing a drained bucket or a running back-off.
// The limiter starts no goroutines and needs no Close.
//
// Usage on a login endpoint:
//
//	ip, _ := ratelimit.ParseRemote(r.RemoteAddr)
//	if ok, wait := l.Allow(ratelimit.PolicyLogin, ip, username); !ok { /* 429, Retry-After: wait */ }
//	if wait := l.Backoff(username); wait > 0 { /* refuse, Retry-After: wait */ }
//	// ... verify the password ...
//	if failed { l.BackoffFail(username) } else { l.BackoffReset(username) }
package ratelimit
