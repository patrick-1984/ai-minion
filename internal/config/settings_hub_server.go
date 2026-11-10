package config

import (
	"errors"
	"fmt"
	"strings"
)

// Names of the Hub server settings, for callers that look them up by key.
const (
	KeyListen         = "listen"
	KeyTLS            = "tls"
	KeyPublicURL      = "public_url"
	KeyTrustedProxies = "trusted_proxies"

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

// DefaultListen is the default listen address of the Hub.
const DefaultListen = ":8443"

func init() { declareHubServer(Default) }

// declareHubServer declares the settings of the Hub's HTTP server.
func declareHubServer(r *Registry) {
	r.Declare(Setting{
		Key:     KeyListen,
		Scope:   ScopeHub,
		Help:    "Address the Hub listens on, as host:port",
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
