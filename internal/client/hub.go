package client

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/netip"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// The Hub REST backend (AD-22, AD-18). Every op is one request,
//
//	POST <hub>/api/v1/ops/<name>
//	Authorization: Bearer mn_...
//	Content-Type: application/json
//
// whose body is the op's arguments as one JSON object. A scalar op answers
// 200 with the result as JSON. A streaming op answers 200 with
// application/x-ndjson: one element per line, flushed as it is produced, and
// when it fails after it started, one last line {"error":{...}}. A failure
// before anything was produced is the HTTP status of the code and the same
// envelope as the body: {"error":{"code","message","details"}}.
//
// What this backend guarantees, and the Hub's contract it relies on:
//
//   - The API key is read from a file for each request (a rotated key is used
//     at once, nothing keeps it alive in memory longer than a call), through
//     bounded and checked reads of the file it opened: a regular file owned
//     by this user that group and others cannot access (a symbolic link is
//     refused), the identity package's private-file rule on every platform.
//     It is never written to an error, a log, a URL or an argument, and every
//     text that came from the other side (messages, and the decoded strings
//     and member names of error details) is scrubbed of it before it is
//     returned. A Hub URL that looks like it holds a key is refused.
//   - The Hub certificate is verified against the system roots. A private CA
//     file replaces them, and an explicit insecure switch skips the check; both
//     live in this client's own transport and change nothing global.
//   - A redirect is never followed: the key goes to the configured origin and
//     to no other, and a write is never sent twice.
//   - Nothing is retried. A write whose request may have gone out and whose
//     answer did not arrive is [ErrConnectionLost] (outcome unknown, AD-15),
//     never "not sent". Only a failure before the request left (the connection
//     was never made, the certificate was refused, the key file is bad) is a
//     definite "nothing was sent".
//   - The size of every answer, line and error body is bounded.
//   - A stream has no overall timeout: a legitimate wait (pipe_wait) may last
//     as long as it takes, and only the caller's context, a dead connection
//     (TCP keep-alive) or the Hub ends it. A scalar call is bounded by the call
//     timeout.

// Typed errors of the Hub backend. A refusal by the Hub is a *proto.Error,
// like on the local socket.
var (
	// ErrHubURL means the Hub URL is unusable. Nothing was sent.
	ErrHubURL = errors.New("the Hub URL is not usable")

	// ErrHubInsecureScheme means the Hub URL is plain http to a host that is
	// not this machine: the API key would travel in clear text. Nothing was
	// sent.
	ErrHubInsecureScheme = errors.New("refusing to send the API key over plain http to a host that is not this machine")

	// ErrAPIKeyMissing means no key file was configured. Nothing was sent.
	ErrAPIKeyMissing = errors.New("no API key file is configured")

	// ErrAPIKeyFile means the key file cannot be used: it is missing, not a
	// regular file, too large, empty or does not hold a Minion API key. The
	// error names the path and never the content. Nothing was sent.
	ErrAPIKeyFile = errors.New("the API key file cannot be used")

	// ErrHubWorker means the default Worker is not a Worker id (UUIDv7). The
	// Hub's REST API takes the id of a Worker in worker_id, and the commands
	// and MCP tools name a Worker by its id everywhere; no name is resolved on
	// the client side. Nothing was sent.
	ErrHubWorker = errors.New("the default Hub Worker is not usable")

	// ErrHubCA means the private CA file cannot be used. Nothing was sent.
	ErrHubCA = errors.New("the Hub CA file cannot be used")

	// ErrHubTLSConflict means both a private CA and the insecure switch were
	// given, which say opposite things about trust.
	ErrHubTLSConflict = errors.New("a Hub CA file and the insecure switch cannot be combined")

	// ErrHubUnreachable means the request never left: the connection could not
	// be made or the certificate was refused. Nothing was sent.
	ErrHubUnreachable = errors.New("cannot reach the Hub")

	// ErrHubCertificate is the case of ErrHubUnreachable in which the TLS
	// certificate of the Hub was not accepted. Nothing was sent.
	ErrHubCertificate = errors.New("the Hub's TLS certificate is not trusted")

	// ErrHubRedirect means the Hub answered with a redirect, which is never
	// followed. The request was not run by that origin.
	ErrHubRedirect = errors.New("the Hub answered with a redirect")
)

// HubInsecureWarning is the warning of a client that does not verify the
// Hub's certificate. It is given at every start of such a client.
const HubInsecureWarning = "warning: --hub-insecure is on: this client does not verify the Hub's TLS certificate, so anyone on the network path can read the API key and the sessions; use --hub-ca-file with the Hub's CA instead"

// Bounds of the Hub backend.
const (
	// DefaultHubCallTimeout bounds a scalar call: a little more than the
	// 2 minutes the Hub itself waits for a Worker's answer.
	DefaultHubCallTimeout = 150 * time.Second

	hubDialTimeout     = 10 * time.Second
	hubTLSTimeout      = 10 * time.Second
	hubKeepAlive       = 30 * time.Second
	maxAPIKeyFileBytes = 4 << 10
	maxCAFileBytes     = 1 << 20
	maxRequestBytes    = 1 << 20 // the Hub's own limit (api.MaxBodyBytes)
	maxResultBytes     = 4 * proto.StreamFrameLimit
	maxStreamLineBytes = 2 * proto.StreamFrameLimit
	maxErrorBodyBytes  = 64 << 10
	maxErrorTextBytes  = 512
)

// HubConfig configures [NewHub].
type HubConfig struct {
	// URL is the base URL of the Hub, https or (for this machine) http, with
	// the path prefix of a reverse proxy when there is one.
	URL string
	// APIKeyFile is the file that holds the API key. It is read for each call.
	APIKeyFile string
	// CAFile is a PEM file with the private CA that signed the Hub's
	// certificate. It replaces the system roots for this client.
	CAFile string
	// Insecure skips the verification of the Hub's certificate for this client.
	Insecure bool
	// Worker is the id (UUIDv7) of the Worker that runs an op whose arguments
	// name none (sessions_list, session_create), and the Worker of a
	// structured target that has none. It is never a name: the Hub accepts
	// only an id in worker_id, and a name is refused by [NewHub] rather than
	// sent. A Worker named in the arguments always wins; a target in the
	// human form <worker>:<session> that a caller of the REST API passes
	// through is resolved by the Hub.
	Worker string
}

// HubOption configures [NewHub].
type HubOption func(*HubBackend)

// WithHubWarn sets where the permanent warning of an insecure client goes
// (default: nowhere).
func WithHubWarn(f func(string)) HubOption { return func(b *HubBackend) { b.warn = f } }

// WithHubCallTimeout changes the bound of a scalar call.
func WithHubCallTimeout(d time.Duration) HubOption {
	return func(b *HubBackend) { b.callTimeout = d }
}

// WithHubUserAgent sets the User-Agent header (default "minion").
func WithHubUserAgent(ua string) HubOption { return func(b *HubBackend) { b.userAgent = ua } }

// HubBackend is the [Backend] over the Hub REST API. It is safe for
// concurrent use.
type HubBackend struct {
	base        *url.URL
	label       string // scheme://host with anything key-shaped redacted, for messages
	keyFile     string
	worker      string
	hc          *http.Client
	callTimeout time.Duration
	warn        func(string)
	userAgent   string
}

var _ Backend = (*HubBackend)(nil)

// NewHub returns the backend for the Hub at cfg.URL. It checks the
// configuration and loads the CA file, and it reads the key file once to fail
// early, but it makes no connection. Every failure is one of the typed errors
// above. The client's TLS settings are its own: nothing global is changed.
func NewHub(cfg HubConfig, opts ...HubOption) (*HubBackend, error) {
	b := &HubBackend{callTimeout: DefaultHubCallTimeout, userAgent: "minion", worker: strings.TrimSpace(cfg.Worker)}
	for _, o := range opts {
		o(b)
	}
	base, err := parseHubURL(cfg.URL)
	if err != nil {
		return nil, err
	}
	b.base = base
	if cfg.APIKeyFile == "" {
		return nil, fmt.Errorf("%w: set MINION_API_KEY_FILE to a file that holds the API key", ErrAPIKeyMissing)
	}
	b.keyFile = cfg.APIKeyFile
	key, err := readAPIKey(b.keyFile)
	if err != nil {
		return nil, err
	}
	// The label of the origin is what messages show. parseHubURL already
	// refused a host that looks like a key; the redaction is the second wall
	// and also removes the key itself, which only this read knows.
	b.label = scrub(base.Scheme+"://"+base.Host, key, maxErrorTextBytes)
	if b.worker != "" && !ops.ValidUUIDv7(b.worker) {
		return nil, fmt.Errorf("%w: %s", ErrHubWorker, "the default Worker must be a Worker id (a UUIDv7 as listed by workers_list); a Worker name is not accepted")
	}
	if cfg.CAFile != "" && cfg.Insecure {
		return nil, ErrHubTLSConflict
	}
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}
	switch {
	case cfg.Insecure:
		tlsCfg.InsecureSkipVerify = true //nolint:gosec // explicit, scoped to this client, warned about
		if b.warn != nil {
			b.warn(HubInsecureWarning)
		}
	case cfg.CAFile != "":
		pem, err := readBoundedFile(cfg.CAFile, maxCAFileBytes)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrHubCA, err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("%w: %s holds no PEM certificate", ErrHubCA, cfg.CAFile)
		}
		tlsCfg.RootCAs = pool
	}
	tr := &http.Transport{
		Proxy:                  http.ProxyFromEnvironment,
		DialContext:            (&net.Dialer{Timeout: hubDialTimeout, KeepAlive: hubKeepAlive}).DialContext,
		TLSClientConfig:        tlsCfg,
		TLSHandshakeTimeout:    hubTLSTimeout,
		ForceAttemptHTTP2:      true,
		MaxIdleConns:           4,
		IdleConnTimeout:        30 * time.Second,
		MaxResponseHeaderBytes: 64 << 10,
		ExpectContinueTimeout:  time.Second,
	}
	b.hc = &http.Client{
		Transport: tr,
		// A redirect is returned to be reported, never followed: the key must
		// not leave the configured origin and a POST must not be sent twice.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		// No Timeout: it would also end a legitimate long stream.
	}
	return b, nil
}

// Close releases the idle connections.
func (b *HubBackend) Close() { b.hc.CloseIdleConnections() }

// parseHubURL validates the base URL and applies the plain-http policy.
func parseHubURL(raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, fmt.Errorf("%w: it is empty", ErrHubURL)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("%w: it is not a valid URL", ErrHubURL)
	}
	switch {
	case u.Hostname() == "":
		return nil, fmt.Errorf("%w: it has no host", ErrHubURL)
	case u.User != nil:
		return nil, fmt.Errorf("%w: it must not contain a user name or password", ErrHubURL)
	case u.RawQuery != "" || u.Fragment != "" || strings.ContainsAny(raw, "?#"):
		return nil, fmt.Errorf("%w: it must not contain a query or a fragment", ErrHubURL)
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
	case "http":
		if !isLoopbackHost(u.Hostname()) {
			return nil, fmt.Errorf("%w: use https:// (plain http is accepted only for localhost)", ErrHubInsecureScheme)
		}
	default:
		return nil, fmt.Errorf("%w: it must start with https://", ErrHubURL)
	}
	if config.LooksLikeKey(raw) || config.LooksLikeKey(u.Host) || config.LooksLikeKey(u.Path) {
		return nil, fmt.Errorf("%w: it contains something that looks like an API key; the key goes only in the file named by MINION_API_KEY_FILE", ErrHubURL)
	}
	u.Scheme = strings.ToLower(u.Scheme)
	u.Path = strings.TrimRight(u.Path, "/")
	u.RawPath = ""
	return u, nil
}

// isLoopbackHost reports whether host is this machine: localhost or a
// loopback address. A name other than localhost is not resolved here.
func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") || strings.HasSuffix(strings.ToLower(host), ".localhost") {
		return true
	}
	a, err := netip.ParseAddr(host)
	return err == nil && a.IsLoopback()
}

// endpoint returns the URL of op.
func (b *HubBackend) endpoint(op string) string {
	u := *b.base
	u.Path += "/api/v1/ops/" + op
	return u.String()
}

var opNameRE = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// Call implements [Backend].
func (b *HubBackend) Call(ctx context.Context, op string, args any) (json.RawMessage, error) {
	return b.run(ctx, op, args, nil)
}

// Stream implements [Backend]. The elements are the lines the Hub flushes; the
// returned result is JSON null, because a stream of the Hub has no final
// element.
func (b *HubBackend) Stream(ctx context.Context, op string, args any, onProgress func(json.RawMessage) error) (json.RawMessage, error) {
	return b.run(ctx, op, args, onProgress)
}

func (b *HubBackend) run(ctx context.Context, op string, args any, onProgress func(json.RawMessage) error) (json.RawMessage, error) {
	if !opNameRE.MatchString(op) {
		return nil, proto.NewError(proto.CodeInvalidArgument, "client: an op name is required")
	}
	meta, known := ops.Lookup(op)
	// An op this client does not know (a newer Hub) is treated as a write
	// where it matters: an ambiguous failure is then "unknown", never "unsent".
	write := !known || meta.ChangesState()
	stream := known && meta.Stream

	raw, err := encodeArgs(args)
	if err != nil {
		return nil, err
	}
	body, err := b.requestBody(meta, known, raw)
	if err != nil {
		return nil, err
	}
	key, err := readAPIKey(b.keyFile)
	if err != nil {
		return nil, err
	}

	callCtx, cancel := ctx, context.CancelFunc(func() {})
	if !stream {
		callCtx, cancel = context.WithTimeout(ctx, b.callTimeout)
	}
	defer cancel()

	var maybeSent atomic.Bool
	trace := &httptrace.ClientTrace{
		WroteHeaders: func() { maybeSent.Store(true) },
		WroteRequest: func(httptrace.WroteRequestInfo) { maybeSent.Store(true) },
	}
	req, err := http.NewRequestWithContext(httptrace.WithClientTrace(callCtx, trace), http.MethodPost, b.endpoint(op), bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrHubURL, "the request cannot be built")
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, application/x-ndjson")
	req.Header.Set("User-Agent", b.userAgent)

	resp, err := b.hc.Do(req)
	if err != nil {
		return nil, b.transportError(ctx, callCtx, err, key, maybeSent.Load())
	}
	defer func() { _ = resp.Body.Close() }()

	switch {
	case resp.StatusCode >= 300 && resp.StatusCode < 400:
		return nil, fmt.Errorf("%w (HTTP %d) from %s: check the Hub URL; redirects are not followed so that the API key stays with the configured host", ErrHubRedirect, resp.StatusCode, b.label)
	case resp.StatusCode != http.StatusOK:
		return nil, b.statusError(resp, key, write)
	}

	if !stream {
		data, err := io.ReadAll(io.LimitReader(resp.Body, maxResultBytes+1))
		if err != nil {
			return nil, b.readError(ctx, callCtx, err, key)
		}
		if len(data) > maxResultBytes {
			return nil, fmt.Errorf("%w: the Hub's answer is larger than %d bytes", ErrProtocol, maxResultBytes)
		}
		data = bytes.TrimSpace(data)
		if !json.Valid(data) {
			return nil, fmt.Errorf("%w: the Hub's answer is not JSON", ErrProtocol)
		}
		return json.RawMessage(data), nil
	}
	if err := b.readStream(ctx, resp, key, onProgress); err != nil {
		return nil, err
	}
	return json.RawMessage("null"), nil
}

// requestBody builds the JSON body: the arguments as one object, with the
// default Worker added where the arguments name none.
func (b *HubBackend) requestBody(op ops.Op, known bool, raw json.RawMessage) ([]byte, error) {
	members := map[string]json.RawMessage{}
	if t := bytes.TrimSpace(raw); len(t) > 0 && string(t) != "null" {
		if err := json.Unmarshal(t, &members); err != nil || members == nil {
			return nil, proto.NewError(proto.CodeInvalidArgument, "the arguments of an op must be one JSON object")
		}
	}
	if known && b.worker != "" && op.Scope == ops.ScopeWorker {
		withWorker(members, b.worker)
	}
	body, err := json.Marshal(members)
	if err != nil {
		return nil, proto.NewError(proto.CodeInvalidArgument, "the arguments are not encodable as JSON")
	}
	if len(body) > maxRequestBytes {
		return nil, proto.NewError(proto.CodeMessageTooLarge, fmt.Sprintf("the request exceeds %d bytes", maxRequestBytes))
	}
	return body, nil
}

// withWorker names the default Worker where the arguments name none: as the
// worker_id of a call without a target, or in a structured target that has no
// Worker part. A Worker the caller named is never replaced, and a target in
// the human form is left to the Hub, which resolves it in the caller's space.
func withWorker(members map[string]json.RawMessage, worker string) {
	id, _ := json.Marshal(worker)
	t, ok := members["target"]
	if !ok || string(bytes.TrimSpace(t)) == "null" {
		if _, has := members["worker_id"]; !has {
			members["worker_id"] = id
		}
		return
	}
	var target map[string]json.RawMessage
	if json.Unmarshal(t, &target) != nil || target == nil {
		return // the human form, or something the Hub will refuse
	}
	if cur, has := target["worker_id"]; has {
		var s string
		if json.Unmarshal(cur, &s) != nil || s != "" {
			return
		}
	}
	target["worker_id"] = id
	if enc, err := json.Marshal(target); err == nil {
		members["target"] = enc
	}
}

// transportError classifies a failure of the round trip. Before the request
// left, it is definitely "not sent"; after, it is unknown.
func (b *HubBackend) transportError(ctx, callCtx context.Context, err error, key string, maybeSent bool) error {
	if ctx.Err() != nil {
		return ctx.Err() // the caller gave up; the CLI treats a write as unknown
	}
	if errors.Is(callCtx.Err(), context.DeadlineExceeded) {
		if !maybeSent {
			return fmt.Errorf("%w at %s: no connection within %s", ErrHubUnreachable, b.label, b.callTimeout)
		}
		return fmt.Errorf("%w: no answer from the Hub within %s", ErrConnectionLost, b.callTimeout)
	}
	reason := b.reason(err, key)
	if !maybeSent {
		var (
			unknownAuth x509.UnknownAuthorityError
			hostErr     x509.HostnameError
			invalid     x509.CertificateInvalidError
			verify      *tls.CertificateVerificationError
		)
		if errors.As(err, &unknownAuth) || errors.As(err, &hostErr) || errors.As(err, &invalid) || errors.As(err, &verify) {
			return fmt.Errorf("%w by this client (%s): give the Hub's CA with --hub-ca-file, or skip the check for this client with --hub-insecure", ErrHubCertificate, reason)
		}
		return fmt.Errorf("%w at %s: %s", ErrHubUnreachable, b.label, reason)
	}
	return fmt.Errorf("%w: %s", ErrConnectionLost, reason)
}

// readError classifies a failure while the body is read: the request went out.
func (b *HubBackend) readError(ctx, callCtx context.Context, err error, key string) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(callCtx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("%w: no complete answer from the Hub within %s", ErrConnectionLost, b.callTimeout)
	}
	return fmt.Errorf("%w: %s", ErrConnectionLost, b.reason(err, key))
}

// reason returns a short, scrubbed description of a network error. It drops
// the URL a *url.Error carries.
func (b *HubBackend) reason(err error, key string) string {
	var ue *url.Error
	if errors.As(err, &ue) && ue.Err != nil {
		err = ue.Err
	}
	return scrub(err.Error(), key, maxErrorTextBytes)
}

// statusError maps a non-200 answer to the codes of the catalog. A body with
// the AD-18 envelope is used for its code and message; anything else (a
// proxy's page) is mapped from the status alone and never echoed.
func (b *HubBackend) statusError(resp *http.Response, key string, write bool) error {
	data, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))
	if pe := parseEnvelope(data, key); pe != nil {
		if wait, ok := retryAfter(resp.Header); ok && pe.Code == proto.CodeRateLimited {
			pe.Message += fmt.Sprintf(" (the Hub asks to wait %d s)", int(wait/time.Second))
		}
		return wrapUnknownWrite(pe, write)
	}
	code, ok := codeOfStatus(resp.StatusCode)
	if !ok {
		if write {
			return fmt.Errorf("%w: the Hub answered HTTP %d without an error code", ErrConnectionLost, resp.StatusCode)
		}
		return fmt.Errorf("%w: the Hub answered HTTP %d without an error code", ErrProtocol, resp.StatusCode)
	}
	pe := proto.NewError(code, fmt.Sprintf("the Hub answered HTTP %d without an error code", resp.StatusCode))
	if resp.StatusCode >= 500 {
		// A proxy or a failing Hub: whether it ran the op is not known.
		return fmt.Errorf("%w: %w", ErrConnectionLost, pe)
	}
	return pe
}

// wrapUnknownWrite makes an answer of the Hub that does not settle whether a
// write ran an [ErrConnectionLost] as well. The Hub's own `connection_lost`
// says so; `internal` means its handler failed after the write may have
// started.
func wrapUnknownWrite(pe *proto.Error, write bool) error {
	if pe.Code == proto.CodeConnectionLost || (write && pe.Code == proto.CodeInternal) {
		return fmt.Errorf("%w: %w", ErrConnectionLost, pe)
	}
	return pe
}

func codeOfStatus(status int) (proto.Code, bool) {
	switch status {
	case http.StatusBadRequest:
		return proto.CodeInvalidArgument, true
	case http.StatusUnauthorized:
		return proto.CodeUnauthenticated, true
	case http.StatusForbidden:
		return proto.CodeForbidden, true
	case http.StatusNotFound:
		return proto.CodeNotFound, true
	case http.StatusMethodNotAllowed:
		return proto.CodeMethodNotAllowed, true
	case http.StatusConflict:
		return proto.CodeConflict, true
	case http.StatusRequestEntityTooLarge:
		return proto.CodeMessageTooLarge, true
	case http.StatusTooManyRequests:
		return proto.CodeRateLimited, true
	case http.StatusNotImplemented:
		return proto.CodeUnsupportedOp, true
	}
	if status >= 500 {
		return proto.CodeInternal, true
	}
	return "", false
}

// envelope is the AD-18 error body.
type envelope struct {
	Error *struct {
		Code    proto.Code      `json:"code"`
		Message string          `json:"message"`
		Details json.RawMessage `json:"details"`
	} `json:"error"`
}

// parseEnvelope returns the error of an AD-18 envelope, or nil when data is
// not one or carries a code that is not declared (a Hub that is newer than
// this client, or something else answering). The message is scrubbed.
func parseEnvelope(data []byte, key string) *proto.Error {
	var e envelope
	if json.Unmarshal(bytes.TrimSpace(data), &e) != nil || e.Error == nil || !e.Error.Code.Known() {
		return nil
	}
	pe := proto.NewError(e.Error.Code, scrub(e.Error.Message, key, maxErrorTextBytes))
	if d := bytes.TrimSpace(e.Error.Details); len(d) > 0 && string(d) != "null" && string(d) != "{}" && len(d) <= 4<<10 {
		pe.Details = scrubJSON(d, key)
	}
	return pe
}

// maxDetailDepth bounds the nesting of the details that are inspected.
const maxDetailDepth = 16

// scrubJSON returns the details of an upstream error with the API key removed.
// The text is decoded first, because JSON can spell the same string in many
// ways (\u escapes, split into pieces by the encoder) that a search of the raw
// bytes does not see: every string value and every member name is inspected
// after decoding, a value that holds the key, anything shaped like one or a
// Bearer header is redacted, and the result is encoded again. Numbers keep
// their exact text (no float round trip) and all other values keep their type.
// A member name that holds a secret, details that are not one JSON value,
// that nest deeper than maxDetailDepth or that cannot be encoded again are
// dropped entirely (nil): the code and the message of the error stay.
func scrubJSON(d json.RawMessage, key string) json.RawMessage {
	dec := json.NewDecoder(bytes.NewReader(d))
	dec.UseNumber()
	var v any
	if dec.Decode(&v) != nil {
		return nil
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil // something follows the value
	}
	v, ok := redactDetail(v, key, 0)
	if !ok {
		return nil
	}
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	enc.SetEscapeHTML(false)
	if enc.Encode(v) != nil {
		return nil
	}
	return json.RawMessage(bytes.TrimSpace(out.Bytes()))
}

// secretText reports whether s holds the key or text shaped like a credential.
func secretText(s, key string) bool {
	return (key != "" && strings.Contains(s, key)) || keyShapeRE.MatchString(s) || bearerRE.MatchString(s)
}

// redactDetail walks a decoded JSON value. ok is false when the whole value
// must be dropped.
func redactDetail(v any, key string, depth int) (any, bool) {
	if depth > maxDetailDepth {
		return nil, false
	}
	switch x := v.(type) {
	case string:
		if secretText(x, key) {
			return redact(x, key), true
		}
		return x, true
	case []any:
		for i := range x {
			e, ok := redactDetail(x[i], key, depth+1)
			if !ok {
				return nil, false
			}
			x[i] = e
		}
		return x, true
	case map[string]any:
		for name, e := range x {
			if secretText(name, key) {
				return nil, false
			}
			e, ok := redactDetail(e, key, depth+1)
			if !ok {
				return nil, false
			}
			x[name] = e
		}
		return x, true
	default: // json.Number, bool, nil
		return v, true
	}
}

// readStream reads the NDJSON elements of a streaming answer.
func (b *HubBackend) readStream(ctx context.Context, resp *http.Response, key string, onProgress func(json.RawMessage) error) error {
	if mt, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type")); err != nil || mt != "application/x-ndjson" {
		return fmt.Errorf("%w: the Hub answered a streaming op without application/x-ndjson", ErrProtocol)
	}
	r := bufio.NewReaderSize(resp.Body, 64<<10)
	for {
		line, err := readBoundedLine(r, maxStreamLineBytes)
		if err != nil {
			switch {
			case errors.Is(err, io.EOF):
				return nil // the Hub ended the stream
			case errors.Is(err, errLineTooLong):
				return fmt.Errorf("%w: a line of the stream is larger than %d bytes", ErrProtocol, maxStreamLineBytes)
			case ctx.Err() != nil:
				return ctx.Err()
			}
			return fmt.Errorf("%w: %s", ErrConnectionLost, b.reason(err, key))
		}
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue // a keep-alive line: legitimate silence, not an element
		}
		var probe envelope
		if json.Unmarshal(line, &probe) != nil {
			return fmt.Errorf("%w: a line of the stream is not JSON", ErrProtocol)
		}
		if probe.Error != nil {
			if pe := parseEnvelope(line, key); pe != nil {
				return pe
			}
			return fmt.Errorf("%w: the stream ended with an error the client does not know", ErrProtocol)
		}
		if onProgress != nil {
			if err := onProgress(json.RawMessage(append([]byte(nil), line...))); err != nil {
				return err
			}
		}
	}
}

var errLineTooLong = errors.New("line too long")

// readBoundedLine returns the next line without its newline. It returns
// io.EOF at a clean end, io.ErrUnexpectedEOF for a last line without a
// newline (the stream was cut) and errLineTooLong above max, never holding
// more than max bytes.
func readBoundedLine(r *bufio.Reader, max int) ([]byte, error) {
	var line []byte
	for {
		chunk, err := r.ReadSlice('\n')
		if len(line)+len(chunk) > max+1 {
			return nil, errLineTooLong
		}
		line = append(line, chunk...)
		switch {
		case err == nil:
			return line[:len(line)-1], nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case errors.Is(err, io.EOF):
			if len(line) == 0 {
				return nil, io.EOF
			}
			return nil, io.ErrUnexpectedEOF
		default:
			return nil, err
		}
	}
}

// readAPIKey reads the API key from path with the checks of a secret file: a
// regular file, at most maxAPIKeyFileBytes, one line, and the shape of a
// Minion API key. The errors name the path and never the content.
func readAPIKey(path string) (string, error) {
	data, err := readPrivateFile(path, maxAPIKeyFileBytes)
	if err != nil {
		// The text names the path (which a mistyped argument may have filled
		// with a key) and never the content: scrub it like network text.
		return "", fmt.Errorf("%w: %s", ErrAPIKeyFile, scrub(err.Error(), "", maxErrorTextBytes))
	}
	s := string(data)
	if t, ok := strings.CutSuffix(s, "\n"); ok {
		s = strings.TrimSuffix(t, "\r")
	}
	if s == "" {
		return "", fmt.Errorf("%w: %s is empty", ErrAPIKeyFile, scrub(path, "", maxErrorTextBytes))
	}
	if _, err := identity.ParseAPIKey(s); err != nil {
		return "", fmt.Errorf("%w: %s does not hold a Minion API key (mn_...) as its only content", ErrAPIKeyFile, scrub(path, "", maxErrorTextBytes))
	}
	return s, nil
}

// readPrivateFile reads the secret file path of at most max bytes through
// [identity.OpenPrivate]: the path is opened once (a symbolic link, a pipe, a
// device and a directory are refused without blocking) and the checks are made
// on that handle, so there is no window to swap the file. The file must be a
// regular file owned by this user that group and others cannot access (Unix:
// no mode bit of 0077; Windows: a protected DACL that grants nobody else
// access). It is never repaired. The errors name the path, never the content.
func readPrivateFile(path string, max int64) ([]byte, error) {
	f, err := identity.OpenPrivate(path)
	if err != nil {
		var pe *os.PathError
		if errors.As(err, &pe) {
			return nil, fmt.Errorf("cannot read %s: %v", path, pe.Err)
		}
		return nil, err
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("cannot read %s", path)
	}
	if fi.Size() > max {
		return nil, fmt.Errorf("%s is larger than %d bytes", path, max)
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

// readBoundedFile reads a regular file of at most max bytes. A directory, a
// device, a pipe or a larger file is refused before it is read, so a wrong
// path can neither block nor exhaust memory. The errors name the path, never
// the content.
func readBoundedFile(path string, max int64) ([]byte, error) {
	before, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("cannot read %s: %v", path, errors.Unwrap(err))
	}
	if !before.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	if before.Size() > max {
		return nil, fmt.Errorf("%s is larger than %d bytes", path, max)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("cannot read %s: %v", path, errors.Unwrap(err))
	}
	defer func() { _ = f.Close() }()
	after, err := f.Stat()
	if err != nil || !after.Mode().IsRegular() || !os.SameFile(before, after) {
		return nil, fmt.Errorf("%s changed while it was opened", path)
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

var (
	keyShapeRE = regexp.MustCompile(`mn_[A-Za-z0-9]{16,}`)
	bearerRE   = regexp.MustCompile(`(?i)bearer\s+\S+`)
)

// scrub makes text that came from the network safe to return: the API key
// and anything shaped like one or like a Bearer header is removed, control
// sequences cannot reach a terminal, and the length is bounded.
func scrub(s, key string, max int) string {
	return proto.SafeText(redact(s, key), max)
}

// redact removes the key, anything shaped like one and anything shaped like a
// Bearer header from s.
func redact(s, key string) string {
	if key != "" {
		s = strings.ReplaceAll(s, key, "[redacted]")
	}
	s = keyShapeRE.ReplaceAllString(s, "[redacted]")
	return bearerRE.ReplaceAllString(s, "[redacted]")
}

// retryAfter parses the Retry-After seconds, only to tell the user how long
// the Hub asks to wait: the backend itself never retries.
func retryAfter(h http.Header) (time.Duration, bool) {
	n, err := strconv.Atoi(h.Get("Retry-After"))
	if err != nil || n < 0 || n > 86400 {
		return 0, false
	}
	return time.Duration(n) * time.Second, true
}
