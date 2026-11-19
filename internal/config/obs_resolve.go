package config

import (
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/textproto"
	"net/url"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
)

// Environment variables of the OpenTelemetry specification that Minion reads
// itself (§3.4): the endpoints are honoured as aliases of otel_endpoint, the
// header variables are ignored with a warning (headers are secrets and come
// from otel_headers or otel_headers_path).
const (
	EnvOTLPEndpoint        = "OTEL_EXPORTER_OTLP_ENDPOINT"
	EnvOTLPLogsEndpoint    = "OTEL_EXPORTER_OTLP_LOGS_ENDPOINT"
	EnvOTLPMetricsEndpoint = "OTEL_EXPORTER_OTLP_METRICS_ENDPOINT"
)

// IgnoredOTLPHeaderEnv are the header variables of the OpenTelemetry
// specification. Minion never reads them: an exporter that parses them can
// echo a bearer token into an error message.
var IgnoredOTLPHeaderEnv = []string{
	"OTEL_EXPORTER_OTLP_HEADERS", "OTEL_EXPORTER_OTLP_LOGS_HEADERS", "OTEL_EXPORTER_OTLP_METRICS_HEADERS",
}

// Limits of a header list.
const (
	maxHeaderLines    = 64
	maxHeaderFileSize = 64 << 10
	maxPEMFileSize    = 1 << 20
)

// SyslogConfig is the validated configuration of the syslog sink: one value,
// built and swapped as a whole.
type SyslogConfig struct {
	Enabled bool
	// Network is udp, tcp or tls.
	Network string
	// Addr is host:port.
	Addr string
	// Level is debug, info, warn or error.
	Level    string
	MaxBytes int
	// RootCAs is nil for the system roots. ClientCert is nil without mutual
	// TLS. Both are set for Network tls only.
	RootCAs    *x509.CertPool
	ClientCert *tls.Certificate
	// ServerName is the host of the endpoint, for certificate checks.
	ServerName string
	// Digest identifies the inputs (files included); equal digests mean an
	// equal configuration.
	Digest string
}

// Destination is the endpoint as status lines may show it.
func (c SyslogConfig) Destination() string {
	if !c.Enabled {
		return ""
	}
	return c.Network + "://" + c.Addr
}

// TLSConfig returns the client TLS configuration of a tls:// endpoint.
func (c SyslogConfig) TLSConfig() *tls.Config {
	return clientTLS(c.RootCAs, c.ClientCert, c.ServerName)
}

// OTLPConfig is the validated configuration of the OpenTelemetry log sink.
type OTLPConfig struct {
	Enabled bool
	// LogsURL and MetricsURL are the exact per-signal URLs.
	LogsURL, MetricsURL string
	// Headers are sent with every request. Never log them.
	Headers map[string]string
	Level   string
	// RootCAs is nil for the system roots; ClientCert is nil without mutual
	// TLS.
	RootCAs    *x509.CertPool
	ClientCert *tls.Certificate
	// Digest identifies the inputs (headers and files included).
	Digest string
}

// MetricsTLSConfig is TLSConfig for the metrics URL, which may name another
// host (OTEL_EXPORTER_OTLP_METRICS_ENDPOINT).
func (c OTLPConfig) MetricsTLSConfig() *tls.Config {
	host := ""
	if u, err := url.Parse(c.MetricsURL); err == nil {
		host = u.Hostname()
	}
	return clientTLS(c.RootCAs, c.ClientCert, host)
}

// TLSConfig returns the client TLS configuration of the exporter.
func (c OTLPConfig) TLSConfig() *tls.Config {
	host := ""
	if u, err := url.Parse(c.LogsURL); err == nil {
		host = u.Hostname()
	}
	return clientTLS(c.RootCAs, c.ClientCert, host)
}

func clientTLS(roots *x509.CertPool, cert *tls.Certificate, server string) *tls.Config {
	cfg := &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots, ServerName: server}
	if cert != nil {
		cfg.Certificates = []tls.Certificate{*cert}
	}
	return cfg
}

// Sinks is the resolved configuration of both log sinks.
type Sinks struct {
	Syslog SyslogConfig
	OTLP   OTLPConfig
	// Warnings are for the local log; they name settings and variables,
	// never values.
	Warnings []string
}

// SinkFiles reads the files the sink settings name. ReadPrivate reads a file
// that holds a secret (a TLS key, the Worker's header file) and must refuse a
// file that other users can read; nil uses a mode check on Unix. Callers pass
// identity.OpenPrivate-based readers, which also check the owner and, on
// Windows, the ACL.
type SinkFiles struct {
	ReadPrivate func(path string) ([]byte, error)
}

// ResolveSinks resolves and validates the syslog and OpenTelemetry sink
// settings of res's role as a whole (§3.2, §3.4). Settings of a sink that is
// off are not checked. Errors name the key, the variable or a line number,
// never a value.
func ResolveSinks(res *Resolver, files SinkFiles) (Sinks, error) {
	if files.ReadPrivate == nil {
		files.ReadPrivate = readPrivateDefault
	}
	var out Sinks
	var err error
	if out.Syslog, err = resolveSyslog(res, files); err != nil {
		return Sinks{}, err
	}
	if out.OTLP, out.Warnings, err = resolveOTLP(res, files); err != nil {
		return Sinks{}, err
	}
	return out, nil
}

func resolveSyslog(res *Resolver, files SinkFiles) (SyslogConfig, error) {
	var c SyslogConfig
	on, err := res.Resolve(KeySyslog)
	if err != nil {
		return c, err
	}
	if on.Value != SinkOn {
		return c, nil
	}
	c.Enabled = true
	ep, err := res.Resolve(KeySyslogEndpoint)
	if err != nil {
		return c, err
	}
	if strings.TrimSpace(ep.Value) == "" {
		return c, fmt.Errorf("syslog is on but %s is not set", KeySyslogEndpoint)
	}
	u, err := url.Parse(strings.TrimSpace(ep.Value))
	if err != nil || u.Opaque != "" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return c, fmt.Errorf("setting %q: expected udp://host:port, tcp://host:port or tls://host:port", KeySyslogEndpoint)
	}
	switch u.Scheme {
	case "udp", "tcp", "tls":
	default:
		return c, fmt.Errorf("setting %q: the scheme must be udp, tcp or tls", KeySyslogEndpoint)
	}
	if err := checkHostPort(u); err != nil {
		return c, fmt.Errorf("setting %q: %v", KeySyslogEndpoint, err)
	}
	c.Network, c.Addr, c.ServerName = u.Scheme, u.Host, u.Hostname()
	lvl, err := res.Resolve(KeySyslogLevel)
	if err != nil {
		return c, err
	}
	c.Level = lvl.Value
	mb, err := res.Resolve(KeySyslogMaxBytes)
	if err != nil {
		return c, err
	}
	c.MaxBytes, err = strconv.Atoi(strings.TrimSpace(mb.Value))
	if err != nil || c.MaxBytes < SyslogMinBytes || c.MaxBytes > SyslogMaxBytes {
		return c, fmt.Errorf("setting %q: must be a number from %d to %d", KeySyslogMaxBytes, SyslogMinBytes, SyslogMaxBytes)
	}
	d := newDigest("syslog", c.Network, c.Addr, c.Level, strconv.Itoa(c.MaxBytes))
	c.RootCAs, c.ClientCert, err = resolveTLSFiles(res, files, d, c.Network == "tls", "a tls:// endpoint",
		KeySyslogCAFile, KeySyslogClientCertFile, KeySyslogClientKeyFile)
	if err != nil {
		return c, err
	}
	if err := credentialsFollowEndpoint(res, ep, KeySyslogEndpoint, KeySyslogClientKeyFile); err != nil {
		return c, err
	}
	c.Digest = d.sum()
	return c, nil
}

func resolveOTLP(res *Resolver, files SinkFiles) (OTLPConfig, []string, error) {
	var c OTLPConfig
	var warn []string
	on, err := res.Resolve(KeyOTel)
	if err != nil {
		return c, nil, err
	}
	if on.Value != SinkOn {
		return c, nil, nil
	}
	c.Enabled = true
	for _, name := range IgnoredOTLPHeaderEnv {
		if v, ok := res.lookupEnv(name); ok && v != "" {
			warn = append(warn, fmt.Sprintf("%s is ignored: Minion reads OTLP headers only from %s", name, headerSettingOf(res.Role)))
		}
	}
	base, err := res.Resolve(KeyOTelEndpoint)
	if err != nil {
		return c, nil, err
	}
	c.LogsURL, err = otlpSignalURL(res, strings.TrimSpace(base.Value), EnvOTLPLogsEndpoint, "/v1/logs")
	if err != nil {
		return c, nil, err
	}
	if c.LogsURL == "" {
		return c, nil, fmt.Errorf("otel is on but has no endpoint: set %s (or %s)", KeyOTelEndpoint, EnvOTLPEndpoint)
	}
	// The metrics URL is resolved now so that a bad alias fails early; it
	// is used once metrics export exists.
	if c.MetricsURL, err = otlpSignalURL(res, strings.TrimSpace(base.Value), EnvOTLPMetricsEndpoint, "/v1/metrics"); err != nil {
		return c, nil, err
	}
	if u, _ := url.Parse(c.LogsURL); u != nil && u.Scheme == "http" && !isLoopbackHost(u.Hostname()) {
		warn = append(warn, "the OpenTelemetry endpoint uses http:// to a host that is not loopback: records and headers travel unencrypted")
	} else if u, _ := url.Parse(c.MetricsURL); u != nil && u.Scheme == "http" && !isLoopbackHost(u.Hostname()) {
		warn = append(warn, "the OpenTelemetry metrics endpoint uses http:// to a host that is not loopback: metrics and headers travel unencrypted")
	}
	lvl, err := res.Resolve(KeyOTelLevel)
	if err != nil {
		return c, nil, err
	}
	c.Level = lvl.Value
	d := newDigest("otlp", c.LogsURL, c.MetricsURL, c.Level)
	if c.Headers, err = resolveHeaders(res, files, d); err != nil {
		return c, nil, err
	}
	// Trust is needed when either signal goes to an https:// endpoint; each
	// signal's client applies it only to its own https:// URL.
	isTLS := strings.HasPrefix(c.LogsURL, "https://") || strings.HasPrefix(c.MetricsURL, "https://")
	c.RootCAs, c.ClientCert, err = resolveTLSFiles(res, files, d, isTLS, "an https:// endpoint",
		KeyOTelCAFile, KeyOTelClientCertFile, KeyOTelClientKeyFile)
	if err != nil {
		return c, nil, err
	}
	if err := credentialsFollowEndpoint(res, base, KeyOTelEndpoint, KeyOTelHeaders, KeyOTelClientKeyFile); err != nil {
		return c, nil, err
	}
	c.Digest = d.sum()
	return c, warn, nil
}

// credentialsFollowEndpoint refuses an endpoint from the Hub panel while a
// credential of the same sink (headers, client key) comes from the
// environment: the deployer set the credential for a destination, and a
// panel edit of the endpoint alone must not send it to another one.
func credentialsFollowEndpoint(res *Resolver, endpoint Resolved, endpointKey string, credKeys ...string) error {
	if res.Role != ScopeHub || endpoint.Source != SourcePanel || strings.TrimSpace(endpoint.Value) == "" {
		return nil
	}
	for _, k := range credKeys {
		if _, ok := res.registry().Lookup(k); !ok {
			continue
		}
		r, err := res.Resolve(k)
		if err != nil {
			return err
		}
		if r.Source == SourceEnv && r.Value != "" {
			return fmt.Errorf("setting %q comes from the environment, so %q must come from the environment too (set %s); the panel cannot send that credential to another destination",
				k, endpointKey, "MINION_"+strings.ToUpper(endpointKey))
		}
	}
	return nil
}

func headerSettingOf(role Scope) string {
	if role == ScopeWorker {
		return KeyOTelHeadersPath
	}
	return KeyOTelHeaders
}

// otlpSignalURL returns the exact URL of one signal: otel_endpoint plus the
// signal path, else the per-signal variable as is, else the general variable
// plus the signal path; "" when none is set.
func otlpSignalURL(res *Resolver, base, signalEnv, path string) (string, error) {
	join := func(u string) string { return strings.TrimRight(u, "/") + path }
	if base != "" {
		u, err := parseHTTPURL(base)
		if err != nil {
			return "", fmt.Errorf("setting %q: %v", KeyOTelEndpoint, err)
		}
		return join(u), nil
	}
	if v, ok := res.lookupEnv(signalEnv); ok && strings.TrimSpace(v) != "" {
		u, err := parseHTTPURL(strings.TrimSpace(v))
		if err != nil {
			return "", fmt.Errorf("%s: %v", signalEnv, err)
		}
		return u, nil
	}
	if v, ok := res.lookupEnv(EnvOTLPEndpoint); ok && strings.TrimSpace(v) != "" {
		u, err := parseHTTPURL(strings.TrimSpace(v))
		if err != nil {
			return "", fmt.Errorf("%s: %v", EnvOTLPEndpoint, err)
		}
		return join(u), nil
	}
	return "", nil
}

// parseHTTPURL accepts an http or https URL with a host, without user
// information, query, fragment (not even an empty "?" or "#") or
// percent-encoded path characters (the exporter sends the decoded path), and
// returns it rebuilt from its parsed parts (lower-case scheme). The messages
// never quote the value.
func parseHTTPURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", errors.New("not a valid URL")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", errors.New("the scheme must be http or https")
	}
	if u.Host == "" || u.Hostname() == "" {
		return "", errors.New("the URL has no host")
	}
	if u.User != nil {
		return "", errors.New("the URL must not contain a user name or password; use headers")
	}
	if strings.ContainsAny(raw, "?#") || u.Opaque != "" {
		return "", errors.New("the URL must not have a query or fragment")
	}
	if strings.Contains(u.EscapedPath(), "%") || u.RawPath != "" {
		return "", errors.New("the URL path must not contain percent-encoded characters")
	}
	if p := u.Port(); p != "" {
		if n, err := strconv.Atoi(p); err != nil || n < 1 || n > 65535 {
			return "", errors.New("the port is invalid")
		}
	}
	// Serialised by net/url, so that an IPv6 zone keeps its escape
	// (http://[fe80::1%25eth0]:4318): a raw "%" would make the URL
	// unparsable for the exporter.
	return (&url.URL{Scheme: u.Scheme, Host: u.Host, Path: u.Path}).String(), nil
}

func checkHostPort(u *url.URL) error {
	if u.Hostname() == "" {
		return errors.New("the endpoint has no host")
	}
	p := u.Port()
	if p == "" {
		return errors.New("the endpoint has no port")
	}
	if n, err := strconv.Atoi(p); err != nil || n < 1 || n > 65535 {
		return errors.New("the port is invalid")
	}
	return nil
}

func isLoopbackHost(h string) bool {
	if strings.EqualFold(h, "localhost") {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// resolveHeaders reads the header list: the Hub's secret otel_headers, the
// Worker's file named by otel_headers_path.
func resolveHeaders(res *Resolver, files SinkFiles, d *digest) (map[string]string, error) {
	var text, key string
	if res.Role == ScopeWorker {
		key = KeyOTelHeadersPath
		p, err := res.Resolve(KeyOTelHeadersPath)
		if err != nil {
			return nil, err
		}
		if strings.TrimSpace(p.Value) == "" {
			return map[string]string{}, nil
		}
		data, err := files.ReadPrivate(strings.TrimSpace(p.Value))
		if err != nil {
			// The path is not quoted: a mistake can put the header text
			// itself into the setting.
			return nil, fmt.Errorf("setting %q: %s", KeyOTelHeadersPath, fileProblem(err))
		}
		if len(data) > maxHeaderFileSize {
			return nil, fmt.Errorf("setting %q: the file is larger than %d bytes", KeyOTelHeadersPath, maxHeaderFileSize)
		}
		text = string(data)
	} else {
		key = KeyOTelHeaders
		r, err := res.Resolve(KeyOTelHeaders)
		if err != nil {
			return nil, err
		}
		text = r.Value
	}
	h, err := ParseHeaderLines(text)
	if err != nil {
		return nil, fmt.Errorf("setting %q: %v", key, err)
	}
	names := make([]string, 0, len(h))
	for k := range h {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		d.add(k, h[k])
	}
	return h, nil
}

// ParseHeaderLines parses "Name: value" lines (the HTTP form). Blank lines
// and lines that start with "#" are skipped. A name must be an HTTP token, a
// value must not contain control characters. Errors give the line number
// only, never the content.
func ParseHeaderLines(text string) (map[string]string, error) {
	out := map[string]string{}
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	n := 0
	for i, line := range lines {
		t := strings.TrimSpace(line)
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		n++
		if n > maxHeaderLines {
			return nil, fmt.Errorf("more than %d headers", maxHeaderLines)
		}
		name, value, ok := strings.Cut(t, ":")
		name, value = strings.TrimSpace(name), strings.TrimSpace(value)
		if !ok || name == "" {
			return nil, fmt.Errorf("line %d: expected \"Name: value\"", i+1)
		}
		if !isToken(name) {
			return nil, fmt.Errorf("line %d: the header name is not a valid HTTP token", i+1)
		}
		for _, r := range value {
			if r < 0x20 && r != '\t' || r == 0x7f {
				return nil, fmt.Errorf("line %d: the header value contains a control character", i+1)
			}
		}
		name = textproto.CanonicalMIMEHeaderKey(name)
		if _, dup := out[name]; dup {
			return nil, fmt.Errorf("line %d: the header is already set on an earlier line (names are case-insensitive)", i+1)
		}
		out[name] = value
	}
	return out, nil
}

func isToken(s string) bool {
	for _, r := range s {
		if r > 0x7e || r <= 0x20 || strings.ContainsRune("\"(),/:;<=>?@[\\]{}", r) {
			return false
		}
	}
	return s != ""
}

// resolveTLSFiles loads the CA file and the client certificate and key of a
// sink. They are allowed only when the endpoint uses TLS (what names it).
func resolveTLSFiles(res *Resolver, files SinkFiles, d *digest, isTLS bool, what, caKey, certKey, keyKey string) (*x509.CertPool, *tls.Certificate, error) {
	path := func(k string) (string, error) {
		r, err := res.Resolve(k)
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(r.Value), nil
	}
	ca, err := path(caKey)
	if err != nil {
		return nil, nil, err
	}
	certPath, err := path(certKey)
	if err != nil {
		return nil, nil, err
	}
	keyPath, err := path(keyKey)
	if err != nil {
		return nil, nil, err
	}
	if !isTLS {
		for _, k := range [][2]string{{caKey, ca}, {certKey, certPath}, {keyKey, keyPath}} {
			if k[1] != "" {
				return nil, nil, fmt.Errorf("setting %q needs %s", k[0], what)
			}
		}
		return nil, nil, nil
	}
	var roots *x509.CertPool
	if ca != "" {
		data, err := readBounded(ca, maxPEMFileSize)
		if err != nil {
			return nil, nil, fmt.Errorf("setting %q: %v", caKey, err)
		}
		roots = x509.NewCertPool()
		if !roots.AppendCertsFromPEM(data) {
			return nil, nil, fmt.Errorf("setting %q: the file holds no PEM certificate", caKey)
		}
		d.add("ca", string(data))
	}
	if (certPath == "") != (keyPath == "") {
		return nil, nil, fmt.Errorf("set both %s and %s, or neither", certKey, keyKey)
	}
	if certPath == "" {
		return roots, nil, nil
	}
	certPEM, err := readBounded(certPath, maxPEMFileSize)
	if err != nil {
		return nil, nil, fmt.Errorf("setting %q: %v", certKey, err)
	}
	keyPEM, err := files.ReadPrivate(keyPath)
	if err != nil {
		// The path is not quoted: a mistake can put the key itself there.
		return nil, nil, fmt.Errorf("setting %q: %s", keyKey, fileProblem(err))
	}
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, nil, fmt.Errorf("settings %q and %q: not a matching PEM certificate and key", certKey, keyKey)
	}
	d.add("cert", string(certPEM))
	d.add("key", string(keyPEM))
	return roots, &pair, nil
}

// fileProblem describes a file error without its path.
func fileProblem(err error) string {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "the file does not exist"
	case errors.Is(err, fs.ErrPermission):
		return "the file cannot be read (permission denied)"
	}
	return "the file is not a regular file of this user that only its owner can read (mode 0600), or it cannot be read"
}

// secretFileProblem describes the error of a secret file without its path.
func secretFileProblem(err error) string {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "the file does not exist"
	case errors.Is(err, fs.ErrPermission):
		return "permission denied"
	case errors.Is(err, errNotRegular):
		return "not a regular file"
	}
	return "the file cannot be read"
}

// readBounded reads a regular file of at most max bytes. The error names the
// path, never the content.
func readBounded(path string, max int64) ([]byte, error) {
	f, err := openRegular(path)
	if err != nil {
		var pe *os.PathError
		if errors.As(err, &pe) {
			return nil, fmt.Errorf("cannot read %s: %v", path, pe.Err)
		}
		return nil, err
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	data, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, fmt.Errorf("cannot read %s", path)
	}
	if int64(len(data)) > max {
		return nil, fmt.Errorf("%s is larger than %d bytes", path, max)
	}
	return data, nil
}

// readPrivateDefault reads a secret file and, on Unix, refuses one that
// group or others can access.
func readPrivateDefault(path string) ([]byte, error) {
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(path)
		if err != nil {
			var pe *os.PathError
			if errors.As(err, &pe) {
				return nil, fmt.Errorf("cannot read %s: %v", path, pe.Err)
			}
			return nil, fmt.Errorf("cannot read %s", path)
		}
		if fi.Mode().Perm()&0o077 != 0 {
			return nil, fmt.Errorf("%s has mode %04o; it must be readable by its owner only (chmod 600)", path, fi.Mode().Perm())
		}
	}
	return readBounded(path, maxPEMFileSize)
}

// digest hashes the inputs of a sink configuration, so that equal inputs give
// equal digests and no input is kept as text.
type digest struct{ parts []string }

func newDigest(parts ...string) *digest { return &digest{parts: parts} }

func (d *digest) add(k, v string) { d.parts = append(d.parts, k, v) }

func (d *digest) sum() string {
	h := sha256.New()
	for _, p := range d.parts {
		_, _ = fmt.Fprintf(h, "%d:%s;", len(p), p)
	}
	return hex.EncodeToString(h.Sum(nil))
}
