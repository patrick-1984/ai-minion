package server

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/audit"
	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/hub/hubid"
	"github.com/patrick-1984/ai-minion/internal/hub/hubid/hubidtest"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/hub/workers"
	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/transport"
)

// workersRegistry registers the modules the workers module needs, in the
// order of the real Hub, without the other stories' modules.
func workersRegistry(t *testing.T) *Registry {
	t.Helper()
	r := NewRegistry()
	if err := registerCore(r); err != nil {
		t.Fatal(err)
	}
	if err := registerStore(r); err != nil {
		t.Fatal(err)
	}
	registerAuthz(r)
	registerAuditModule(r)
	registerHubIDModule(r)
	registerWorkersModule(r)
	return r
}

func TestWorkersModuleIsRegisteredInTheDefaultRegistry(t *testing.T) {
	found := false
	for _, n := range DefaultRegistry.Names() {
		found = found || n == "workers"
	}
	if !found {
		t.Errorf("modules = %v", DefaultRegistry.Names())
	}
}

func TestWorkersModuleNamesMissingServices(t *testing.T) {
	r := NewRegistry()
	registerWorkersModule(r)
	err := NewKernel(r).Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), `"workers"`) {
		t.Errorf("err = %v", err)
	}
}

// TestHubServesTheWorkerConnectEndpoint is the integration test of the
// module: a real Hub run over TLS, a Worker that dials with no flag and no
// certificate check, the identity key on disk, the audit log and the authz
// lookup wired.
func TestHubServesTheWorkerConnectEndpoint(t *testing.T) {
	data := t.TempDir()
	reg := workersRegistry(t)
	var (
		svc *workers.Service
		st  *store.Store
		az  *authz.Authorizer
		lg  *audit.Log
	)
	if err := reg.Register("grab", 60, func(_ context.Context, env *Env) error {
		var err error
		if svc, err = Require[*workers.Service](env); err != nil {
			return err
		}
		if st, err = Require[*store.Store](env); err != nil {
			return err
		}
		if az, err = Require[*authz.Authorizer](env); err != nil {
			return err
		}
		lg, err = Require[*audit.Log](env)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	h := startHub(t, &Config{DataDir: data}, reg)

	// The Hub is established (sealed root, certified operational key); no
	// plain identity key exists, and the invitation fingerprint is the root's.
	if _, err := os.Stat(filepath.Join(data, workers.IdentityFile)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a plain identity key exists: %v", err)
	}
	if svc.Identity().Fingerprint() == "" || len(svc.Identity().Public()) != 32 {
		t.Errorf("no root fingerprint: %q", svc.Identity().Fingerprint())
	}

	// The authz Worker lookup is wired to the workers table.
	wk, _ := identity.GenerateKey()
	row := store.WorkerRow{ID: store.NewID(), Pub: wk.Public(), Name: "pi", OwnerID: "owner-1", Kind: store.WorkerKindPrivate, CreatedAt: time.Now().UnixMilli()}
	if err := st.InsertWorker(context.Background(), nil, row); err != nil {
		t.Fatal(err)
	}
	if err := az.SetWorkerLookup(nil); err == nil {
		t.Error("SetWorkerLookup(nil) accepted")
	}
	if err := az.SetUserLookup(userLookupFunc(func(string) authz.UserState { return authz.UserState{Role: authz.RoleUser, Active: true} })); err != nil {
		t.Fatal(err)
	}
	read := ops.MustActionSet(ops.ActionRead)
	if err := az.Decide(context.Background(), authz.NewSessionPrincipal("owner-1"), read, authz.WorkerResource{ID: row.ID}); err != nil {
		t.Errorf("owner reads own Worker: %v", err)
	}
	if err := az.Decide(context.Background(), authz.NewSessionPrincipal("someone-else"), read, authz.WorkerResource{ID: row.ID}); err == nil {
		t.Error("a foreign user reads the Worker")
	}

	// A Worker connects over wss with no TLS flag and gets welcome.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	conn, err := transport.Dial(ctx, "https://"+h.srv.Addr())
	if err != nil {
		t.Fatal(err)
	}
	res, err := transport.WorkerHandshake(conn, transport.WorkerConfig{
		Key: wk, Name: "pi", Ops: []string{"send_text"}, Mode: transport.ModeConnect,
		Pin: identity.Pin{Pub: svc.Identity().Public()}, Timeout: 10 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	body, err := proto.ReadFrame(res.Channel, proto.StreamFrameLimit)
	if err != nil {
		t.Fatal(err)
	}
	var welcome workers.Welcome
	if err := json.Unmarshal(body, &welcome); err != nil || welcome.Type != proto.TypeWelcome || welcome.WorkerID != row.ID {
		t.Fatalf("welcome = %s (%v)", body, err)
	}
	sess, err := transport.NewSession(res.Channel, transport.RoleWorker, transport.SessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for !svc.Registry().IsOnline(row.ID) {
		if time.Now().After(deadline) {
			t.Fatal("the Worker never went online")
		}
		time.Sleep(5 * time.Millisecond)
	}

	// Stopping the Hub closes the Worker connection (hijacked connections
	// are not covered by the HTTP server's own shutdown).
	h.stop(t)
	select {
	case <-sess.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("the Worker connection survived the Hub stop")
	}
	_ = lg
}

type userLookupFunc func(string) authz.UserState

func (f userLookupFunc) User(_ context.Context, id string) (authz.UserState, error) {
	return f(id), nil
}

func TestHubIdentitySurvivesRestarts(t *testing.T) {
	data := t.TempDir()
	var fps []string
	for i := 0; i < 2; i++ {
		var svc *workers.Service
		reg := workersRegistry(t)
		if err := reg.Register("grab", 60, func(_ context.Context, env *Env) error {
			var err error
			svc, err = Require[*workers.Service](env)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		h := startHub(t, &Config{DataDir: data}, reg)
		fps = append(fps, svc.Identity().Fingerprint().String())
		h.stop(t)
	}
	if fps[0] != fps[1] {
		t.Fatalf("identity changed across restarts: %v", fps)
	}
}

// identityRun is what one Hub start reports about its identity and its TLS
// certificate.
type identityRun struct {
	identity string // fingerprint of the Hub identity key
	tlsPrint string // fingerprint of the TLS certificate the listener served
}

// startAndObserve starts a Hub on data, records its identity fingerprint and
// the fingerprint of the certificate its listener serves, and stops it.
func startAndObserve(t *testing.T, data string) identityRun {
	t.Helper()
	var svc *workers.Service
	reg := workersRegistry(t)
	if err := reg.Register("grab", 60, func(_ context.Context, env *Env) error {
		var err error
		svc, err = Require[*workers.Service](env)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	h := startHub(t, &Config{DataDir: data}, reg)
	conn, err := tls.Dial("tcp", h.srv.Addr(), &tls.Config{InsecureSkipVerify: true}) //nolint:gosec // reads the self-signed certificate
	if err != nil {
		t.Fatal(err)
	}
	certs := conn.ConnectionState().PeerCertificates
	_ = conn.Close()
	if len(certs) == 0 {
		t.Fatal("the Hub served no certificate")
	}
	run := identityRun{identity: svc.Identity().Fingerprint().String(), tlsPrint: CertFingerprint(certs[0].Raw)}
	h.stop(t)
	return run
}

// TestHubIdentityIsIndependentOfTheTLSCertificate is the "never changes on
// certificate rotation" part of the identity AC: the TLS pair is deleted
// between two starts, the Hub serves a new certificate and keeps its identity.
func TestHubIdentityIsIndependentOfTheTLSCertificate(t *testing.T) {
	data := t.TempDir()
	first := startAndObserve(t, data)
	for _, f := range []string{TLSCertFile, TLSKeyFile} {
		if err := os.Remove(filepath.Join(data, TLSDir, f)); err != nil {
			t.Fatal(err)
		}
	}
	second := startAndObserve(t, data)
	if first.tlsPrint == second.tlsPrint {
		t.Fatalf("the TLS certificate was not rotated: %s", first.tlsPrint)
	}
	if first.identity != second.identity {
		t.Fatalf("the Hub identity changed with the TLS certificate: %s -> %s", first.identity, second.identity)
	}
}

// TestHubIdentitySurvivesARestoreIntoAFreshDataDir is the "restore" part of
// the identity AC: identity.key, data.key and the database are copied into an
// empty data directory (a backup restore) and the Hub there has the same
// identity, with its own fresh TLS certificate.
func TestHubIdentitySurvivesARestoreIntoAFreshDataDir(t *testing.T) {
	data := t.TempDir()
	first := startAndObserve(t, data) // the Hub is stopped: the database is checkpointed
	restored := t.TempDir()
	for _, name := range []string{"root.key.age", "op.key", store.DBFile, store.DBFile + "-wal", store.DBFile + "-shm", "data.key"} {
		raw, err := os.ReadFile(filepath.Join(data, name))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(restored, name), raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	second := startAndObserve(t, restored)
	if first.identity != second.identity {
		t.Fatalf("the Hub identity changed on restore: %s -> %s", first.identity, second.identity)
	}
	if first.tlsPrint == second.tlsPrint {
		t.Error("the restored Hub reused a TLS certificate it was never given")
	}
}

func TestHubRefusesToStartWithoutItsSealedRoot(t *testing.T) {
	data := t.TempDir()
	reg := workersRegistry(t)
	h := startHub(t, &Config{DataDir: data}, reg)
	h.stop(t)
	if err := os.Remove(filepath.Join(data, "root.key.age")); err != nil {
		t.Fatal(err)
	}
	srv := New(&Config{DataDir: data, Listen: "127.0.0.1:0", Logger: testLogger(&syncBuf{})}, workersRegistry(t))
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	err := srv.Run(ctx)
	if err == nil || !strings.Contains(err.Error(), "root.key.age") || !strings.Contains(err.Error(), "restore") {
		t.Fatalf("err = %v", err)
	}
	for _, name := range []string{"root.key.age", workers.IdentityFile} {
		if _, statErr := os.Stat(filepath.Join(data, name)); !errors.Is(statErr, fs.ErrNotExist) {
			t.Errorf("a new %s was generated", name)
		}
	}
}

func TestConnectEndpointRequiresTheWorkerSubprotocol(t *testing.T) {
	reg := workersRegistry(t)
	h := startHub(t, &Config{DataDir: t.TempDir()}, reg)
	req, _ := http.NewRequest(http.MethodGet, "https://"+h.srv.Addr()+transport.ConnectPath, nil)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Sec-WebSocket-Version", "13")
	req.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	resp, err := insecureClient(0).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSwitchingProtocols {
		if resp.StatusCode < 400 {
			t.Errorf("status = %d", resp.StatusCode)
		}
		return
	}
	// The upgrade completed without subprotocol minion.v1: the Hub sends a
	// WebSocket close frame at once instead of starting a handshake (a
	// handshake would start with the Worker's hello, so any first bytes from
	// the Hub are the close).
	done := make(chan struct{})
	go func() {
		buf := make([]byte, 64)
		_, _ = resp.Body.Read(buf)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Error("the Hub kept a connection without subprotocol minion.v1")
	}
}

// A pristine data directory (B0) starts the Hub without a Worker service and
// without creating any key; the Worker endpoint answers 503 (design 9.7).
func TestAPristineHubStartsWithoutKeysAndRefusesWorkers(t *testing.T) {
	data := t.TempDir()
	reg := workersRegistry(t)
	var id *hubid.Service
	if err := reg.Register("grab", 60, func(_ context.Context, env *Env) error {
		var err error
		id, err = Require[*hubid.Service](env)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	h := startHubPristine(t, &Config{DataDir: data}, reg)
	if id.State() != hubid.StatePristine {
		t.Fatalf("state = %v", id.State())
	}
	for _, name := range []string{"identity.key", "identity.key.next", "root.key.age", "root.key.age.next", "op.key", "op.key.next"} {
		if _, err := os.Stat(filepath.Join(data, name)); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("B0 created %s: %v", name, err)
		}
	}
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}} //nolint:gosec // test
	resp, err := client.Get("https://" + h.srv.Addr() + transport.ConnectPath)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("the Worker endpoint answered %d, want 503", resp.StatusCode)
	}
}

// A migrated 0.6 Hub (B1: plain identity.key, no passphrase) and a Hub with a
// sealed root but no certificate yet (B2) refuse Worker connections with 503
// and create no certificate (design 9.7).
func TestB1AndB2HubsRefuseWorkerConnections(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, dir string)
		want  hubid.State
	}{
		{"B1", func(t *testing.T, dir string) {
			st, err := store.Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = st.Close() }()
			k, err := identity.GenerateKey()
			if err != nil {
				t.Fatal(err)
			}
			if err := identity.SaveKey(filepath.Join(dir, hubid.PlainFile), k); err != nil {
				t.Fatal(err)
			}
			if err := st.PutHubIdentity(context.Background(), store.HubIdentityRow{Pub: k.Public(), Fingerprint: k.Fingerprint().String(), CreatedAt: 1}); err != nil {
				t.Fatal(err)
			}
			if _, err := st.DB().Exec(`UPDATE hub_identity SET migrated_from_06 = 1`); err != nil {
				t.Fatal(err)
			}
		}, hubid.StatePlain},
		{"B2", func(t *testing.T, dir string) {
			hubidtest.Provision(t, dir)
			st, err := store.Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = st.Close() }()
			if _, err := st.DB().Exec(`UPDATE hub_identity SET current_cert = NULL`); err != nil {
				t.Fatal(err)
			}
		}, hubid.StateSealedNoCert},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := t.TempDir()
			tc.setup(t, data)
			reg := workersRegistry(t)
			var id *hubid.Service
			if err := reg.Register("grab", 60, func(_ context.Context, env *Env) error {
				var err error
				id, err = HubIdentity(env)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			h := startHubPristine(t, &Config{DataDir: data}, reg)
			if id.State() != tc.want {
				t.Fatalf("state = %v, want %v", id.State(), tc.want)
			}
			client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}} //nolint:gosec // test
			resp, err := client.Get("https://" + h.srv.Addr() + transport.ConnectPath)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusServiceUnavailable {
				t.Errorf("the Worker endpoint answered %d, want 503", resp.StatusCode)
			}
			if _, err := id.Current(); err == nil {
				t.Error("the Hub serves a certificate")
			}
		})
	}
}
