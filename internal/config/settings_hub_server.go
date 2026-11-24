package config

import (
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
)

// Names of the Hub server settings, for callers that look them up by key.
const (
	KeyListen         = "listen"
	KeyTLS            = "tls"
	KeyPublicURL      = "public_url"
	KeyTrustedProxies = "trusted_proxies"
	// KeyManagementListen is the optional loopback-only management listener
	// (docs/architecture/proposals/hub-identity-v2.md, section 8.1 item 5).
	KeyManagementListen = "management_listen"

	// KeyTLSCertFile and KeyTLSKeyFile name the certificate and key file
	// settings. Their environment names are MINION_TLS_CERT_FILE and
	// MINION_TLS_KEY_FILE (EnvTLSCertFile, EnvTLSKeyFile). They are not
	// declared in the registry: the registry reserves the "_FILE" suffix for
	// the path variable of a secret (see Setting.FileEnvName), and these two
	// hold file paths themselves. [ResolveHubServer] reads them directly
	// (environment, then the panel key). Until the registry can declare a
	// non-secret path setting that ends in _FILE, they have no panel field.
	KeyTLSCertFile = "tls_cert_file"
	KeyTLSKeyFile  = "tls_key_file"

	EnvTLSCertFile = "MINION_TLS_CERT_FILE"
	EnvTLSKeyFile  = "MINION_TLS_KEY_FILE"
)

// TLS modes of the Hub listener (AD-18).
const (
	// TLSAuto serves HTTPS: with the configured certificate, or with a
	// self-signed certificate that the Hub generates on first start.
	TLSAuto = "auto"
	// TLSOff serves plain HTTP, for a Hub behind a TLS-terminating proxy. It
	// requires MINION_PUBLIC_URL.
	TLSOff = "off"
)

// DefaultListen is the default listen setting of the Hub: a plain port, which
// means every interface ([ListenAddr] turns it into ":8443").
const DefaultListen = "8443"

// ParsePort parses a plain port number, the canonical form of a listen
// setting: digits only (no sign, no space, at most 16 of them), leading zeros
// allowed as the Go listener allows them, and a value from 0 to 65535.
func ParsePort(v string) (int, bool) {
	if !isPort(v) {
		return 0, false
	}
	n := 0
	for _, c := range v {
		n = n*10 + int(c-'0')
		if n > 65535 {
			return 0, false
		}
	}
	return n, true
}

// isPort reports whether v has the form of a plain port number (digits only);
// [ParsePort] checks the range.
func isPort(v string) bool {
	if v == "" || len(v) > 16 {
		return false
	}
	for _, c := range v {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// HostPortPort returns the port of a host:port listen address as the Hub
// accepted it before plain ports (v0.20.4 and earlier) and as the Go listener
// binds it: the port after the last colon read as a decimal number, an
// optional sign and any number of leading zeros included ("127.0.0.1:+9443"
// and "127.0.0.1:0009443" bind 9443), from 0 to 65535. Every value an older
// Hub started with keeps working.
func HostPortPort(addr string) (int, bool) {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return 0, false
	}
	p, err := strconv.Atoi(port)
	if err != nil || p < 0 || p > 65535 {
		return 0, false
	}
	return p, true
}

// ListenAddr turns a listen setting into a Go listen address. A plain port
// number such as "8443" (the canonical form) means every interface, ":8443";
// the older forms ":8443" and "host:port" are kept as they are.
func ListenAddr(v string) string {
	v = strings.TrimSpace(v)
	if isPort(v) {
		return ":" + v
	}
	return v
}

// ManagementListenAddr turns a management_listen setting into a listen
// address. A plain port number such as "8444" means the loopback address
// "127.0.0.1:8444" (the management listener must be loopback); a
// "host:port" value is kept as it is.
func ManagementListenAddr(v string) string {
	v = strings.TrimSpace(v)
	if isPort(v) {
		return "127.0.0.1:" + v
	}
	return v
}

// ListenPort returns the port of a listen setting in any of its forms
// ("8443", ":8443", "host:8443", a padded ":0009443" or a signed
// "host:+9443" of an older Hub), as the listener binds it ("9443"), or def
// when it names none (the setting is absent or not a port). An explicit port
// 0 is returned as "0": the listener binds an ephemeral port, which is not
// the default one, and a caller that must reach the Hub has to say so
// ([EphemeralPort]) instead of probing the default port.
func ListenPort(v, def string) string {
	v = strings.TrimSpace(v)
	p, ok := ParsePort(v)
	if !ok {
		p, ok = HostPortPort(v)
	}
	if ok {
		return strconv.Itoa(p)
	}
	return def
}

// EphemeralPort is what [ListenPort] returns for a listen setting that binds
// an ephemeral port (port 0).
const EphemeralPort = "0"

func init() { declareHubServer(Default) }

// declareHubServer declares the settings of the Hub's HTTP server.
func declareHubServer(r *Registry) {
	r.Declare(Setting{
		Key:     KeyListen,
		Scope:   ScopeHub,
		Help:    "Port the Hub listens on, for example 8443 (every interface); host:port such as 127.0.0.1:8443 limits it to one address",
		Default: DefaultListen,
	})
	r.Declare(Setting{
		Key:     KeyTLS,
		Scope:   ScopeHub,
		Help:    "TLS mode: auto (HTTPS, self-signed certificate unless one is configured) or off (plain HTTP behind a proxy; needs public_url)",
		Default: TLSAuto,
		Choices: []string{TLSAuto, TLSOff},
	})
	r.Declare(Setting{
		Key:   KeyPublicURL,
		Scope: ScopeHub,
		Help:  "External URL of the Hub, for example https://hub.example.com; required when tls is off",
	})
	r.Declare(Setting{
		Key:   KeyTrustedProxies,
		Scope: ScopeHub,
		Help:  "Comma-separated CIDRs (or addresses) of reverse proxies whose X-Forwarded-For and X-Forwarded-Proto headers are honoured",
	})
	r.Declare(Setting{
		Key:   KeyManagementListen,
		Scope: ScopeHub,
		Help:  "Optional management listener: a port such as 8444 (on 127.0.0.1) or a loopback host:port, served as plain HTTP. It is the only place where a passphrase is accepted without HTTPS; never point a reverse proxy at it",
	})
}

// HubServer holds the raw Hub server settings. [ResolveHubServer] fills it;
// the server package validates and parses the values.
type HubServer struct {
	DataDir        string
	Listen         string
	TLS            string // TLSAuto or TLSOff
	CertFile       string
	KeyFile        string
	PublicURL      string
	TrustedProxies string // comma-separated CIDRs
	// ManagementListen is the loopback address of the management listener,
	// empty when there is none.
	ManagementListen string
}

// ResolveHubServer resolves the settings of the Hub's HTTP server through r
// (a Hub-role resolver). The certificate and key file paths come from
// MINION_TLS_CERT_FILE and MINION_TLS_KEY_FILE, read through r.LookupEnv, and
// otherwise from the panel keys tls_cert_file and tls_key_file when r has a
// Panel (environment before panel, as for every Hub setting). Setting only one
// of the two is an error.
func ResolveHubServer(r *Resolver) (HubServer, error) {
	if r.Role != ScopeHub {
		return HubServer{}, fmt.Errorf("config: ResolveHubServer needs a %q resolver, got %q", ScopeHub, r.Role)
	}
	var hs HubServer
	for _, f := range []struct {
		key string
		dst *string
	}{
		{KeyDataDir, &hs.DataDir},
		{KeyListen, &hs.Listen},
		{KeyTLS, &hs.TLS},
		{KeyPublicURL, &hs.PublicURL},
		{KeyTrustedProxies, &hs.TrustedProxies},
		{KeyManagementListen, &hs.ManagementListen},
	} {
		res, err := r.Resolve(f.key)
		if err != nil {
			return HubServer{}, err
		}
		*f.dst = strings.TrimSpace(res.Value)
	}
	var err error
	if hs.CertFile, err = r.pathSetting(EnvTLSCertFile, KeyTLSCertFile); err != nil {
		return HubServer{}, err
	}
	if hs.KeyFile, err = r.pathSetting(EnvTLSKeyFile, KeyTLSKeyFile); err != nil {
		return HubServer{}, err
	}
	if (hs.CertFile == "") != (hs.KeyFile == "") {
		return HubServer{}, errors.New("set both " + EnvTLSCertFile + " and " + EnvTLSKeyFile + ", or neither")
	}
	return hs, nil
}

// pathSetting resolves a setting that is not in the registry: the environment
// variable wins, then the panel value for key, otherwise "".
func (r *Resolver) pathSetting(env, key string) (string, error) {
	if v, ok := r.lookupEnv(env); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v), nil
	}
	if r.Panel == nil {
		return "", nil
	}
	v, ok, err := r.Panel.Lookup(key)
	if err != nil {
		return "", fmt.Errorf("config: read %s from the panel: %w", key, err)
	}
	if !ok {
		return "", nil
	}
	return strings.TrimSpace(v), nil
}
