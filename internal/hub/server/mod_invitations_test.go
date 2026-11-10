package server

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/auth/session"
	"github.com/patrick-1984/ai-minion/internal/hub/workers"
	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/transport"
)

func TestInvitationsModuleIsRegisteredInTheDefaultRegistry(t *testing.T) {
	names := DefaultRegistry.Names()
	pos := map[string]int{}
	for i, n := range names {
		pos[n] = i
	}
	inv, ok := pos["invitations"]
	if !ok {
		t.Fatalf("modules = %v", names)
	}
	for _, dep := range []string{"store", "audit", "authz", "auth", "api", "workers"} {
		if d, ok := pos[dep]; !ok || d > inv {
			t.Errorf("module %q (at %d) must come before invitations (at %d): %v", dep, d, inv, names)
		}
	}
}

func TestInvitationsModuleNamesMissingServices(t *testing.T) {
	r := NewRegistry()
	registerInvitationsModule(r)
	err := NewKernel(r).Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), `"invitations"`) {
		t.Errorf("err = %v", err)
	}
}

// enrollWorker dials the Hub in enroll_invite mode with key (a whole key, or
// only its secret when hubFpr names the Hub) and returns the Hub's answer
// frame type and body.
func enrollWorker(t *testing.T, addr string, hubFpr identity.Fingerprint, name, secret string) (string, []byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	conn, err := transport.Dial(ctx, "https://"+addr)
	if err != nil {
		t.Fatal(err)
	}
	wk, err := identity.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	res, err := transport.WorkerHandshake(conn, transport.WorkerConfig{
		Key: wk, Name: name, Mode: transport.ModeEnrollInvite, HubFingerprint: hubFpr,
		Ops: []string{"send_text"}, Timeout: 10 * time.Second,
	})
	if err != nil {
		t.Fatalf("handshake: %v", err)
	}
	t.Cleanup(func() { _ = res.Channel.Close() })
	if err := proto.WriteFrame(res.Channel, proto.StreamFrameLimit, map[string]string{"type": "invite.secret", "secret": secret}); err != nil {
		t.Fatal(err)
	}
	_ = res.Channel.SetReadDeadline(time.Now().Add(10 * time.Second))
	body, err := proto.ReadFrame(res.Channel, proto.StreamFrameLimit)
	if err != nil {
		t.Fatalf("reading the answer: %v", err)
	}
	var head struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(body, &head); err != nil {
		t.Fatalf("answer %s: %v", body, err)
	}
	return head.Type, body
}

func secretPart(t *testing.T, key string) string {
	t.Helper()
	_, secret, ok := strings.Cut(key, ".")
	if !ok {
		t.Fatal("malformed key")
	}
	return secret
}

// TestProductionStartupCreatesAnInvitationAndEnrollsAWorker starts the Hub with
// the process-wide module registry, exactly as `minion hub serve` does, with
// MINION_INFRA_INVITE_KEY_FILE set. No test code wires a service: an admin
// creates an invitation through the REST API and a Worker enrolls with it, then
// a Worker enrolls with the infrastructure key.
func TestProductionStartupCreatesAnInvitationAndEnrollsAWorker(t *testing.T) {
	const adminPW = "invitations-integration-pw-1"
	t.Setenv("MINION_ADMIN_USERNAME", "root")
	t.Setenv("MINION_ADMIN_PASSWORD", adminPW)

	// The infrastructure key is the bare secret: the operator does not know
	// the Hub fingerprint before the first start.
	other, err := identity.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	infra, err := identity.NewInviteKey(other.Fingerprint())
	if err != nil {
		t.Fatal(err)
	}
	infraSecret := secretPart(t, infra.String())
	keyFile := filepath.Join(t.TempDir(), "infra.key")
	if err := os.WriteFile(keyFile, []byte(infraSecret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MINION_INFRA_INVITE_KEY_FILE", keyFile)

	data := t.TempDir()
	h := startHub(t, &Config{DataDir: data}, DefaultRegistry)
	hubKey, err := identity.LoadKey(filepath.Join(data, workers.IdentityFile))
	if err != nil {
		t.Fatal(err)
	}
	hubFpr := hubKey.Fingerprint()

	hc := newHubClient(t, h, "https")
	code, body, csrf := hc.login("root", adminPW)
	if code != 200 || csrf == "" {
		t.Fatalf("login = %d %s", code, body)
	}
	hdr := map[string]string{session.CSRFHeader: csrf}

	// invitation_create is served (it was unsupported_op before the wiring).
	code, body, _ = hc.do("POST", "/api/v1/ops/invitation_create", ops.InvitationCreateArgs{Mode: ops.InvitationOneTime, TTL: ops.TTL1h, Name: "lab"}, hdr)
	if code != 200 {
		t.Fatalf("invitation_create = %d %s", code, body)
	}
	var created ops.InvitationCreateResult
	if err := json.Unmarshal([]byte(body), &created); err != nil || created.ID == "" || !strings.HasPrefix(created.Key, "mnk_") {
		t.Fatalf("create result = %v (%v)", body, err)
	}
	pk, err := identity.ParseInviteKey(created.Key)
	if err != nil || pk.Fingerprint != hubFpr {
		t.Fatalf("the key does not name this Hub: fingerprint %v want %v (%v)", pk.Fingerprint, hubFpr, err)
	}

	// A Worker enrolls with the key; a one-time key is then exhausted.
	typ, ans := enrollWorker(t, h.srv.Addr(), hubFpr, "edge-1", secretPart(t, created.Key))
	var w workers.Welcome
	if typ != proto.TypeWelcome || json.Unmarshal(ans, &w) != nil || w.WorkerID == "" || w.Kind != "private" {
		t.Fatalf("enrollment answer = %s", ans)
	}
	typ, ans = enrollWorker(t, h.srv.Addr(), hubFpr, "edge-2", secretPart(t, created.Key))
	var ef proto.ErrorFrame
	if typ != proto.TypeError || json.Unmarshal(ans, &ef) != nil || ef.Code != proto.CodeInviteExhausted {
		t.Fatalf("second use answer = %s, want invite_exhausted", ans)
	}

	// invitation_list shows the use.
	code, body, _ = hc.do("POST", "/api/v1/ops/invitation_list", ops.InvitationListArgs{}, hdr)
	var list ops.InvitationListResult
	if code != 200 || json.Unmarshal([]byte(body), &list) != nil || len(list.Invitations) != 1 {
		t.Fatalf("invitation_list = %d %s", code, body)
	}
	if got := list.Invitations[0]; got.ID != created.ID || got.Uses != 1 || got.Status != "exhausted" {
		t.Errorf("listed invitation = %+v", got)
	}

	// MINION_INFRA_INVITE_KEY_FILE takes effect: the infrastructure key
	// enrolls an infrastructure Worker, repeatedly.
	for _, name := range []string{"infra-1", "infra-2"} {
		typ, ans = enrollWorker(t, h.srv.Addr(), hubFpr, name, infraSecret)
		w = workers.Welcome{}
		if typ != proto.TypeWelcome || json.Unmarshal(ans, &w) != nil || w.Kind != "infrastructure" {
			t.Fatalf("infrastructure enrollment answer = %s", ans)
		}
	}

	// The audit log holds the creation and the enrollments, and no secret.
	code, body, _ = hc.do("POST", "/api/v1/ops/audit_list", map[string]any{"limit": 100}, hdr)
	if code != 200 {
		t.Fatalf("audit_list = %d %s", code, body)
	}
	var events ops.AuditListResult
	if err := json.Unmarshal([]byte(body), &events); err != nil {
		t.Fatal(err)
	}
	seen := map[string]int{}
	for _, e := range events.Events {
		if e.Outcome == "success" {
			seen[e.Action]++
		}
	}
	if seen[workers.AuditInvitationCreate] != 1 || seen[workers.AuditWorkerEnroll] != 3 {
		t.Errorf("audit actions = %v, want 1 %s and 3 %s successes", seen, workers.AuditInvitationCreate, workers.AuditWorkerEnroll)
	}
	for _, secret := range []string{secretPart(t, created.Key), infraSecret, adminPW} {
		if strings.Contains(body, secret) || strings.Contains(h.logs.String(), secret) {
			t.Error("a secret reached the audit log or the Hub log")
		}
	}
}

// TestProductionStartupRejectsABrokenInfrastructureKeyFile: a configured key
// file that cannot be loaded stops the start, instead of leaving infrastructure
// Workers rejected without a word.
func TestProductionStartupRejectsABrokenInfrastructureKeyFile(t *testing.T) {
	t.Setenv("MINION_ADMIN_USERNAME", "root")
	t.Setenv("MINION_ADMIN_PASSWORD", "invitations-integration-pw-1")
	for name, content := range map[string]string{
		"malformed":  "not-a-key\n",
		"other hub":  "mnk_" + strings.Repeat("a", 52) + "." + strings.Repeat("b", 52) + "\n",
		"empty file": "",
	} {
		t.Run(name, func(t *testing.T) {
			keyFile := filepath.Join(t.TempDir(), "infra.key")
			if err := os.WriteFile(keyFile, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("MINION_INFRA_INVITE_KEY_FILE", keyFile)
			logs := &syncBuf{}
			srv := New(&Config{DataDir: t.TempDir(), Listen: "127.0.0.1:0", Logger: testLogger(logs)}, DefaultRegistry)
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			err := srv.Run(ctx)
			if err == nil || !strings.Contains(err.Error(), "invitations") {
				t.Fatalf("err = %v", err)
			}
		})
	}
	t.Run("missing file", func(t *testing.T) {
		t.Setenv("MINION_INFRA_INVITE_KEY_FILE", filepath.Join(t.TempDir(), "absent.key"))
		srv := New(&Config{DataDir: t.TempDir(), Listen: "127.0.0.1:0", Logger: testLogger(&syncBuf{})}, DefaultRegistry)
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if err := srv.Run(ctx); err == nil || !strings.Contains(err.Error(), "invitations") {
			t.Fatalf("err = %v", err)
		}
	})
}
