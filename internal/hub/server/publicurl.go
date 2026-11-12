package server

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync/atomic"
)

// Network answers the questions every module asks about how a request
// reached the Hub: the client address, the scheme and the public URL
// (AD-18). Behind a reverse proxy the forwarding headers are honoured only
// when the direct peer is in the trusted-proxy list; a request from any other
// peer cannot spoof its address or scheme.
//
// A Network is immutable and safe for concurrent use. Modules get it as a
// service ([Require]), or through the package functions [ClientIP] and
// [PublicURL].
type Network struct {
	publicURL string // normalised, no trailing slash; empty when unset
	tls       bool   // the Hub itself terminates TLS
	proxies   []netip.Prefix
}

// NewNetwork builds a Network. publicURL may be empty. proxies is the parsed
// trusted-proxy list ([ParseTrustedProxies]). tls says whether the Hub
// terminates TLS itself.
func NewNetwork(publicURL string, tls bool, proxies []netip.Prefix) (*Network, error) {
	pu, err := NormalizePublicURL(publicURL)
	if err != nil {
		return nil, err
	}
	return &Network{publicURL: pu, tls: tls, proxies: append([]netip.Prefix(nil), proxies...)}, nil
}

// NormalizePublicURL validates MINION_PUBLIC_URL and returns it without a
// trailing slash. It must be an absolute http or https URL with a host and
// without credentials, path, query or fragment: the Hub is served at the root
// of its origin. An empty value is valid and stays empty.
func NormalizePublicURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", errors.New("MINION_PUBLIC_URL is not a valid URL; use the form https://hub.example.com")
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.Hostname() == "" {
		return "", errors.New("MINION_PUBLIC_URL must be an absolute http or https URL such as https://hub.example.com")
	}
	if u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
		return "", errors.New("MINION_PUBLIC_URL must be only scheme, host and optional port, for example https://hub.example.com")
	}
	return u.Scheme + "://" + strings.ToLower(u.Host), nil
}

// ParseTrustedProxies parses a comma-separated list of CIDRs or single
// addresses (a single address means /32 or /128). Spaces are ignored and an
// empty list is valid.
func ParseTrustedProxies(list string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for _, part := range strings.Split(list, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if p, err := netip.ParsePrefix(part); err == nil {
			out = append(out, netip.PrefixFrom(p.Addr().Unmap(), unmapBits(p)).Masked())
			continue
		}
		a, err := netip.ParseAddr(part)
		if err != nil {
			return nil, fmt.Errorf("MINION_TRUSTED_PROXIES: %q is not a CIDR or an IP address", part)
		}
		a = a.Unmap().WithZone("")
		out = append(out, netip.PrefixFrom(a, a.BitLen()))
	}
	return out, nil
}

// unmapBits returns the prefix length of p after its address is unmapped from
// IPv4-in-IPv6 form (::ffff:10.0.0.0/104 is 10.0.0.0/8).
func unmapBits(p netip.Prefix) int {
	if p.Addr().Is4In6() && p.Bits() >= 96 {
		return p.Bits() - 96
	}
	return p.Bits()
}

// PublicURL returns the configured MINION_PUBLIC_URL without a trailing
// slash, or "" when it is not set. It is the source for the cookie Secure
// flag, the OIDC redirect URI and absolute links; use [Network.BaseURL] when
// a request is at hand and a link must be built even without a setting.
func (n *Network) PublicURL() string { return n.publicURL }

// Secure reports whether browsers reach the Hub over HTTPS, which decides the
// cookie Secure flag: the public URL says so when it is set, otherwise the
// Hub's own TLS mode.
func (n *Network) Secure() bool {
	if n.publicURL != "" {
		return strings.HasPrefix(n.publicURL, "https://")
	}
	return n.tls
}

// BaseURL returns the origin for absolute links in the response to r: the
// public URL when it is set, otherwise the scheme and Host header of r.
func (n *Network) BaseURL(r *http.Request) string {
	if n.publicURL != "" {
		return n.publicURL
	}
	return n.Scheme(r) + "://" + r.Host
}

// trusted reports whether a is a configured reverse proxy.
func (n *Network) trusted(a netip.Addr) bool {
	a = a.Unmap().WithZone("")
	for _, p := range n.proxies {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// peer returns the address of the direct peer of r.
func peer(r *http.Request) netip.Addr {
	host := r.RemoteAddr
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	if a, err := netip.ParseAddr(host); err == nil {
		return a.Unmap().WithZone("")
	}
	return netip.Addr{}
}

// ClientIP returns the address of the client that sent r. Without trusted
// proxies, or when the direct peer is not one of them, that is the peer
// address. From a trusted proxy, X-Forwarded-For is read from the right, and
// the first address that is not itself a trusted proxy is the client; a
// header that cannot be parsed is ignored. The result is the zero Addr when
// even the peer address is unknown.
func (n *Network) ClientIP(r *http.Request) netip.Addr {
	p := peer(r)
	if !p.IsValid() || !n.trusted(p) {
		return p
	}
	hops := strings.Split(strings.Join(r.Header.Values("X-Forwarded-For"), ","), ",")
	for i := len(hops) - 1; i >= 0; i-- {
		a, err := netip.ParseAddr(strings.TrimSpace(hops[i]))
		if err != nil {
			return p // junk from the proxy side: do not guess
		}
		a = a.Unmap().WithZone("")
		if !n.trusted(a) {
			return a
		}
	}
	return p
}

// Scheme returns "https" or "http" for the connection the client used. A
// trusted proxy knows the client's protocol, so a valid X-Forwarded-Proto
// from a trusted peer wins, whatever the proxy-to-Hub transport is (a proxy
// may reach the Hub over TLS and still serve its clients over plain HTTP).
// Otherwise the scheme follows the Hub's own TLS state.
func (n *Network) Scheme(r *http.Request) string {
	if p := peer(r); p.IsValid() && n.trusted(p) {
		// The proxy appends its own value last; the rightmost is the one
		// the trusted hop set.
		vals := strings.Split(strings.Join(r.Header.Values("X-Forwarded-Proto"), ","), ",")
		switch strings.ToLower(strings.TrimSpace(vals[len(vals)-1])) {
		case "https":
			return "https"
		case "http":
			return "http"
		}
	}
	if r.TLS != nil {
		return "https"
	}
	return "http"
}

// EffectiveHTTPS reports whether the browser's request arrived over HTTPS, for
// the operations that accept a passphrase (contract section 8.1 item 5). The
// decision is the effective scheme of [Network.Scheme]: the TLS state of the
// connection itself, or a forwarded protocol header that counts only when the
// immediate peer is inside the configured trusted-proxy networks. The loopback
// address of the peer is never by itself a reason to accept cleartext, and a
// forwarded header from an untrusted peer is ignored. The one exception is a
// request that arrived on the configured management listener.
func (n *Network) EffectiveHTTPS(r *http.Request) bool {
	if onManagementListener(r) {
		return true
	}
	return n.Scheme(r) == "https"
}

// The process-wide Network behind [ClientIP] and [PublicURL]. [Server.Run]
// installs it while the Hub runs.
var defaultNetwork atomic.Pointer[Network]

func setDefaultNetwork(n *Network) { defaultNetwork.Store(n) }

func clearDefaultNetwork(n *Network) { defaultNetwork.CompareAndSwap(n, nil) }

// ClientIP returns the client address of r as the running Hub sees it, with
// the trusted-proxy rules of [Network.ClientIP]. Without a running Hub it
// returns the peer address.
func ClientIP(r *http.Request) netip.Addr {
	if n := defaultNetwork.Load(); n != nil {
		return n.ClientIP(r)
	}
	return peer(r)
}

// PublicURL returns the running Hub's MINION_PUBLIC_URL, or "" when it is not
// set or no Hub runs. See [Network.PublicURL].
func PublicURL() string {
	if n := defaultNetwork.Load(); n != nil {
		return n.PublicURL()
	}
	return ""
}
