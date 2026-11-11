package oidc

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// fakeIdP is an in-process OIDC issuer served over httptest TLS with a private
// CA. It speaks just enough of the protocol for the Hub: discovery, an
// authorization endpoint that redirects straight back (no user interface), a
// token endpoint that checks the PKCE verifier and the client secret, and a
// key set. Fields guarded by mu can be changed between sign-ins.
type fakeIdP struct {
	t   *testing.T
	srv *httptest.Server
	key *rsa.PrivateKey

	// CAFile is the path of a PEM file with the private CA that signed the
	// server certificate.
	CAFile string
	// ClientID and ClientSecret are what the token endpoint accepts.
	ClientID, ClientSecret string

	mu sync.Mutex
	// Claims are added to the ID token of the next sign-ins (on top of iss,
	// sub, aud, exp, iat and nonce).
	claims map[string]any
	// Subject is the sub claim.
	subject string
	// OmitIDToken makes the token endpoint answer without an id_token.
	omitIDToken bool
	// WrongNonce makes the ID token carry another nonce than the request.
	wrongNonce bool
	// RefreshToken is returned by the token endpoint when not empty.
	refreshToken string
	// TokenDelay delays the token endpoint.
	tokenDelay time.Duration
	// DiscoveryDelay delays the discovery document.
	discoveryDelay time.Duration
	// FailToken makes the token endpoint answer 500.
	failToken bool
	// TokenURL, when not empty, replaces the token endpoint of the discovery
	// document.
	tokenURL string

	codes     map[string]authRequest
	tokenHits int
}

type authRequest struct {
	nonce, challenge, redirectURI string
}

func newFakeIdP(t *testing.T) *fakeIdP {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeIdP{
		t: t, key: key, ClientID: "minion", ClientSecret: "s3cret-for-tests",
		subject: "user-1", refreshToken: "rt-default", codes: map[string]authRequest{}, claims: map[string]any{},
	}
	ca, caKey := newCA(t)
	leaf := newLeaf(t, ca, caKey)
	f.CAFile = filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(f.CAFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.Raw}), 0o600); err != nil {
		t.Fatal(err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", f.discovery)
	mux.HandleFunc("GET /authorize", f.authorize)
	mux.HandleFunc("POST /token", f.token)
	mux.HandleFunc("GET /jwks", f.jwks)
	f.srv = httptest.NewUnstartedServer(mux)
	f.srv.TLS = &tls.Config{Certificates: []tls.Certificate{leaf}, MinVersion: tls.VersionTLS12}
	f.srv.StartTLS()
	t.Cleanup(f.srv.Close)
	return f
}

// Issuer is the issuer URL.
func (f *fakeIdP) Issuer() string { return f.srv.URL }

func (f *fakeIdP) set(fn func(f *fakeIdP)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

func (f *fakeIdP) discovery(w http.ResponseWriter, _ *http.Request) {
	f.mu.Lock()
	d := f.discoveryDelay
	f.mu.Unlock()
	time.Sleep(d)
	f.mu.Lock()
	tokenURL := f.tokenURL
	f.mu.Unlock()
	if tokenURL == "" {
		tokenURL = f.Issuer() + "/token"
	}
	writeJSON(w, map[string]any{
		"issuer":                                f.Issuer(),
		"authorization_endpoint":                f.Issuer() + "/authorize",
		"token_endpoint":                        tokenURL,
		"jwks_uri":                              f.Issuer() + "/jwks",
		"id_token_signing_alg_values_supported": []string{"RS256"},
		"response_types_supported":              []string{"code"},
		"subject_types_supported":               []string{"public"},
		"code_challenge_methods_supported":      []string{"S256"},
	})
}

func (f *fakeIdP) authorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if q.Get("client_id") != f.ClientID || q.Get("response_type") != "code" ||
		q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" ||
		q.Get("state") == "" || q.Get("nonce") == "" || q.Get("redirect_uri") == "" {
		http.Error(w, "bad authorization request", http.StatusBadRequest)
		return
	}
	code := randomHex()
	f.mu.Lock()
	f.codes[code] = authRequest{nonce: q.Get("nonce"), challenge: q.Get("code_challenge"), redirectURI: q.Get("redirect_uri")}
	f.mu.Unlock()
	u, _ := url.Parse(q.Get("redirect_uri"))
	v := u.Query()
	v.Set("code", code)
	v.Set("state", q.Get("state"))
	u.RawQuery = v.Encode()
	http.Redirect(w, r, u.String(), http.StatusFound)
}

func (f *fakeIdP) token(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.tokenHits++
	delay, fail := f.tokenDelay, f.failToken
	f.mu.Unlock()
	time.Sleep(delay)
	if fail {
		http.Error(w, "boom", http.StatusInternalServerError)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	id, secret, ok := r.BasicAuth()
	if !ok {
		id, secret = r.PostForm.Get("client_id"), r.PostForm.Get("client_secret")
	}
	if u, err := url.QueryUnescape(id); err == nil {
		id = u
	}
	if u, err := url.QueryUnescape(secret); err == nil {
		secret = u
	}
	if id != f.ClientID || secret != f.ClientSecret {
		w.WriteHeader(http.StatusUnauthorized)
		writeJSON(w, map[string]string{"error": "invalid_client"})
		return
	}
	f.mu.Lock()
	req, found := f.codes[r.PostForm.Get("code")]
	delete(f.codes, r.PostForm.Get("code")) // a code is single use
	f.mu.Unlock()
	sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
	if !found || r.PostForm.Get("redirect_uri") != req.redirectURI ||
		base64.RawURLEncoding.EncodeToString(sum[:]) != req.challenge {
		w.WriteHeader(http.StatusBadRequest)
		writeJSON(w, map[string]string{"error": "invalid_grant"})
		return
	}
	f.mu.Lock()
	resp := map[string]any{"access_token": "at-" + randomHex(), "token_type": "Bearer", "expires_in": 300}
	if f.refreshToken != "" {
		resp["refresh_token"] = f.refreshToken
	}
	if !f.omitIDToken {
		nonce := req.nonce
		if f.wrongNonce {
			nonce = "another-nonce"
		}
		resp["id_token"] = f.signIDToken(nonce, f.subject, f.claims)
	}
	f.mu.Unlock()
	writeJSON(w, resp)
}

func (f *fakeIdP) jwks(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]any{"keys": []map[string]string{{
		"kty": "RSA", "alg": "RS256", "use": "sig", "kid": "k1",
		"n": b64(f.key.N.Bytes()), "e": b64(big.NewInt(int64(f.key.E)).Bytes()),
	}}})
}

// signIDToken returns a signed RS256 ID token. The caller holds mu.
func (f *fakeIdP) signIDToken(nonce, sub string, extra map[string]any) string {
	return f.signWith(f.key, map[string]any{"iss": f.Issuer(), "aud": f.ClientID}, nonce, sub, extra)
}

func (f *fakeIdP) signWith(key *rsa.PrivateKey, base map[string]any, nonce, sub string, extra map[string]any) string {
	claims := map[string]any{"sub": sub, "nonce": nonce,
		"iat": time.Now().Unix(), "exp": time.Now().Add(5 * time.Minute).Unix()}
	for k, v := range base {
		claims[k] = v
	}
	for k, v := range extra {
		claims[k] = v
	}
	header, _ := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT", "kid": "k1"})
	payload, _ := json.Marshal(claims)
	signing := b64(header) + "." + b64(payload)
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		f.t.Fatal(err)
	}
	return signing + "." + b64(sig)
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func randomHex() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return b64(b)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func newCA(t *testing.T) (*x509.Certificate, *ecdsa.PrivateKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Minion test private CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return cert, key
}

func newLeaf(t *testing.T, ca *x509.Certificate, caKey *ecdsa.PrivateKey) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "idp.test"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, DNSNames: []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}
