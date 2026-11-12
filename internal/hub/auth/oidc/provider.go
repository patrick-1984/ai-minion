package oidc

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"

	gooidc "github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/logx"
)

// DefaultTimeout is the deadline of every OIDC network call when a provider
// sets none (AD-10).
const DefaultTimeout = config.DefaultOIDCTimeout

// maxCAFileSize bounds the PEM file of a private CA.
const maxCAFileSize = 1 << 20

// Provider sources, reported next to a [Config].
const (
	SourceSettings = "settings"
	SourcePanel    = "panel"
)

// Config describes one OIDC provider: either the one that the environment or
// the settings name, or one stored in the panel. It is plain data so that the
// logic around it is testable without a network.
type Config struct {
	// ID is the short id used in sign-in URLs.
	ID string
	// Name is the label after "Sign in with" on the login page.
	Name   string
	Source string
	Issuer string

	ClientID string
	// ClientSecret is empty for a public client. It is redacted in logs.
	ClientSecret logx.Secret

	// RoleClaim is the one ID token claim that is read for roles; the rules
	// map its values to roles.
	RoleClaim   string
	AdminValues []string
	UserValues  []string

	// Scopes defaults to "openid profile email".
	Scopes []string
	// CAFile is the path of a PEM file with the private CA of the IdP.
	CAFile string
	// InsecureSkipVerify turns TLS verification of the IdP off (R96).
	InsecureSkipVerify bool
	// Timeout is the deadline of each network call; 0 means [DefaultTimeout].
	Timeout time.Duration
	// Disabled hides the provider and refuses its sign-ins.
	Disabled bool
}

// Rules returns the role rules of the provider.
func (c Config) Rules() Rules {
	return Rules{Claim: c.RoleClaim, Admin: c.AdminValues, User: c.UserValues}
}

func (c Config) timeout() time.Duration {
	if c.Timeout > 0 {
		return c.Timeout
	}
	return DefaultTimeout
}

func (c Config) scopes() []string {
	if len(c.Scopes) > 0 {
		return c.Scopes
	}
	return strings.Fields(config.DefaultOIDCScopes)
}

// Validate checks that the provider can be used.
func (c Config) Validate() error {
	if c.ID == "" {
		return errors.New("oidc: provider has no id")
	}
	u, err := url.Parse(c.Issuer)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return fmt.Errorf("oidc: provider %q: issuer must be an https URL", c.ID)
	}
	if u.Scheme != "https" && !c.InsecureSkipVerify {
		// Cleartext would expose the client secret and let anyone on the path
		// substitute the signing keys. Only the explicit insecure flag, which
		// warns permanently (R96), allows it.
		return fmt.Errorf("oidc: provider %q: issuer must be an https URL (plain http needs the insecure TLS flag and is for development only)", c.ID)
	}
	if c.ClientID == "" {
		return fmt.Errorf("oidc: provider %q: client id is required", c.ID)
	}
	if !slices.Contains(c.scopes(), "openid") {
		return fmt.Errorf("oidc: provider %q: scopes must include openid", c.ID)
	}
	return nil
}

// fingerprint identifies the parts of the configuration that a discovered
// [Provider] depends on, so a cached one is replaced when any of them
// changes. It is a digest, never the secret itself.
func (c Config) fingerprint() string {
	h := sha256.New()
	for _, s := range []string{c.Issuer, c.ClientID, c.ClientSecret.Reveal(), c.CAFile, fmt.Sprint(c.InsecureSkipVerify), c.timeout().String(), strings.Join(c.scopes(), " ")} {
		_, _ = fmt.Fprintf(h, "%d:%s;", len(s), s)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// DefaultProviderID is the id of the provider that the environment or the
// settings describe (MINION_OIDC_*).
const DefaultProviderID = "default"

// FromSettings returns the provider that the resolved settings describe, and
// false when no issuer is set (OIDC login is off).
func FromSettings(h config.HubOIDC) (Config, bool) {
	if !h.Configured() {
		return Config{}, false
	}
	return Config{
		ID: DefaultProviderID, Name: h.Name, Source: SourceSettings,
		Issuer: h.Issuer, ClientID: h.ClientID, ClientSecret: logx.Secret(h.ClientSecret),
		RoleClaim: h.RoleClaim, AdminValues: h.AdminValues, UserValues: h.UserValues,
		Scopes: h.Scopes, CAFile: h.CAFile, InsecureSkipVerify: h.InsecureSkipVerify, Timeout: h.Timeout,
	}, true
}

// FromRow returns the provider of a panel row with its role rules. The panel
// has no timeout field: the deadline is [DefaultTimeout] unless timeout is
// positive (the oidc_timeout setting).
func FromRow(p store.OIDCProviderRow, rules []store.RoleRuleRow, timeout time.Duration) Config {
	c := Config{
		ID: p.ID, Name: p.Name, Source: SourcePanel, Issuer: p.Issuer, ClientID: p.ClientID,
		ClientSecret: logx.Secret(p.ClientSecret), RoleClaim: p.RoleClaim,
		Scopes: strings.Fields(p.Scopes), CAFile: p.CAFile, InsecureSkipVerify: p.InsecureSkipVerify,
		Timeout: timeout, Disabled: !p.Enabled,
	}
	for _, r := range rules {
		if r.ProviderID != p.ID {
			continue
		}
		switch r.Role {
		case RoleAdmin:
			c.AdminValues = append(c.AdminValues, r.Value)
		case RoleUser:
			c.UserValues = append(c.UserValues, r.Value)
		}
	}
	return c
}

// ConfigSource yields the providers that are configured now. The service asks
// for them on every request, so a panel change applies without a restart.
type ConfigSource interface {
	OIDCProviders(ctx context.Context) ([]Config, error)
}

// Identity is a verified sign-in: who the IdP says the user is.
type Identity struct {
	// Issuer and Subject identify the account at the IdP.
	Issuer, Subject string
	// Claims holds every claim of the ID token. Only the configured role claim
	// and preferred_username are ever read from it.
	Claims map[string]any
}

// Tokens are what the token endpoint returned that the Hub uses.
type Tokens struct {
	// IDToken is the raw ID token; a response without one is refused before a
	// Tokens is built.
	IDToken string
	// RefreshToken is empty when the IdP issued none.
	RefreshToken logx.Secret
}

// Provider is a discovered OIDC provider: endpoints, key set and an HTTP
// client with the configured trust and deadline. It is safe for concurrent
// use.
type Provider struct {
	cfg      Config
	op       *gooidc.Provider
	verifier *gooidc.IDTokenVerifier
	client   *http.Client
}

// Config returns the configuration the provider was discovered with.
func (p *Provider) Config() Config { return p.cfg }

// HTTPClient builds the client for the calls to an IdP: every call is bounded
// by the provider's timeout, and TLS trusts the system roots plus the CA file,
// or nothing at all when InsecureSkipVerify is set (R96).
func HTTPClient(cfg Config) (*http.Client, error) {
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if cfg.CAFile != "" {
		pem, err := readCAFile(cfg.CAFile)
		if err != nil {
			return nil, err
		}
		pool, err := x509.SystemCertPool()
		if err != nil || pool == nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("oidc: CA file %q holds no PEM certificate", cfg.CAFile)
		}
		tlsCfg.RootCAs = pool
	}
	if cfg.InsecureSkipVerify {
		tlsCfg.InsecureSkipVerify = true //nolint:gosec // the explicit, warned-about flag of R96
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = tlsCfg
	tr.Proxy = nil // the binary dials only the IdP it is configured for (AD-19)
	return &http.Client{
		Transport: tr,
		Timeout:   cfg.timeout(),
		// An IdP endpoint that redirects is not part of the protocol.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}, nil
}

func readCAFile(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("oidc: open CA file: %w", err)
	}
	defer func() { _ = f.Close() }()
	pem, err := io.ReadAll(io.LimitReader(f, maxCAFileSize+1))
	if err != nil {
		return nil, fmt.Errorf("oidc: read CA file %q: %w", path, err)
	}
	if len(pem) > maxCAFileSize {
		return nil, fmt.Errorf("oidc: CA file %q is larger than %d bytes", path, maxCAFileSize)
	}
	return pem, nil
}

// Discover fetches the discovery document of cfg.Issuer under cfg's deadline
// and returns the provider. A failure says what failed and changes nothing.
func Discover(ctx context.Context, cfg Config) (*Provider, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	client, err := HTTPClient(cfg)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(gooidc.ClientContext(ctx, client), cfg.timeout())
	defer cancel()
	op, err := gooidc.NewProvider(ctx, cfg.Issuer)
	if err != nil {
		return nil, fmt.Errorf("oidc: discovery at %s failed: %w", cfg.Issuer, err)
	}
	if err := checkEndpoints(cfg, op); err != nil {
		return nil, err
	}
	return &Provider{
		cfg:      cfg,
		op:       op,
		client:   client,
		verifier: op.Verifier(&gooidc.Config{ClientID: cfg.ClientID}),
	}, nil
}

// checkEndpoints refuses a discovery document whose authorization, token or
// key-set endpoint is not https, before any credential is sent to it. The
// insecure flag lifts the rule, as it does for the issuer.
func checkEndpoints(cfg Config, op *gooidc.Provider) error {
	if cfg.InsecureSkipVerify {
		return nil
	}
	var doc struct {
		JWKSURI string `json:"jwks_uri"`
	}
	if err := op.Claims(&doc); err != nil {
		return fmt.Errorf("oidc: discovery at %s: %w", cfg.Issuer, err)
	}
	ep := op.Endpoint()
	for _, e := range []struct{ name, raw string }{
		{"authorization_endpoint", ep.AuthURL}, {"token_endpoint", ep.TokenURL}, {"jwks_uri", doc.JWKSURI},
	} {
		u, err := url.Parse(e.raw)
		if err != nil || u.Scheme != "https" || u.Host == "" {
			return fmt.Errorf("oidc: discovery at %s: %s is not an https URL", cfg.Issuer, e.name)
		}
	}
	return nil
}

func (p *Provider) oauth(redirectURL string) *oauth2.Config {
	return &oauth2.Config{
		ClientID:     p.cfg.ClientID,
		ClientSecret: p.cfg.ClientSecret.Reveal(),
		Endpoint:     p.op.Endpoint(),
		RedirectURL:  redirectURL,
		Scopes:       p.cfg.scopes(),
	}
}

// AuthCodeURL returns the authorization endpoint URL for one sign-in: the
// authorization-code flow with PKCE (S256), the state and the nonce.
func (p *Provider) AuthCodeURL(redirectURL, state, nonce, codeVerifier string) string {
	return p.oauth(redirectURL).AuthCodeURL(state,
		oauth2.S256ChallengeOption(codeVerifier), gooidc.Nonce(nonce))
}

// AuthCodeURLReauth is [Provider.AuthCodeURL] for a re-authentication: it asks
// the IdP to authenticate the user again (max_age=0, prompt=login). Whether
// the IdP did is judged by the auth_time claim, never by this request.
func (p *Provider) AuthCodeURLReauth(redirectURL, state, nonce, codeVerifier string) string {
	return p.oauth(redirectURL).AuthCodeURL(state,
		oauth2.S256ChallengeOption(codeVerifier), gooidc.Nonce(nonce),
		oauth2.SetAuthURLParam("max_age", "0"), oauth2.SetAuthURLParam("prompt", "login"))
}

// Exchange trades an authorization code for tokens under the provider's
// deadline. A response without an ID token is an error: the ID token is
// required (AD-10).
func (p *Provider) Exchange(ctx context.Context, redirectURL, code, codeVerifier string) (Tokens, error) {
	ctx, cancel := context.WithTimeout(gooidc.ClientContext(ctx, p.client), p.cfg.timeout())
	defer cancel()
	tok, err := p.oauth(redirectURL).Exchange(ctx, code, oauth2.VerifierOption(codeVerifier))
	if err != nil {
		return Tokens{}, fmt.Errorf("oidc: code exchange failed: %w", sanitizeExchangeError(err))
	}
	raw, _ := tok.Extra("id_token").(string)
	if raw == "" {
		return Tokens{}, ErrNoIDToken
	}
	return Tokens{IDToken: raw, RefreshToken: logx.Secret(tok.RefreshToken)}, nil
}

// sanitizeExchangeError keeps the cause of a failed exchange without the
// response body, which an IdP may fill with details about the request.
func sanitizeExchangeError(err error) error {
	var re *oauth2.RetrieveError
	if errors.As(err, &re) {
		if re.ErrorCode != "" {
			return fmt.Errorf("the token endpoint answered %d (%s)", re.Response.StatusCode, re.ErrorCode)
		}
		return fmt.Errorf("the token endpoint answered %d", re.Response.StatusCode)
	}
	return err
}

// Errors of the sign-in, matched with [errors.Is].
var (
	// ErrNoIDToken is returned when the token response holds no ID token.
	ErrNoIDToken = errors.New("oidc: the token response has no ID token")
	// ErrNonceMismatch is returned when the ID token carries another nonce
	// than the sign-in sent.
	ErrNonceMismatch = errors.New("oidc: ID token nonce does not match the sign-in")
	// ErrInvalidIDToken is returned for an ID token that fails verification.
	ErrInvalidIDToken = errors.New("oidc: ID token is not valid")
)

// VerifyIDToken verifies the signature, issuer, audience and expiry of raw
// and that its nonce is wantNonce, and returns the identity it names. The
// subject is required.
func (p *Provider) VerifyIDToken(ctx context.Context, raw, wantNonce string) (Identity, error) {
	ctx, cancel := context.WithTimeout(gooidc.ClientContext(ctx, p.client), p.cfg.timeout())
	defer cancel()
	tok, err := p.verifier.Verify(ctx, raw)
	if err != nil {
		return Identity{}, fmt.Errorf("%w: %w", ErrInvalidIDToken, err)
	}
	if wantNonce == "" || subtle.ConstantTimeCompare([]byte(tok.Nonce), []byte(wantNonce)) != 1 {
		return Identity{}, ErrNonceMismatch
	}
	if tok.Subject == "" {
		return Identity{}, fmt.Errorf("%w: it has no subject", ErrInvalidIDToken)
	}
	claims := map[string]any{}
	if err := tok.Claims(&claims); err != nil {
		return Identity{}, fmt.Errorf("%w: claims: %w", ErrInvalidIDToken, err)
	}
	return Identity{Issuer: tok.Issuer, Subject: tok.Subject, Claims: claims}, nil
}
