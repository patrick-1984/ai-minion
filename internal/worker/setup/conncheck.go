package setup

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/transport"
)

// The four stages of the Hub address check, in the order they run (UX-DR1).
const (
	StageDNS      = "DNS"
	StageTCP      = "TCP"
	StageTLS      = "TLS"
	StageProtocol = "Protocol"
)

// Status is the outcome of one stage.
type Status string

// The outcomes of a stage.
const (
	// StatusOK means the stage passed.
	StatusOK Status = "ok"
	// StatusWarning means the stage passed with something the user should
	// know: the certificate is not trusted by this computer, or the
	// connection is not encrypted. The Worker can still work with the Hub.
	StatusWarning Status = "warning"
	// StatusFailed means the Worker cannot reach the Hub this way.
	StatusFailed Status = "failed"
	// StatusSkipped means the stage did not run, because it makes no sense
	// (plain http has no TLS) or an earlier stage failed.
	StatusSkipped Status = "skipped"
)

// Stage is the plain-English result of one stage of the check.
type Stage struct {
	Name   string
	Status Status
	// Message is a sentence a non-technical user can act on. It never
	// contains secrets: the Hub URL is shown without credentials.
	Message string
}

// Report is the result of checking a Hub address.
type Report struct {
	// URL is the address that was checked.
	URL string
	// Stages has the four stages in order.
	Stages []Stage
	// CertFingerprint is the SHA-256 fingerprint of the certificate the Hub
	// presented, as colon-separated upper-case hex. It is empty when no
	// certificate was seen (plain http, or the TLS stage did not get that far).
	CertFingerprint string
	// CertTrusted is true only when this computer verified the certificate
	// against its trusted authorities (or the CA file of the hub_ca_file
	// setting). A self-signed Hub never gets true.
	CertTrusted bool
	// Notes are extra remarks, for example about a configured proxy.
	Notes []string
}

// Failed reports whether any stage failed.
func (r Report) Failed() bool {
	for _, s := range r.Stages {
		if s.Status == StatusFailed {
			return true
		}
	}
	return false
}

// Stage returns the stage with the given name.
func (r Report) Stage(name string) (Stage, bool) {
	for _, s := range r.Stages {
		if s.Name == name {
			return s, true
		}
	}
	return Stage{}, false
}

// Checker checks a Hub address stage by stage: DNS, TCP, TLS and protocol.
// The zero value is ready to use and checks with the system resolver, the
// system trust store and the real Worker dial.
//
// What the TLS stage means. The Worker never relies on the Hub's TLS
// certificate (A-15, AD-6): the channel to the Hub is authenticated inside, by
// the pinned key or the pairing code. So the check does not fail a Hub whose
// certificate this computer does not trust, and it never says the certificate
// is verified when it is not. It first verifies the certificate like any
// HTTPS client. When that fails, it looks at the certificate once without
// verifying it, only to show its fingerprint, and reports a warning that
// says exactly why the certificate is not trusted.
type Checker struct {
	// LookupHost resolves a name; nil uses the system resolver.
	LookupHost func(ctx context.Context, host string) ([]string, error)
	// DialContext opens a TCP connection; nil uses a net.Dialer.
	DialContext func(ctx context.Context, network, addr string) (net.Conn, error)
	// DialHub opens the Worker's real connection to the Hub (the WSS dial
	// with subprotocol minion.v1); nil uses [transport.Dial]. The check only
	// opens it and closes it again: it sends nothing, so no Hub state changes.
	DialHub func(ctx context.Context, hubURL string) (net.Conn, error)
	// RootCAs are the authorities that make a certificate trusted, from the
	// hub_ca_file setting; nil means the system trust store.
	RootCAs *x509.CertPool
	// Insecure is the hub_insecure setting: certificate trust is not checked.
	// The stage then reports a warning and the fingerprint, never "trusted".
	Insecure bool
	// StageTimeout bounds each stage; zero means 10 seconds.
	StageTimeout time.Duration
	// Proxy returns the proxy the Worker's connection would use for a URL;
	// nil uses the proxy environment variables like the Worker's dial does.
	Proxy func(*url.URL) (*url.URL, error)
}

const defaultStageTimeout = 10 * time.Second

func (c *Checker) timeout() time.Duration {
	if c.StageTimeout > 0 {
		return c.StageTimeout
	}
	return defaultStageTimeout
}

// Check checks hubURL. It always returns a report with the four stages, and
// stops running stages after the first failure (the rest are skipped). It
// returns early with skipped stages when ctx ends.
func (c *Checker) Check(ctx context.Context, hubURL string) Report {
	rep := Report{URL: redact(hubURL)}
	u, err := url.Parse(strings.TrimSpace(hubURL))
	if err != nil || u.Hostname() == "" {
		rep.Stages = append(rep.Stages, Stage{StageDNS, StatusFailed, "This is not a usable address. Use a form like https://hub.example:8443."})
		return c.skipRest(rep, StageTCP)
	}
	scheme := strings.ToLower(u.Scheme)
	secure := scheme == "https" || scheme == "wss"
	host := u.Hostname()
	port := u.Port()
	if port == "" {
		port = "80"
		if secure {
			port = "443"
		}
	}

	if proxy := c.proxyFor(u); proxy != "" {
		rep.Notes = append(rep.Notes, "This computer is set up to reach the Hub through a proxy ("+proxy+"). "+
			"DNS, TCP and TLS below are checked directly from here; the protocol check uses the proxy like the Worker does.")
	}

	addrs, st := c.checkDNS(ctx, host)
	rep.Stages = append(rep.Stages, st)
	if st.Status == StatusFailed {
		return c.skipRest(rep, StageTCP)
	}

	conn, st := c.checkTCP(ctx, host, port, addrs)
	rep.Stages = append(rep.Stages, st)
	if st.Status == StatusFailed {
		return c.skipRest(rep, StageTLS)
	}

	if secure {
		tlsStage, fp, trusted := c.checkTLS(ctx, conn, host, port, addrs)
		rep.Stages = append(rep.Stages, tlsStage)
		rep.CertFingerprint, rep.CertTrusted = fp, trusted
		if tlsStage.Status == StatusFailed {
			return c.skipRest(rep, StageProtocol)
		}
	} else {
		_ = conn.Close()
		rep.Stages = append(rep.Stages, Stage{StageTLS, StatusWarning,
			"The address starts with http://, so the connection to the Hub is not encrypted by TLS. " +
				"Use https:// if the Hub offers it. The Worker still authenticates the Hub inside its own channel."})
	}

	// The dial gets the address without userinfo, query and fragment: the
	// Worker's connection uses none of them, and no error of it may repeat them.
	rep.Stages = append(rep.Stages, c.checkProtocol(ctx, rep.URL))
	return rep
}

// skipRest marks every stage from first on as skipped.
func (c *Checker) skipRest(rep Report, first string) Report {
	order := []string{StageDNS, StageTCP, StageTLS, StageProtocol}
	skip := false
	for _, name := range order {
		if name == first {
			skip = true
		}
		if skip {
			rep.Stages = append(rep.Stages, Stage{name, StatusSkipped, "Not checked, because an earlier step failed."})
		}
	}
	return rep
}

func (c *Checker) proxyFor(u *url.URL) string {
	proxy := c.Proxy
	if proxy == nil {
		proxy = func(u *url.URL) (*url.URL, error) {
			return http.ProxyFromEnvironment(&http.Request{URL: wsToHTTP(u)})
		}
	}
	p, err := proxy(u)
	if err != nil || p == nil {
		return ""
	}
	return p.Host
}

// wsToHTTP maps the Hub URL to the URL scheme that the proxy environment
// variables are keyed on.
func wsToHTTP(u *url.URL) *url.URL {
	v := *u
	switch strings.ToLower(v.Scheme) {
	case "wss":
		v.Scheme = "https"
	case "ws":
		v.Scheme = "http"
	}
	return &v
}

func (c *Checker) checkDNS(ctx context.Context, host string) ([]string, Stage) {
	if ip := net.ParseIP(host); ip != nil {
		return []string{host}, Stage{StageDNS, StatusOK, "The address is an IP address, so no name lookup is needed."}
	}
	sctx, cancel := context.WithTimeout(ctx, c.timeout())
	defer cancel()
	lookup := c.LookupHost
	if lookup == nil {
		lookup = net.DefaultResolver.LookupHost
	}
	addrs, err := lookup(sctx, host)
	if err != nil || len(addrs) == 0 {
		return nil, Stage{StageDNS, StatusFailed, describeDNS(sctx, ctx, host, err)}
	}
	return addrs, Stage{StageDNS, StatusOK, fmt.Sprintf("%s was found (%s).", host, firstAddrs(addrs))}
}

func firstAddrs(addrs []string) string {
	if len(addrs) > 2 {
		return strings.Join(addrs[:2], ", ") + ", ..."
	}
	return strings.Join(addrs, ", ")
}

func describeDNS(stage, parent context.Context, host string, err error) string {
	var dnsErr *net.DNSError
	switch {
	case parent.Err() != nil:
		return "The check was cancelled."
	case err == nil:
		return fmt.Sprintf("The name %s has no address. Check the spelling.", host)
	case errors.As(err, &dnsErr) && dnsErr.IsNotFound:
		return fmt.Sprintf("The name %s does not exist. Check the spelling of the address, or whether this computer can reach your DNS server.", host)
	case errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &dnsErr) && dnsErr.IsTimeout) || stage.Err() != nil:
		return fmt.Sprintf("Looking up %s took too long. Check the network connection of this computer.", host)
	}
	return fmt.Sprintf("The name %s could not be looked up. Check the network connection of this computer.", host)
}

// dial opens a TCP connection to one of addrs on port within the stage
// timeout. It tries the addresses in order and returns the first success.
func (c *Checker) dial(ctx context.Context, host, port string, addrs []string) (net.Conn, error) {
	dial := c.DialContext
	if dial == nil {
		d := &net.Dialer{}
		dial = d.DialContext
	}
	var last error
	for _, a := range addrs {
		conn, err := dial(ctx, "tcp", net.JoinHostPort(a, port))
		if err == nil {
			return conn, nil
		}
		last = err
		if ctx.Err() != nil {
			break
		}
	}
	if last == nil {
		last = fmt.Errorf("no address to connect to for %s", host)
	}
	return nil, last
}

func (c *Checker) checkTCP(ctx context.Context, host, port string, addrs []string) (net.Conn, Stage) {
	sctx, cancel := context.WithTimeout(ctx, c.timeout())
	defer cancel()
	conn, err := c.dial(sctx, host, port, addrs)
	if err != nil {
		return nil, Stage{StageTCP, StatusFailed, describeTCP(sctx, ctx, host, port, err)}
	}
	return conn, Stage{StageTCP, StatusOK, fmt.Sprintf("Connected to %s on port %s.", host, port)}
}

func describeTCP(stage, parent context.Context, host, port string, err error) string {
	var ne net.Error
	switch {
	case parent.Err() != nil:
		return "The check was cancelled."
	case errors.Is(err, syscall.ECONNREFUSED) || strings.Contains(err.Error(), "refused"):
		return fmt.Sprintf("%s answered, but nothing listens on port %s (connection refused). Check the port number in the address.", host, port)
	case stage.Err() != nil || (errors.As(err, &ne) && ne.Timeout()):
		return fmt.Sprintf("%s did not answer on port %s in time. Check the address and port, and whether a firewall blocks the connection.", host, port)
	case errors.Is(err, syscall.ENETUNREACH) || errors.Is(err, syscall.EHOSTUNREACH):
		return fmt.Sprintf("There is no network route to %s. Check the network connection of this computer.", host)
	}
	return fmt.Sprintf("Could not connect to %s on port %s. Check the address and the network connection.", host, port)
}

// checkTLS runs the TLS stage on conn (a fresh TCP connection to the Hub). It
// returns the stage, the certificate fingerprint and whether the certificate
// was verified.
func (c *Checker) checkTLS(ctx context.Context, conn net.Conn, host, port string, addrs []string) (Stage, string, bool) {
	sctx, cancel := context.WithTimeout(ctx, c.timeout())
	defer cancel()

	if !c.Insecure {
		cert, err := handshake(sctx, conn, c.tlsConfig(host, false))
		if err == nil {
			fp := fingerprint(cert)
			return Stage{StageTLS, StatusOK, fmt.Sprintf(
				"The Hub's certificate is trusted by this computer (%s). Fingerprint %s.", validity(cert), fp)}, fp, true
		}
		if !isVerifyError(err) {
			return Stage{StageTLS, StatusFailed, describeTLS(sctx, ctx, err)}, "", false
		}
		// The certificate is not trusted: look at it without trusting it, only
		// to show its fingerprint.
		reason := describeVerify(err)
		conn2, derr := c.dial(sctx, host, port, addrs)
		if derr != nil {
			return Stage{StageTLS, StatusFailed, describeTCP(sctx, ctx, host, port, derr)}, "", false
		}
		cert, err = handshake(sctx, conn2, c.tlsConfig(host, true))
		if err != nil {
			return Stage{StageTLS, StatusFailed, describeTLS(sctx, ctx, err)}, "", false
		}
		fp := fingerprint(cert)
		return Stage{StageTLS, StatusWarning, fmt.Sprintf(
			"The Hub's certificate is not trusted by this computer: %s. "+
				"That is normal for a Hub with its own (self-signed) certificate. The Worker does not rely on it: "+
				"it authenticates the Hub inside its own channel. Certificate fingerprint %s; "+
				"compare it with the one the Hub shows if you want to be sure.", reason, fp)}, fp, false
	}

	cert, err := handshake(sctx, conn, c.tlsConfig(host, true))
	if err != nil {
		return Stage{StageTLS, StatusFailed, describeTLS(sctx, ctx, err)}, "", false
	}
	fp := fingerprint(cert)
	return Stage{StageTLS, StatusWarning, fmt.Sprintf(
		"The Hub answers with TLS, but certificate checking is switched off (hub_insecure), so its certificate is not verified. "+
			"Certificate fingerprint %s.", fp)}, fp, false
}

func (c *Checker) tlsConfig(host string, skipVerify bool) *tls.Config {
	cfg := &tls.Config{
		ServerName: host,
		RootCAs:    c.RootCAs,
		MinVersion: tls.VersionTLS12,
		NextProtos: []string{"http/1.1"},
	}
	if skipVerify {
		// Only to read the certificate for its fingerprint; nothing is sent
		// on this connection and no result of it is called "trusted".
		cfg.InsecureSkipVerify = true //nolint:gosec // see above
	}
	return cfg
}

// handshake runs a TLS client handshake on conn and returns the Hub's
// certificate. It closes conn.
func handshake(ctx context.Context, conn net.Conn, cfg *tls.Config) (*x509.Certificate, error) {
	defer func() { _ = conn.Close() }()
	tc := tls.Client(conn, cfg)
	if err := tc.HandshakeContext(ctx); err != nil {
		return nil, err
	}
	certs := tc.ConnectionState().PeerCertificates
	if len(certs) == 0 {
		return nil, errors.New("the server sent no certificate")
	}
	return certs[0], nil
}

func fingerprint(c *x509.Certificate) string {
	sum := sha256.Sum256(c.Raw)
	var b strings.Builder
	b.WriteString("SHA-256 ")
	for i, v := range sum {
		if i > 0 {
			b.WriteByte(':')
		}
		fmt.Fprintf(&b, "%02X", v)
	}
	return b.String()
}

func validity(c *x509.Certificate) string {
	return "valid until " + c.NotAfter.UTC().Format("2006-01-02")
}

// isVerifyError reports whether err means that the handshake worked but the
// certificate did not verify.
func isVerifyError(err error) bool {
	var cve *tls.CertificateVerificationError
	var ua x509.UnknownAuthorityError
	var he x509.HostnameError
	var ci x509.CertificateInvalidError
	return errors.As(err, &cve) || errors.As(err, &ua) || errors.As(err, &he) || errors.As(err, &ci)
}

func describeVerify(err error) string {
	var he x509.HostnameError
	var ci x509.CertificateInvalidError
	var ua x509.UnknownAuthorityError
	switch {
	case errors.As(err, &he):
		return "it was issued for a different name than the one in the address"
	case errors.As(err, &ci) && ci.Reason == x509.Expired:
		return "it has expired or is not valid yet"
	case errors.As(err, &ua):
		return "it is signed by an authority this computer does not know (self-signed or private authority)"
	}
	return "this computer could not verify it"
}

func describeTLS(stage, parent context.Context, err error) string {
	var ne net.Error
	msg := err.Error()
	switch {
	case parent.Err() != nil:
		return "The check was cancelled."
	case stage.Err() != nil || (errors.As(err, &ne) && ne.Timeout()):
		return "The Hub did not finish the secure (TLS) handshake in time. Check the address and port."
	case strings.Contains(msg, "first record does not look like a TLS handshake"),
		strings.Contains(msg, "http: server gave HTTP response to HTTPS client"):
		return "The server on this port does not speak HTTPS. Use http:// if the Hub has no TLS, or check the port number."
	case strings.Contains(msg, "protocol version"):
		return "The server only offers an old, unsafe TLS version that this Worker refuses."
	case strings.Contains(msg, "EOF"), strings.Contains(msg, "reset by peer"):
		return "The server closed the connection during the secure (TLS) handshake. Check the port; the Hub may be behind a proxy that expects another address."
	}
	return "The secure (TLS) handshake with the Hub failed. Check the address and port."
}

var statusCodeRE = regexp.MustCompile(`but got (\d{3})`)

// checkProtocol opens the Worker's real connection to the Hub: the WSS dial
// with subprotocol minion.v1. Opening it proves that the address is a Hub that
// speaks the protocol of this Worker. The check closes it at once without
// sending a hello, so the Hub does not see a connection attempt of a Worker.
func (c *Checker) checkProtocol(ctx context.Context, hubURL string) Stage {
	sctx, cancel := context.WithTimeout(ctx, c.timeout())
	defer cancel()
	dial := c.DialHub
	if dial == nil {
		dial = transport.Dial
	}
	conn, err := dial(sctx, hubURL)
	if err != nil {
		return Stage{StageProtocol, StatusFailed, describeProtocol(sctx, ctx, err)}
	}
	_ = conn.Close()
	return Stage{StageProtocol, StatusOK, fmt.Sprintf(
		"The Hub speaks the Minion connection protocol (%s, version %s of this Worker). "+
			"The exact version range is agreed when the Worker pairs.", transport.Subprotocol, proto.Current)}
}

func describeProtocol(stage, parent context.Context, err error) string {
	msg := err.Error()
	var ne net.Error
	if m := statusCodeRE.FindStringSubmatch(msg); m != nil {
		code, _ := strconv.Atoi(m[1])
		switch code {
		case http.StatusNotFound:
			return "The server answered (HTTP 404), but it is not a Minion Hub at this address. Check the address, including any path that a reverse proxy adds."
		case http.StatusUnauthorized, http.StatusForbidden:
			return fmt.Sprintf("The server refused the connection (HTTP %d). A proxy or firewall in front of the Hub may be blocking Workers.", code)
		case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
			return fmt.Sprintf("The server answered (HTTP %d), but the Hub behind it is not reachable. Is the Hub running?", code)
		}
		return fmt.Sprintf("The server answered (HTTP %d), but it is not a Minion Hub, or a proxy in front of it does not pass WebSocket connections.", code)
	}
	switch {
	case parent.Err() != nil:
		return "The check was cancelled."
	case strings.Contains(msg, "subprotocol"):
		return "The server accepts WebSocket connections but does not speak the Minion protocol this Worker uses (" + transport.Subprotocol + "). It may be a different program, or a Hub of an incompatible version."
	case stage.Err() != nil || (errors.As(err, &ne) && ne.Timeout()):
		return "The Hub did not answer the connection request in time. Check the address and whether a proxy in front of the Hub passes WebSocket connections."
	}
	return "The server did not accept a Minion connection. Check that the address is a Minion Hub."
}

// redact returns raw reduced to scheme, host and path, so that it can be
// shown: the user name and password, the query and the fragment are dropped,
// because any of them may be a credential and none is used by the connection.
// A value that does not parse is never echoed.
func redact(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "(invalid address)"
	}
	u.User = nil
	u.RawQuery, u.ForceQuery, u.Fragment, u.RawFragment = "", false, "", ""
	u.Opaque = ""
	return u.String()
}
