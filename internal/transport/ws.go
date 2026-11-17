package transport

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/coder/websocket"

	"github.com/patrick-1984/ai-minion/internal/logx"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

const (
	// ConnectPath is the Hub endpoint a Worker dials (AD-3).
	ConnectPath = "/api/v1/worker/connect"
	// Subprotocol is the WebSocket subprotocol of the Worker connection.
	Subprotocol = "minion.v1"

	// wsReadLimit bounds one WebSocket message. The largest legitimate
	// message is a handshake frame (4 + 64 KiB); records are 16 KiB.
	wsReadLimit = 2*proto.HandshakeFrameLimit + 64
)

// ConnectURL turns the Hub URL a Worker was configured with (https, http,
// wss or ws; an optional path prefix for a reverse proxy is kept) into the
// WebSocket URL of the connect endpoint.
func ConnectURL(hubURL string) (string, error) {
	// The input may carry credentials (https://user:secret@host), so no error
	// below formats it: a *url.Error repeats the raw input and some of its
	// inner errors quote parts of it, hence a fixed message for a parse
	// failure, and fixed messages elsewhere.
	u, err := url.Parse(strings.TrimSpace(hubURL))
	if err != nil {
		return "", errors.New("transport: hub url is not a valid URL")
	}
	switch strings.ToLower(u.Scheme) {
	case "https", "wss":
		u.Scheme = "wss"
	case "http", "ws":
		u.Scheme = "ws"
	default:
		return "", errors.New("transport: hub url must use https, http, wss or ws")
	}
	if u.Host == "" {
		return "", errors.New("transport: hub url has no host")
	}
	u.Path = strings.TrimRight(u.Path, "/") + ConnectPath
	u.RawPath = ""
	u.RawQuery = ""
	u.Fragment = ""
	u.User = nil
	return u.String(), nil
}

// dialClient returns the HTTP client of the WSS dial. It accepts any server
// certificate (self-signed, wrong name, expired) on purpose (A-15, AD-6): the
// Hub is authenticated by the handshake inside the channel, never by TLS, so
// the dial needs no CA file and no insecure flag. Those flags belong to plain
// HTTPS calls only.
func dialClient() *http.Client {
	return &http.Client{Transport: &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		TLSClientConfig: &tls.Config{
			MinVersion:         tls.VersionTLS12,
			InsecureSkipVerify: true, //nolint:gosec // trust comes from the AD-6 handshake (A-15)
			NextProtos:         []string{"http/1.1"},
		},
		DisableKeepAlives: true,
	}}
}

// Dial opens the Worker's WebSocket to the Hub (subprotocol minion.v1, binary
// messages) and returns it as a net.Conn on which the AD-6 handshake runs. ctx
// bounds only the dial; closing the returned conn ends the connection.
func Dial(ctx context.Context, hubURL string) (net.Conn, error) {
	conn, _, err := DialInfo(ctx, hubURL)
	return conn, err
}

// DialFacts describe a dial for the debug log: where it went and what the TLS
// peer presented. The certificate is not trusted for anything (A-15); its
// subject, issuer and fingerprint help to see a TLS-intercepting proxy or a
// wrong host.
type DialFacts struct {
	// URL is the WebSocket URL dialled and Proxy the proxy from the
	// environment ("" for none), both as scheme and host only
	// ([logx.SafeURL]): no credential, path or query.
	URL   string
	Proxy string
	// HTTPStatus is the status of the upgrade answer (101 when it worked).
	HTTPStatus int
	// TLS facts; empty for ws://.
	TLSVersion  string
	PeerSubject string
	PeerIssuer  string
	// PeerSHA256 is the SHA-256 of the leaf certificate (hex).
	PeerSHA256  string
	Subprotocol string
}

// DialInfo is Dial that also returns the facts of the dial, also when it
// failed after the HTTP answer.
func DialInfo(ctx context.Context, hubURL string) (net.Conn, DialFacts, error) {
	var facts DialFacts
	wsURL, err := ConnectURL(hubURL)
	if err != nil {
		return nil, facts, err
	}
	// Only scheme and host reach a log line: a proxy may carry a credential
	// as its user name, which URL.Redacted would keep.
	facts.URL = logx.SafeURL(wsURL)
	if req, rerr := http.NewRequest(http.MethodGet, strings.Replace(strings.Replace(wsURL, "wss://", "https://", 1), "ws://", "http://", 1), nil); rerr == nil {
		if pu, perr := http.ProxyFromEnvironment(req); perr == nil && pu != nil {
			facts.Proxy = logx.SafeURL(pu.String())
		}
	}
	c, resp, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		HTTPClient:      dialClient(),
		Subprotocols:    []string{Subprotocol},
		CompressionMode: websocket.CompressionDisabled,
	})
	if resp != nil {
		facts.HTTPStatus = resp.StatusCode
		if resp.TLS != nil {
			facts.TLSVersion = tls.VersionName(resp.TLS.Version)
			if len(resp.TLS.PeerCertificates) > 0 {
				leaf := resp.TLS.PeerCertificates[0]
				sum := sha256.Sum256(leaf.Raw)
				facts.PeerSubject, facts.PeerIssuer = leaf.Subject.String(), leaf.Issuer.String()
				facts.PeerSHA256 = hex.EncodeToString(sum[:])
			}
		}
	}
	if err != nil {
		return nil, facts, fmt.Errorf("transport: dial hub: %w", err)
	}
	facts.Subprotocol = c.Subprotocol()
	if c.Subprotocol() != Subprotocol {
		_ = c.Close(websocket.StatusProtocolError, "subprotocol "+Subprotocol+" required")
		return nil, facts, fmt.Errorf("transport: hub did not select subprotocol %s", Subprotocol)
	}
	return wrap(c), facts, nil
}

// ErrNotWorkerConnect is returned by Accept for a request that is not a
// WebSocket upgrade with subprotocol minion.v1; the HTTP response has already
// been written.
var ErrNotWorkerConnect = errors.New("transport: not a minion.v1 websocket request")

// Accept upgrades a request to the Worker WebSocket on the Hub. It requires
// subprotocol minion.v1 and answers everything else itself (400 for a request
// that is no upgrade, a close for a missing subprotocol). Browsers from
// another origin are refused by the library's origin check. The returned
// net.Conn carries binary messages.
func Accept(w http.ResponseWriter, r *http.Request) (net.Conn, error) {
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		Subprotocols:    []string{Subprotocol},
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrNotWorkerConnect, err)
	}
	if c.Subprotocol() != Subprotocol {
		_ = c.Close(websocket.StatusPolicyViolation, "subprotocol "+Subprotocol+" required")
		return nil, fmt.Errorf("%w: subprotocol %q", ErrNotWorkerConnect, c.Subprotocol())
	}
	return wrap(c), nil
}

// wrap turns the WebSocket into a net.Conn. NetConn itself resets the read
// limit to unlimited, so the limit is set after it has been created.
func wrap(c *websocket.Conn) net.Conn {
	nc := websocket.NetConn(context.Background(), c, websocket.MessageBinary)
	c.SetReadLimit(wsReadLimit)
	return nc
}
