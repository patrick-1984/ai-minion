package config

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/url"
	"sort"
	"strings"
)

// ComplianceConfig is the validated configuration of the compliance export:
// one value, built and swapped as a whole. It never falls back to the
// ordinary OpenTelemetry settings: the compliance collector has its own
// endpoint, credentials and trust.
type ComplianceConfig struct {
	// Level is off, actions or output; Input says that the content that was
	// accepted for sending is recorded too.
	Level string
	Input bool
	// LogsURL is the exact URL of the OTLP/HTTP logs endpoint.
	LogsURL string
	// Headers are sent with every request. Never log them.
	Headers    map[string]string
	RootCAs    *x509.CertPool
	ClientCert *tls.Certificate
	// Digest identifies every input (headers and files included).
	Digest string
	// Env is true when any compliance key comes from the environment.
	Env bool
}

// Active reports whether anything is recorded.
func (c ComplianceConfig) Active() bool { return c.Level != "" && c.Level != ComplianceOff }

// Destination is the endpoint as a status or an audit row may show it:
// scheme://host:port, never a path, credential or query.
func (c ComplianceConfig) Destination() string {
	return DestinationLabel(c.LogsURL)
}

// DestinationLabel reduces an endpoint URL to scheme://host:port.
func DestinationLabel(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return ""
	}
	host := u.Host
	if u.Port() == "" {
		if u.Scheme == "https" {
			host += ":443"
		} else {
			host += ":80"
		}
	}
	return u.Scheme + "://" + host
}

// TLSConfig returns the client TLS configuration of the exporter.
func (c ComplianceConfig) TLSConfig() *tls.Config {
	host := ""
	if u, err := url.Parse(c.LogsURL); err == nil {
		host = u.Hostname()
	}
	return clientTLS(c.RootCAs, c.ClientCert, host)
}

// ResolveCompliance resolves and validates the compliance settings of the Hub
// as a whole (design §5.4). Nothing but the level and the input switch is
// looked at while monitoring is off. Errors name keys and line numbers, never
// values.
func ResolveCompliance(res *Resolver, files SinkFiles) (ComplianceConfig, error) {
	if files.ReadPrivate == nil {
		files.ReadPrivate = readPrivateDefault
	}
	var c ComplianceConfig
	lvl, err := res.Resolve(KeyComplianceMonitoring)
	if err != nil {
		return c, err
	}
	in, err := res.Resolve(KeyComplianceInput)
	if err != nil {
		return c, err
	}
	for _, k := range ComplianceKeys {
		if r, err := res.Resolve(k); err == nil && r.Source == SourceEnv {
			c.Env = true
		}
	}
	c.Level = lvl.Value
	c.Input = in.Value == SinkOn
	if c.Level == ComplianceOff {
		if c.Input {
			return c, fmt.Errorf("setting %q is on but %q is off: input is recorded only together with actions or output", KeyComplianceInput, KeyComplianceMonitoring)
		}
		return c, nil
	}
	ep, err := res.Resolve(KeyComplianceEndpoint)
	if err != nil {
		return c, err
	}
	base := strings.TrimSpace(ep.Value)
	if base == "" {
		return c, fmt.Errorf("compliance monitoring is on but %q is not set", KeyComplianceEndpoint)
	}
	clean, err := parseHTTPURL(base)
	if err != nil {
		return c, fmt.Errorf("setting %q: %v", KeyComplianceEndpoint, err)
	}
	u, _ := url.Parse(clean)
	if u.Scheme != "https" && !isLoopbackHost(u.Hostname()) {
		return c, fmt.Errorf("setting %q: use https://, or http:// to a loopback address (a collector on this host); the record holds session content", KeyComplianceEndpoint)
	}
	if err := checkHostPort(u); err != nil {
		return c, fmt.Errorf("setting %q: %v", KeyComplianceEndpoint, err)
	}
	c.LogsURL = strings.TrimRight(clean, "/") + "/v1/logs"

	d := newDigest("compliance", c.Level, in.Value, c.LogsURL)
	hr, err := res.Resolve(KeyComplianceHeaders)
	if err != nil {
		return c, err
	}
	if c.Headers, err = ParseHeaderLines(hr.Value); err != nil {
		return c, fmt.Errorf("setting %q: %v", KeyComplianceHeaders, err)
	}
	names := make([]string, 0, len(c.Headers))
	for k := range c.Headers {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		d.add(k, c.Headers[k])
	}
	isTLS := u.Scheme == "https"
	c.RootCAs, c.ClientCert, err = resolveTLSFiles(res, files, d, isTLS, "an https:// endpoint",
		KeyComplianceCAFile, KeyComplianceClientCertFile, KeyComplianceClientKeyFile)
	if err != nil {
		return c, err
	}
	if !isLoopbackHost(u.Hostname()) && len(c.Headers) == 0 && c.ClientCert == nil {
		return c, fmt.Errorf("setting %q is not a loopback address, so %q or a client certificate (%q, %q) is required",
			KeyComplianceEndpoint, KeyComplianceHeaders, KeyComplianceClientCertFile, KeyComplianceClientKeyFile)
	}
	if err := credentialsFollowEndpoint(res, ep, KeyComplianceEndpoint, KeyComplianceHeaders, KeyComplianceClientKeyFile); err != nil {
		return c, err
	}
	c.Digest = d.sum()
	return c, nil
}
