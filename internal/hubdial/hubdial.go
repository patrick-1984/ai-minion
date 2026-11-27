// Package hubdial is the Android app's Hub address override: a hostname
// that this network's DNS cannot resolve is dialled at a fixed IP address.
//
// Only the dial target changes. The URL, the TLS server name (SNI and the
// certificate name check) and the HTTP Host header stay the hostname, so a
// certificate for the hostname is still required wherever it is checked.
//
// The override is read from [EnvName] ("hostname=address"), which only the
// Android app sets. It takes effect only in a GOOS=android build
// (dial_android.go); every other build compiles [Dial] to the identity
// (dial_other.go), so it behaves exactly as before.
package hubdial

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strings"
)

// EnvName is the environment variable the Android app sets on every native
// process it starts, as "hostname=address", for example
// "minion.example=192.0.2.10".
const EnvName = "MINION_HUB_DIAL_ADDRESS"

// DialFunc is the signature of net.Dialer.DialContext and
// http.Transport.DialContext.
type DialFunc func(ctx context.Context, network, addr string) (net.Conn, error)

// Override dials Host at Addr.
type Override struct {
	// Host is the lower-case hostname, without a trailing dot.
	Host string
	// Addr is the IPv4 or IPv6 address dialled instead of Host.
	Addr netip.Addr
}

// Parse reads "hostname=address". An empty value is no override (nil, nil).
func Parse(value string) (*Override, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, nil
	}
	host, addr, ok := strings.Cut(value, "=")
	if !ok {
		return nil, fmt.Errorf("%s must be hostname=address, such as hub.example=10.0.0.5", EnvName)
	}
	host = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	if err := checkHostname(host); err != nil {
		return nil, fmt.Errorf("%s: %w", EnvName, err)
	}
	ip, err := netip.ParseAddr(strings.Trim(strings.TrimSpace(addr), "[]"))
	if err != nil || ip.Zone() != "" {
		return nil, fmt.Errorf("%s: %q is not an IPv4 or IPv6 address", EnvName, strings.TrimSpace(addr))
	}
	return &Override{Host: host, Addr: ip.Unmap()}, nil
}

// String is the value Parse reads back.
func (o *Override) String() string { return o.Host + "=" + o.Addr.String() }

// Target returns the address to dial for addr ("host:port"): the override's
// address with the same port when the host is the override's hostname, else
// addr unchanged.
func (o *Override) Target(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil || !strings.EqualFold(strings.TrimSuffix(host, "."), o.Host) {
		return addr
	}
	return net.JoinHostPort(o.Addr.String(), port)
}

// Wrap returns a dial function that dials the override's address for the
// override's hostname and passes every other address to next unchanged. A
// nil next uses a zero net.Dialer, as http.Transport does.
func (o *Override) Wrap(next DialFunc) DialFunc {
	if next == nil {
		next = (&net.Dialer{}).DialContext
	}
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		return next(ctx, network, o.Target(addr))
	}
}

// dialWith is Dial for a parsed override: next unchanged without one, a
// dial that fails with the reason for an invalid one.
func dialWith(o *Override, err error, next DialFunc) DialFunc {
	switch {
	case err != nil:
		return failing(err)
	case o == nil:
		return next
	}
	return o.Wrap(next)
}

// failing reports a bad override on every dial instead of ignoring it, so
// the Worker shows the real cause rather than a DNS error.
func failing(err error) DialFunc {
	return func(context.Context, string, string) (net.Conn, error) { return nil, err }
}

func checkHostname(host string) error {
	if host == "" {
		return errors.New("the hostname is empty")
	}
	if _, err := netip.ParseAddr(host); err == nil {
		return fmt.Errorf("%q is already an address; the override needs the Hub's hostname", host)
	}
	if len(host) > 253 {
		return errors.New("the hostname is longer than 253 characters")
	}
	labels := strings.Split(host, ".")
	for _, l := range labels {
		if l == "" || len(l) > 63 || l[0] == '-' || l[len(l)-1] == '-' {
			return fmt.Errorf("%q is not a valid hostname", host)
		}
		for i := 0; i < len(l); i++ {
			c := l[i]
			if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
				return fmt.Errorf("%q is not a valid hostname", host)
			}
		}
	}
	last := labels[len(labels)-1]
	if strings.Trim(last, "0123456789") == "" {
		return fmt.Errorf("%q is not a valid hostname", host)
	}
	return nil
}
