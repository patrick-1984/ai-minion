package ratelimit

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net"
	"net/netip"
	"strings"
)

// maxKeyLen is the longest target or account key kept verbatim. Longer input
// is replaced by a SHA-256 digest so that table memory stays bounded however
// large an attacker-chosen name is.
const maxKeyLen = 128

// invalidIPKey is the shared bucket key for a request whose address could not
// be determined. Sharing one bucket fails closed: unknown clients compete for
// the same tokens instead of each getting a fresh bucket.
const invalidIPKey = "invalid"

// ipv6PrefixBits is the IPv6 grouping: one /64 is one client, because a single
// subscriber normally controls a whole /64.
const ipv6PrefixBits = 64

// IPKey returns the bucket key of a client address: the address itself for
// IPv4 (/32) and the masked /64 network for IPv6. An IPv4-mapped IPv6 address
// is treated as IPv4 and an IPv6 zone is dropped. The zero Addr maps to a
// single shared key.
func IPKey(addr netip.Addr) string {
	if !addr.IsValid() {
		return invalidIPKey
	}
	addr = addr.Unmap().WithZone("")
	if addr.Is4() {
		return addr.String()
	}
	p, err := addr.Prefix(ipv6PrefixBits)
	if err != nil {
		return invalidIPKey
	}
	return p.String()
}

// ParseRemote extracts the client address from an http.Request.RemoteAddr
// style string. It accepts "host:port", "[v6]:port" and a bare address.
func ParseRemote(remote string) (netip.Addr, error) {
	host := strings.TrimSpace(remote)
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}, errors.New("ratelimit: not an IP address")
	}
	return addr.Unmap().WithZone(""), nil
}

// NormalizeKey canonicalises a target or account name for bucket lookup: it
// trims space, lower-cases and replaces over-long input by a digest. Names that
// differ only in case therefore share a bucket, which can only make the limit
// stricter, never looser. The empty string stays empty.
//
// The result never shares memory with the argument: TrimSpace and ToLower may
// return a substring of a large caller buffer (a request body, a padded name),
// so a short result is copied and a long one is replaced by a digest.
func NormalizeKey(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if len(s) > maxKeyLen {
		sum := sha256.Sum256([]byte(s))
		return "sha256:" + hex.EncodeToString(sum[:])
	}
	return strings.Clone(s)
}
