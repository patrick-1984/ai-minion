package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/hub/adminsock"
	"github.com/patrick-1984/ai-minion/internal/hub/audit"
	"github.com/patrick-1984/ai-minion/internal/hub/auth/session"
	"github.com/patrick-1984/ai-minion/internal/hub/hubid"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

const (
	idPass1  = "correct horse battery staple"
	idPass2  = "another long pass phrase!!"
	idCanary = "canary-PASS-9f3a1c77-longer-than-twelve"
	idWrong  = "wrong-canary-PASS-5521-also-long"
)

// idFakeSealer is a fast stand-in for the age sealer (one real derivation
// costs about a second): the file is "FKS1" + SHA-256(passphrase) + seed.
type idFakeSealer struct {
	mu    sync.Mutex
	seeds map[string][]byte
}

func newIDFakeSealer() *idFakeSealer { return &idFakeSealer{seeds: map[string][]byte{}} }

func (f *idFakeSealer) Seal(seed []byte, pass string) ([]byte, error) {
	h := sha256.Sum256([]byte(pass))
	k, _ := identity.KeyFromSeed(seed)
	f.mu.Lock()
	f.seeds[string(k.Public())] = bytes.Clone(seed)
	f.mu.Unlock()
	return append(append([]byte("FKS1"), h[:]...), seed...), nil
}

func (f *idFakeSealer) Reseal(s *identity.RootSigner, pass string) ([]byte, error) {
	f.mu.Lock()
	seed := f.seeds[string(s.Public())]
	f.mu.Unlock()
	return f.Seal(seed, pass)
}

func (f *idFakeSealer) Open(sealed []byte, pass string) (*identity.RootSigner, error) {
	if len(sealed) != 4+32+32 || string(sealed[:4]) != "FKS1" {
		return nil, identity.ErrSealedRootInvalid
	}
	h := sha256.Sum256([]byte(pass))
	if !bytes.Equal(sealed[4:36], h[:]) {
		return nil, identity.ErrWrongPassphrase
	}
	k, _ := identity.KeyFromSeed(sealed[36:])
	f.mu.Lock()
	f.seeds[string(k.Public())] = bytes.Clone(sealed[36:])
	f.mu.Unlock()
	return identity.RootSignerFromSeed(sealed[36:])
}

func (f *idFakeSealer) Dummy(string) {}

// idHub is a Hub with the real identity module behind the REST dispatcher.
type idHub struct {
	*apiHub
	svc *hubid.Service
	dir string
	// pass is set after bootstrap.
}

type idHubOpts struct {
	dir     string
	cfg     func(*Config)
	options func(*hubid.Options)
}

// shortDir returns a data directory whose admin socket path fits a Unix socket.
func shortDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "mn")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func startIdentityHub(t *testing.T, o idHubOpts) *idHub {
	t.Helper()
	t.Setenv("MINION_ADMIN_USERNAME", "root")
	t.Setenv("MINION_ADMIN_PASSWORD", "identity-integration-pw-1")
	sealer := newIDFakeSealer()
	identityOptions = func(opts *hubid.Options) {
		opts.Sealer = sealer
		opts.LockWait = 500 * time.Millisecond
		if o.options != nil {
			o.options(opts)
		}
	}
	t.Cleanup(func() { identityOptions = nil })

	ih := &idHub{apiHub: &apiHub{workers: fakeWorkers{}}, dir: o.dir}
	if ih.dir == "" {
		ih.dir = shortDir(t)
	}
	cat := ops.NewRegistry()
	for _, n := range []string{ops.OpHubIdentityStatus, ops.OpHubIdentitySetPassphrase, ops.OpHubIdentityReissue,
		ops.OpHubIdentityRotate, ops.OpHubIdentityChangePassphrase, ops.OpAPIKeyCreate, ops.OpAPIKeyList, ops.OpAPIKeyRevoke} {
		op, ok := ops.Lookup(n)
		if !ok {
			t.Fatalf("no op %s", n)
		}
		cat.Register(op)
	}
	r := NewRegistry()
	for _, err := range []error{
		registerCore(r),
		registerStore(r),
		r.Register(settingsModuleName, settingsModuleOrder, initSettingsModule,
			DeclaresService[*SettingsService](), DeclaresService[config.PanelSource]()),
	} {
		if err != nil {
			t.Fatal(err)
		}
	}
	registerAuditModule(r)
	registerAuthz(r)
	registerAuth(r)
	registerAPI(r, cat)
	registerAPIKeyModule(r)
	registerHubIDModule(r)
	registerHubIdentityModule(r)
	err := r.Register("grab", hubIdentityModuleOrder+1, func(_ context.Context, env *Env) error {
		var err error
		if ih.st, err = Require[*store.Store](env); err != nil {
			return err
		}
		if ih.audit, err = Require[*audit.Log](env); err != nil {
			return err
		}
		if ih.mgr, err = Require[*session.Manager](env); err != nil {
			return err
		}
		ih.svc, err = Require[*hubid.Service](env)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg := &Config{DataDir: ih.dir}
	if o.cfg != nil {
		o.cfg(cfg)
	}
	ih.h = startHubPristine(t, cfg, r)
	scheme := "https"
	if cfg.TLSOff {
		scheme = "http"
	}
	ih.hc = newHubClient(t, ih.h, scheme)
	return ih
}

// sessionAt starts a session of the user whose last credential check was age ago
// (negative age: never checked).
func (ih *idHub) sessionAt(t *testing.T, ident string, age time.Duration) (tok, csrf string) {
	t.Helper()
	row, err := ih.st.UserByIdentifier(context.Background(), ident)
	if err != nil {
		t.Fatal(err)
	}
	var auth session.Auth
	if age >= 0 {
		auth = session.Auth{At: time.Now().Add(-age), Method: session.AuthMethodLocal}
	}
	is, err := ih.mgr.CreateAuth(context.Background(), row, time.Hour, auth)
	if err != nil {
		t.Fatal(err)
	}
	return is.Token, is.CSRFToken
}

func (ih *idHub) fresh(t *testing.T, ident string) map[string]string {
	tok, csrf := ih.sessionAt(t, ident, time.Second)
	return ih.browser(tok, csrf)
}

// browser returns the headers a same-origin browser sends with a session.
func (ih *idHub) browser(tok, csrf string) map[string]string {
	h := asSession(tok, csrf)
	h["Origin"] = ih.hc.base
	h["Sec-Fetch-Site"] = "same-origin"
	return h
}

func (ih *idHub) op(t *testing.T, name string, body any, headers map[string]string) apiResp {
	t.Helper()
	return ih.call(t, "POST", "/api/v1/ops/"+name, body, headers)
}

func (ih *idHub) status(t *testing.T) ops.HubIdentityStatusResult {
	t.Helper()
	r := ih.op(t, ops.OpHubIdentityStatus, map[string]any{}, ih.fresh(t, "root"))
	if r.code != 200 {
		t.Fatalf("status = %d %s", r.code, r.body)
	}
	var st ops.HubIdentityStatusResult
	if err := json.Unmarshal([]byte(r.body), &st); err != nil {
		t.Fatal(err)
	}
	return st
}

// bootstrap sets the passphrase through the REST operation and returns its result.
func (ih *idHub) bootstrap(t *testing.T, pass string, days int) ops.HubIdentityResult {
	t.Helper()
	r := ih.op(t, ops.OpHubIdentitySetPassphrase, map[string]any{"passphrase": pass, "confirm": pass, "validity_days": days}, ih.fresh(t, "root"))
	if r.code != 200 {
		t.Fatalf("set passphrase = %d %s", r.code, r.body)
	}
	var res ops.HubIdentityResult
	if err := json.Unmarshal([]byte(r.body), &res); err != nil {
		t.Fatal(err)
	}
	return res
}

func (ih *idHub) auditRows(t *testing.T, action string) []store.AuditRow {
	t.Helper()
	rows, err := ih.st.ListAudit(context.Background(), store.AuditQuery{Action: action, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

// socket sends a request to the local admin socket of the Hub.
func (ih *idHub) socket(t *testing.T, method, path string, body any) (int, string) {
	t.Helper()
	sock := adminsock.Path(ih.dir)
	c := &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{
		DisableKeepAlives: true,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", sock)
		},
	}}
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, "http://hub"+path, rd)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestHubIdentityModuleIsRegisteredInTheDefaultRegistry(t *testing.T) {
	found := false
	for _, n := range DefaultRegistry.Names() {
		found = found || n == "hub_identity"
	}
	if !found {
		t.Errorf("modules = %v", DefaultRegistry.Names())
	}
}

func TestHubIdentityModuleNamesMissingServices(t *testing.T) {
	r := NewRegistry()
	registerHubIdentityModule(r)
	err := NewKernel(r).Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), `module "hub_identity"`) || !strings.Contains(err.Error(), "requires service") {
		t.Errorf("err = %v", err)
	}
}

// A pristine Hub (B0) creates nothing; the first passphrase creates the identity.
func TestAnAdminSetsThePassphraseAndTheStatusFollows(t *testing.T) {
	ih := startIdentityHub(t, idHubOpts{})
	if entries, _ := os.ReadDir(ih.dir); len(entries) > 0 {
		for _, e := range entries {
			if e.Name() == hubid.RootFile || e.Name() == hubid.OpFile || e.Name() == hubid.PlainFile {
				t.Errorf("B0 created %s", e.Name())
			}
		}
	}
	st := ih.status(t)
	if st.State != "B0" || st.PassphraseSet || st.Serving {
		t.Fatalf("status before = %+v", st)
	}
	res := ih.bootstrap(t, idPass1, 90)
	if res.Kind != "passphrase_set" || res.Serial == 0 || res.RootFingerprint == "" || res.OpFingerprint == "" {
		t.Fatalf("result = %+v", res)
	}
	st = ih.status(t)
	if st.State != "B3" || !st.PassphraseSet || !st.Serving || st.RootFingerprint != res.RootFingerprint || st.DaysLeft < 89 || st.Warning != "" {
		t.Errorf("status after = %+v", st)
	}
	admin, _ := ih.st.UserByIdentifier(context.Background(), "root")
	rows := ih.auditRows(t, "hub_identity.passphrase_set")
	if len(rows) != 1 || rows[0].Actor != admin.ID || rows[0].Outcome != "success" || !strings.Contains(rows[0].Target, "channel=ui") {
		t.Errorf("audit = %+v", rows)
	}
}

// The authorization matrix of contract section 8.1 for the four operations.
func TestIdentityOperationsAuthorizationMatrix(t *testing.T) {
	ih := startIdentityHub(t, idHubOpts{})
	ih.bootstrap(t, idPass1, 90)
	ih.user(t, "alice")
	reissue := map[string]any{"passphrase": idPass1}
	before := ih.status(t)

	// A non-admin, even with a fresh sign-in, gets forbidden.
	aTok, aCSRF := ih.sessionAt(t, "alice", time.Second)
	if r := ih.op(t, ops.OpHubIdentityReissue, reissue, ih.browser(aTok, aCSRF)); r.code != 403 || r.err.Code != proto.CodeForbidden {
		t.Errorf("non-admin = %d %s", r.code, r.body)
	}
	// An API key, whatever its scope, may not perform them; the status op is
	// for an everything key only.
	adminTok, adminCSRF := ih.sessionAt(t, "root", time.Second)
	every := []map[string]any{{"kind": "everything"}}
	key, _ := ih.newKey(t, ih.browser(adminTok, adminCSRF), map[string]any{"name": "manage", "resources": every, "actions": []string{"manage"}})
	for _, name := range []string{ops.OpHubIdentitySetPassphrase, ops.OpHubIdentityReissue, ops.OpHubIdentityRotate, ops.OpHubIdentityChangePassphrase} {
		r := ih.op(t, name, map[string]any{"passphrase": idPass1, "new_passphrase": idPass2, "confirm": idPass2, "word": "ROTATE"}, asKey(key))
		if r.code != 403 || r.err.Code != proto.CodeForbidden {
			t.Errorf("API key %s = %d %s", name, r.code, r.body)
		}
	}
	if r := ih.op(t, ops.OpHubIdentityStatus, map[string]any{}, asKey(key)); r.code != 200 {
		t.Errorf("status with an everything key = %d %s", r.code, r.body)
	}
	// A stale session (never authenticated; authenticated six minutes ago).
	for name, age := range map[string]time.Duration{"never checked": -1, "six minutes": 6 * time.Minute} {
		tok, csrf := ih.sessionAt(t, "root", age)
		r := ih.op(t, ops.OpHubIdentityReissue, reissue, ih.browser(tok, csrf))
		if r.code != 403 || r.err.Code != proto.CodeReauthRequired {
			t.Errorf("stale session (%s) = %d %s", name, r.code, r.body)
		}
	}
	// Nothing changed so far.
	if after := ih.status(t); after.Serial != before.Serial || after.Web.Failures != 0 {
		t.Errorf("a refused call changed the identity: %+v -> %+v", before, after)
	}
	// Four minutes old is fresh; no request field or header can make a stale session fresh.
	tok, csrf := ih.sessionAt(t, "root", 4*time.Minute)
	if r := ih.op(t, ops.OpHubIdentityReissue, reissue, ih.browser(tok, csrf)); r.code != 200 {
		t.Errorf("four-minute-old session = %d %s", r.code, r.body)
	}
	tok, csrf = ih.sessionAt(t, "root", 6*time.Minute)
	h := ih.browser(tok, csrf)
	h["X-Auth-At"] = fmt.Sprint(time.Now().UnixMilli())
	forged := map[string]any{"passphrase": idPass1, "auth_at": time.Now().UnixMilli(), "auth_method": "local"}
	if r := ih.op(t, ops.OpHubIdentityReissue, forged, h); r.code != 403 || r.err.Code != proto.CodeReauthRequired {
		t.Errorf("forged auth_at = %d %s", r.code, r.body)
	}
	// A fresh admin session performs each of the four operations.
	good := ih.fresh(t, "root")
	for _, c := range []struct {
		op   string
		body map[string]any
		kind string
	}{
		{ops.OpHubIdentityReissue, map[string]any{"passphrase": idPass1, "validity_days": 100}, "reissue"},
		{ops.OpHubIdentityChangePassphrase, map[string]any{"passphrase": idPass1, "new_passphrase": idPass2, "confirm": idPass2}, "passphrase_change"},
		{ops.OpHubIdentityRotate, map[string]any{"passphrase": idPass2, "new_passphrase": idPass1, "confirm": idPass1, "word": "ROTATE"}, "rotate"},
	} {
		r := ih.op(t, c.op, c.body, good)
		if r.code != 200 || !strings.Contains(r.body, `"kind":"`+c.kind+`"`) {
			t.Fatalf("%s = %d %s", c.op, r.code, r.body)
		}
	}
	// The rotation revoked the invitations and says so.
	// Set-passphrase is refused once a sealed root exists (B3).
	r := ih.op(t, ops.OpHubIdentitySetPassphrase, map[string]any{"passphrase": idPass2, "confirm": idPass2}, good)
	if r.code != 409 || r.err.Code != proto.CodeConflict {
		t.Errorf("set-passphrase in B3 = %d %s", r.code, r.body)
	}
	// The rotation word is checked.
	r = ih.op(t, ops.OpHubIdentityRotate, map[string]any{"passphrase": idPass1, "new_passphrase": idPass2, "confirm": idPass2, "word": "rotate"}, good)
	if r.code != 400 || r.err.Code != proto.CodeInvalidArgument {
		t.Errorf("rotate with a wrong word = %d %s", r.code, r.body)
	}
	// The audit trail carries the admin as actor on channel ui.
	admin, _ := ih.st.UserByIdentifier(context.Background(), "root")
	for _, a := range []string{"hub_identity.reissue", "hub_identity.passphrase_change", "hub_identity.rotate"} {
		rows := ih.auditRows(t, a)
		if len(rows) == 0 || rows[0].Actor != admin.ID || !strings.Contains(rows[0].Target, "channel=ui") {
			t.Errorf("audit %s = %+v", a, rows)
		}
	}
}

// Set-passphrase in B2 (a sealed root is recorded, only the certificate is
// missing) is a conflict that reads, unseals, writes and counts nothing.
func TestSetPassphraseIsRefusedInB2(t *testing.T) {
	dir := shortDir(t)
	sealer := newIDFakeSealer()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	crashed, err := hubid.Open(context.Background(), hubid.Options{
		DataDir: dir, Store: st, Sealer: sealer,
		Hooks: hubid.Hooks{Point: func(n string) error {
			if n == hubid.PointBSPlainGone {
				return hubid.ErrCrash
			}
			return nil
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := crashed.SetPassphrase(context.Background(), hubid.Actor{ID: "x", Channel: hubid.ChannelUI}, idPass1, idPass1, hubid.IssueOptions{}); err == nil {
		t.Fatal("the simulated crash did not happen")
	}
	_ = st.Close()

	ih := startIdentityHub(t, idHubOpts{dir: dir})
	if s := ih.status(t); s.State != "B2" || s.Serving {
		t.Fatalf("status = %+v, want B2", s)
	}
	h := ih.fresh(t, "root")
	r := ih.op(t, ops.OpHubIdentitySetPassphrase, map[string]any{"passphrase": idWrong, "confirm": idWrong}, h)
	if r.code != 409 || r.err.Code != proto.CodeConflict || !strings.Contains(r.err.Message, "re-issue") {
		t.Errorf("set-passphrase in B2 = %d %s", r.code, r.body)
	}
	if s := ih.status(t); s.Web.Failures != 0 || s.Web.AttemptsThisHour != 0 || s.State != "B2" {
		t.Errorf("the refusal read or counted something: %+v", s)
	}
	// Re-issue with the existing passphrase issues the first certificate.
	if r := ih.op(t, ops.OpHubIdentityReissue, map[string]any{"passphrase": idPass1}, h); r.code != 200 {
		t.Fatalf("re-issue in B2 = %d %s", r.code, r.body)
	}
	if s := ih.status(t); s.State != "B3" || !s.Serving {
		t.Errorf("status after = %+v", s)
	}
}

func TestIdentityCSRFAndSameOriginRules(t *testing.T) {
	ih := startIdentityHub(t, idHubOpts{})
	ih.bootstrap(t, idPass1, 90)
	tok, csrf := ih.sessionAt(t, "root", time.Second)
	body := map[string]any{"passphrase": idWrong}
	good := ih.browser(tok, csrf)

	missing := map[string]string{"Cookie": good["Cookie"], "Origin": good["Origin"]}
	forged := map[string]string{"Cookie": good["Cookie"], session.CSRFHeader: "forged", "Origin": good["Origin"]}
	foreign := ih.browser(tok, csrf)
	foreign["Origin"] = "https://evil.example"
	crossSite := ih.browser(tok, csrf)
	crossSite["Sec-Fetch-Site"] = "cross-site"
	noOrigin := ih.browser(tok, csrf)
	delete(noOrigin, "Origin")
	nullOrigin := ih.browser(tok, csrf)
	nullOrigin["Origin"] = "null"
	for name, h := range map[string]map[string]string{
		"missing token": missing, "forged token": forged, "foreign origin": foreign,
		"cross-site fetch": crossSite, "no origin": noOrigin, "null origin": nullOrigin,
	} {
		r := ih.op(t, ops.OpHubIdentityReissue, body, h)
		if r.code != 403 || r.err.Code != proto.CodeForbidden {
			t.Errorf("%s = %d %s", name, r.code, r.body)
		}
	}
	// Other content types are refused.
	req, _ := http.NewRequest("POST", ih.hc.base+"/api/v1/ops/"+ops.OpHubIdentityReissue, strings.NewReader("passphrase="+idWrong))
	for k, v := range good {
		req.Header.Set(k, v)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := ih.hc.c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode/100 == 2 {
		t.Errorf("form content type = %d %s", resp.StatusCode, b)
	}
	// None of these counted as a passphrase failure or attempt.
	if s := ih.status(t); s.Web.Failures != 0 || s.Web.AttemptsThisHour != 0 {
		t.Errorf("refused requests counted: %+v", s.Web)
	}
	// The same request, well-formed, reaches the service (and fails the passphrase).
	if r := ih.op(t, ops.OpHubIdentityReissue, body, good); r.code != 403 || r.err.Code != proto.CodePassphraseInvalid {
		t.Errorf("well-formed wrong passphrase = %d %s", r.code, r.body)
	}
}

// The effective HTTPS rule: TLS of the connection, or a forwarded protocol
// header that counts only from a trusted proxy; the loopback address alone is
// never a reason; the management listener is the one exception.
func TestIdentityOperationsNeedEffectiveHTTPS(t *testing.T) {
	plain := func(proxies string) idHubOpts {
		return idHubOpts{cfg: func(c *Config) {
			c.TLSOff, c.PublicURL = true, "https://hub.example.com"
			if proxies != "" {
				p, err := ParseTrustedProxies(proxies)
				if err != nil {
					t.Fatal(err)
				}
				c.TrustedProxies = p
			}
			c.ManagementListen = "127.0.0.1:0"
		}}
	}
	reissue := map[string]any{"passphrase": idWrong}
	check := func(t *testing.T, ih *idHub, extra map[string]string, wantTLS bool) {
		t.Helper()
		h := ih.fresh(t, "root")
		for k, v := range extra {
			h[k] = v
		}
		r := ih.op(t, ops.OpHubIdentityReissue, reissue, h)
		if wantTLS && (r.code != 403 || r.err.Code != proto.CodeTLSRequired) {
			t.Errorf("%v: want tls_required, got %d %s", extra, r.code, r.body)
		}
		if !wantTLS && r.err.Code == proto.CodeTLSRequired {
			t.Errorf("%v: refused as tls_required: %s", extra, r.body)
		}
	}

	t.Run("no proxies: forged forwarded headers are ignored", func(t *testing.T) {
		ih := startIdentityHub(t, plain(""))
		ih.bootstrapMgmt(t)
		check(t, ih, nil, true)
		check(t, ih, map[string]string{"X-Forwarded-Proto": "https"}, true)
		check(t, ih, map[string]string{"Forwarded": "proto=https", "X-Forwarded-Ssl": "on", "X-Forwarded-Scheme": "https"}, true)
		if s := ih.status(t); s.Web.Failures != 0 {
			t.Errorf("a cleartext refusal counted as a failure: %+v", s.Web)
		}
	})
	t.Run("loopback is not a reason when it is not a trusted proxy", func(t *testing.T) {
		ih := startIdentityHub(t, plain("10.0.0.0/8"))
		ih.bootstrapMgmt(t)
		check(t, ih, map[string]string{"X-Forwarded-Proto": "https"}, true)
	})
	t.Run("a trusted loopback proxy that says https", func(t *testing.T) {
		ih := startIdentityHub(t, plain("127.0.0.1/32"))
		ih.bootstrapMgmt(t)
		check(t, ih, map[string]string{"X-Forwarded-Proto": "https"}, false)
		check(t, ih, map[string]string{"X-Forwarded-Proto": "http"}, true)
		check(t, ih, nil, true) // a trusted proxy that sends nothing is still cleartext
	})
	t.Run("the Hub's own TLS", func(t *testing.T) {
		ih := startIdentityHub(t, idHubOpts{})
		check(t, ih, nil, false)
		// A forwarded http from an untrusted peer does not downgrade the real TLS.
		check(t, ih, map[string]string{"X-Forwarded-Proto": "http"}, false)
	})
	t.Run("the management listener is the only cleartext exception", func(t *testing.T) {
		ih := startIdentityHub(t, plain(""))
		addr := ih.h.srv.ManagementAddr()
		if addr == "" {
			t.Fatal("no management listener")
		}
		tok, csrf := ih.sessionAt(t, "root", time.Second)
		mg := newHubClient(t, ih.h, "http")
		mg.base = "http://" + addr
		h := asSession(tok, csrf)
		h["Origin"] = mg.base
		// The main listener refuses the same request; the management listener serves it.
		code, body, _ := ih.hc.do("POST", "/api/v1/ops/"+ops.OpHubIdentitySetPassphrase, map[string]any{"passphrase": idPass1, "confirm": idPass1}, mergeHeaders(h, map[string]string{"Origin": ih.hc.base}))
		if code != 403 || !strings.Contains(body, string(proto.CodeTLSRequired)) {
			t.Errorf("main listener = %d %s", code, body)
		}
		code, body, _ = mg.do("POST", "/api/v1/ops/"+ops.OpHubIdentitySetPassphrase, map[string]any{"passphrase": idPass1, "confirm": idPass1}, h)
		if code != 200 {
			t.Errorf("management listener = %d %s", code, body)
		}
		// A client cannot claim the management listener by a header.
		code, body, _ = ih.hc.do("POST", "/api/v1/ops/"+ops.OpHubIdentityReissue, reissue, mergeHeaders(h, map[string]string{"Origin": ih.hc.base, "X-Minion-Management": "1", "Host": addr}))
		if code != 403 || !strings.Contains(body, string(proto.CodeTLSRequired)) {
			t.Errorf("forged management claim = %d %s", code, body)
		}
	})
}

func mergeHeaders(a, b map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		out[k] = v
	}
	return out
}

// bootstrapMgmt gives a cleartext test Hub its passphrase through the
// management listener (the way a local operator would), so that later checks
// run in B3.
func (ih *idHub) bootstrapMgmt(t *testing.T) {
	t.Helper()
	mg := newHubClient(t, ih.h, "http")
	mg.base = "http://" + ih.h.srv.ManagementAddr()
	tok, csrf := ih.sessionAt(t, "root", time.Second)
	h := asSession(tok, csrf)
	h["Origin"] = mg.base
	if code, body, _ := mg.do("POST", "/api/v1/ops/"+ops.OpHubIdentitySetPassphrase, map[string]any{"passphrase": idPass1, "confirm": idPass1}, h); code != 200 {
		t.Fatalf("bootstrap through the management listener = %d %s", code, body)
	}
}

func TestIdentityLockoutIsShownAndTheWebLockDoesNotBlockTheCommand(t *testing.T) {
	ih := startIdentityHub(t, idHubOpts{})
	ih.bootstrap(t, idPass1, 90)
	h := ih.fresh(t, "root")
	for i := 0; i < 5; i++ {
		r := ih.op(t, ops.OpHubIdentityReissue, map[string]any{"passphrase": idWrong}, h)
		if r.code != 403 || r.err.Code != proto.CodePassphraseInvalid || r.err.Message != "The passphrase was not accepted." {
			t.Fatalf("attempt %d = %d %s", i+1, r.code, r.body)
		}
	}
	// The sixth is refused as locked, even with the right passphrase.
	r := ih.op(t, ops.OpHubIdentityReissue, map[string]any{"passphrase": idPass1}, h)
	if r.code != 429 || r.err.Code != proto.CodeRateLimited || r.err.Details["limit"] != "locked" || r.err.Details["retry_at"] == nil {
		t.Fatalf("locked = %d %s", r.code, r.body)
	}
	st := ih.status(t)
	if !st.Web.Locked || st.Web.RetryAfterSecs < 14*60 || st.Web.LockedUntil == nil || st.CLI.Locked || st.CLI.Failures != 0 {
		t.Errorf("status = web %+v cli %+v", st.Web, st.CLI)
	}
	if len(ih.auditRows(t, "hub_identity.locked")) != 1 || len(ih.auditRows(t, "hub_identity.unseal_failed")) != 5 {
		t.Errorf("audit: locked=%d unseal_failed=%d", len(ih.auditRows(t, "hub_identity.locked")), len(ih.auditRows(t, "hub_identity.unseal_failed")))
	}
	// The owner at the Hub host is not locked out: the command has its own
	// counter, lockout and allowance.
	code, body := ih.socket(t, "POST", adminsock.PathReissue, map[string]any{"passphrase": idPass1})
	if code != 200 {
		t.Fatalf("command while the web is locked = %d %s", code, body)
	}
	code, body = ih.socket(t, "GET", adminsock.PathStatus, nil)
	var cs ops.HubIdentityStatusResult
	if code != 200 || json.Unmarshal([]byte(body), &cs) != nil || !cs.Web.Locked || cs.CLI.Locked {
		t.Errorf("status over the socket = %d %s", code, body)
	}
	// The command's audit actor is local:<uid>, channel cli.
	rows := ih.auditRows(t, "hub_identity.reissue")
	if len(rows) == 0 || rows[0].Actor != fmt.Sprintf("local:%d", os.Geteuid()) || !strings.Contains(rows[0].Target, "channel=cli") {
		t.Errorf("command audit = %+v", rows)
	}
}

// Passphrases never reach a response, a log line, an audit event or a file.
func TestNoPassphraseIsEchoedLoggedOrAudited(t *testing.T) {
	ih := startIdentityHub(t, idHubOpts{})
	var bodies []string
	res := ih.op(t, ops.OpHubIdentitySetPassphrase, map[string]any{"passphrase": idCanary, "confirm": idCanary}, ih.fresh(t, "root"))
	bodies = append(bodies, res.body)
	h := ih.fresh(t, "root")
	for _, b := range []map[string]any{
		{"passphrase": idWrong},
		{"passphrase": idWrong, "validity_days": 7000},
		{"passphrase": 12345},
		{"passphrase": idCanary, "unknown_member": idWrong},
	} {
		bodies = append(bodies, ih.op(t, ops.OpHubIdentityReissue, b, h).body)
	}
	bodies = append(bodies, ih.op(t, ops.OpHubIdentityChangePassphrase, map[string]any{"passphrase": idWrong, "new_passphrase": idCanary + "2", "confirm": idCanary + "2"}, h).body)
	bodies = append(bodies, ih.op(t, ops.OpHubIdentityChangePassphrase, map[string]any{"passphrase": idCanary, "new_passphrase": idWrong + "2", "confirm": "mismatch-" + idWrong}, h).body)
	_, sockBody := ih.socket(t, "POST", adminsock.PathReissue, map[string]any{"passphrase": idWrong})
	bodies = append(bodies, sockBody)
	_, sockBody = ih.socket(t, "POST", adminsock.PathReissue, map[string]any{"passphrase": idWrong, "bogus": idCanary})
	bodies = append(bodies, sockBody)

	var all strings.Builder
	for _, b := range bodies {
		all.WriteString(b)
	}
	all.WriteString(ih.h.logs.String())
	rows, err := ih.st.ListAudit(context.Background(), store.AuditQuery{Limit: 500})
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		fmt.Fprintf(&all, "%s %s %s %s %s\n", row.Actor, row.ActingAs, row.Action, row.Target, row.Outcome)
	}
	entries, _ := os.ReadDir(ih.dir)
	for _, e := range entries {
		if e.IsDir() || strings.HasSuffix(e.Name(), ".sock") {
			continue
		}
		if b, err := os.ReadFile(ih.dir + "/" + e.Name()); err == nil {
			all.Write(b)
		}
	}
	for _, secret := range []string{idCanary, idWrong, idPass1} {
		if strings.Contains(all.String(), secret) {
			t.Errorf("the passphrase %q leaked into a response, log, audit event or file", secret)
		}
	}
	// And no part longer than a few characters of the canary either.
	if strings.Contains(all.String(), idCanary[:14]) || strings.Contains(all.String(), idWrong[:14]) {
		t.Error("a part of a passphrase leaked")
	}
}

// The expiry timer runs in the Hub: within 30 days of not_after it writes
// hub_identity.expiry_warning once per UTC day.
func TestTheExpiryTimerIsWired(t *testing.T) {
	old := identityTickEvery
	identityTickEvery = 50 * time.Millisecond
	t.Cleanup(func() { identityTickEvery = old })
	ih := startIdentityHub(t, idHubOpts{})
	ih.bootstrap(t, idPass1, 10) // 10 days: inside the 30-day window
	deadline := time.Now().Add(10 * time.Second)
	for len(ih.auditRows(t, "hub_identity.expiry_warning")) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("no hub_identity.expiry_warning event")
		}
		time.Sleep(25 * time.Millisecond)
	}
	time.Sleep(300 * time.Millisecond) // several more ticks
	rows := ih.auditRows(t, "hub_identity.expiry_warning")
	if len(rows) != 1 || rows[0].Actor != hubid.SystemActor || !strings.Contains(rows[0].Target, "channel=system") {
		t.Errorf("events = %+v", rows)
	}
	if st := ih.status(t); st.Warning != "warning" && st.Warning != "error" {
		t.Errorf("status warning = %q", st.Warning)
	}
}

func TestTheAdminSocketRefusesALiveSecondHubAndIsOwnerOnly(t *testing.T) {
	ih := startIdentityHub(t, idHubOpts{})
	fi, err := os.Lstat(adminsock.Path(ih.dir))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSocket == 0 || fi.Mode().Perm() != 0o600 {
		t.Errorf("socket mode = %v", fi.Mode())
	}
	if _, err := adminsock.Listen(adminsock.Path(ih.dir)); err == nil {
		t.Error("a second listener on a live socket was allowed")
	}
}
