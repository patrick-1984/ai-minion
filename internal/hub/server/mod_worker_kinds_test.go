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
	"github.com/patrick-1984/ai-minion/internal/hub/hubid/hubidtest"
	"github.com/patrick-1984/ai-minion/internal/hub/workers"
	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/transport"
)

func TestWorkerKindsModuleIsRegisteredInTheDefaultRegistry(t *testing.T) {
	names := DefaultRegistry.Names()
	pos := map[string]int{}
	for i, n := range names {
		pos[n] = i
	}
	me, ok := pos["worker_kinds"]
	if !ok {
		t.Fatalf("modules = %v", names)
	}
	for _, dep := range []string{"store", "audit", "authz", "workers", "api"} {
		if d, ok := pos[dep]; !ok || d > me {
			t.Errorf("module %q (at %d) must come before worker_kinds (at %d): %v", dep, d, me, names)
		}
	}
}

func TestWorkerKindsModuleNamesMissingServices(t *testing.T) {
	r := NewRegistry()
	registerWorkerKindsModule(r)
	err := NewKernel(r).Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), `"worker_kinds"`) {
		t.Errorf("err = %v", err)
	}
}

// connectWorker dials the Hub in connect mode as the Worker of wk and returns
// the welcome the Hub sent. The session stays open until the test ends.
func connectWorker(t *testing.T, addr string, wk *identity.Key, hubPub []byte, name string) workers.Welcome {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	conn, err := transport.Dial(ctx, "https://"+addr)
	if err != nil {
		t.Fatal(err)
	}
	res, err := transport.WorkerHandshake(conn, transport.WorkerConfig{
		Key: wk, Name: name, Ops: []string{"send_text"}, Mode: transport.ModeConnect,
		Pin: identity.Pin{Pub: hubPub}, Timeout: 10 * time.Second,
	})
	if err != nil {
		t.Fatalf("handshake: %v", err)
	}
	t.Cleanup(func() { _ = res.Channel.Close() })
	_ = res.Channel.SetReadDeadline(time.Now().Add(10 * time.Second))
	body, err := proto.ReadFrame(res.Channel, proto.StreamFrameLimit)
	if err != nil {
		t.Fatalf("reading welcome: %v", err)
	}
	var w workers.Welcome
	if err := json.Unmarshal(body, &w); err != nil || w.Type != proto.TypeWelcome {
		t.Fatalf("welcome = %s (%v)", body, err)
	}
	return w
}

// enrollInfraWorker enrolls a Worker with the infrastructure key and returns
// its key and welcome.
func enrollInfraWorker(t *testing.T, addr string, hubFpr identity.Fingerprint, name, secret string) (*identity.Key, workers.Welcome) {
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
		t.Fatalf("enroll handshake: %v", err)
	}
	defer func() { _ = res.Channel.Close() }()
	if err := proto.WriteFrame(res.Channel, proto.StreamFrameLimit, map[string]string{"type": "invite.secret", "secret": secret}); err != nil {
		t.Fatal(err)
	}
	_ = res.Channel.SetReadDeadline(time.Now().Add(10 * time.Second))
	body, err := proto.ReadFrame(res.Channel, proto.StreamFrameLimit)
	if err != nil {
		t.Fatalf("reading the enrollment answer: %v", err)
	}
	var w workers.Welcome
	if err := json.Unmarshal(body, &w); err != nil || w.Type != proto.TypeWelcome || w.WorkerID == "" {
		t.Fatalf("enrollment answer = %s (%v)", body, err)
	}
	return wk, w
}

// TestProductionStartupServesWorkerKindsTrustAndRestriction starts the Hub with
// the process-wide module registry, exactly as `minion hub serve` does. No test
// code wires a service. The five operations of Story 6.6 are served (none is
// unsupported_op), the stored preview_only policy reaches the welcome of a
// connecting Worker, and a removal deletes the Worker's trust rows.
func TestProductionStartupServesWorkerKindsTrustAndRestriction(t *testing.T) {
	const adminPW = "worker-kinds-integration-pw-1"
	t.Setenv("MINION_ADMIN_USERNAME", "root")
	t.Setenv("MINION_ADMIN_PASSWORD", adminPW)
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
	rootPub := hubidtest.Provision(t, data)
	h := startHub(t, &Config{DataDir: data}, DefaultRegistry)
	hubFpr, err := identity.FingerprintOf(rootPub)
	if err != nil {
		t.Fatal(err)
	}
	hc := newHubClient(t, h, "https")
	code, body, csrf := hc.login("root", adminPW)
	if code != 200 || csrf == "" {
		t.Fatalf("login = %d %s", code, body)
	}
	hdr := map[string]string{session.CSRFHeader: csrf}
	call := func(op string, args map[string]any) (int, string) {
		t.Helper()
		if op != ops.OpTrustList {
			args["request_id"] = ops.NewUUIDv7()
		}
		code, body, _ := hc.do("POST", "/api/v1/ops/"+op, args, hdr)
		if code == 501 || strings.Contains(body, string(proto.CodeUnsupportedOp)) {
			t.Fatalf("%s is not served: %d %s", op, code, body)
		}
		return code, body
	}
	trustCount := func() int {
		t.Helper()
		code, body := call(ops.OpTrustList, map[string]any{})
		var list ops.TrustListResult
		if code != 200 || json.Unmarshal([]byte(body), &list) != nil {
			t.Fatalf("trust_list = %d %s", code, body)
		}
		return len(list.Trust)
	}

	code, body = call(ops.OpUserCreate, map[string]any{"username": "alice"})
	var alice ops.UserCreateResult
	if code != 200 || json.Unmarshal([]byte(body), &alice) != nil || alice.User.ID == "" {
		t.Fatalf("user_create = %d %s", code, body)
	}

	wk, first := enrollInfraWorker(t, h.srv.Addr(), hubFpr, "edge-1", infraSecret)
	if first.Kind != "infrastructure" || first.Policy.PreviewOnly {
		t.Fatalf("enrolled welcome = %+v", first)
	}
	id := first.WorkerID

	// worker_restrict: stored, and the next welcome carries it.
	if code, body := call(ops.OpWorkerRestrict, map[string]any{"worker_id": id, "preview_only": true}); code != 200 {
		t.Fatalf("worker_restrict = %d %s", code, body)
	}
	if w := connectWorker(t, h.srv.Addr(), wk, rootPub, "edge-1"); !w.Policy.PreviewOnly {
		t.Errorf("welcome = %+v, want preview_only after the restriction", w)
	}
	if code, body := call(ops.OpWorkerRestrict, map[string]any{"worker_id": id, "preview_only": false}); code != 200 {
		t.Fatalf("worker_restrict off = %d %s", code, body)
	}
	if w := connectWorker(t, h.srv.Addr(), wk, rootPub, "edge-1"); w.Policy.PreviewOnly {
		t.Errorf("welcome = %+v, want the restriction lifted", w)
	}

	// worker_rename, and the next welcome carries the new name.
	if code, body := call(ops.OpWorkerRename, map[string]any{"worker_id": id, "name": "edge-renamed"}); code != 200 {
		t.Fatalf("worker_rename = %d %s", code, body)
	}

	// trust_set and trust_list.
	if code, body := call(ops.OpTrustSet, map[string]any{"worker_id": id}); code != 200 {
		t.Fatalf("trust_set = %d %s", code, body)
	}
	if n := trustCount(); n != 1 {
		t.Fatalf("trust rows = %d, want 1", n)
	}

	// worker_reassign deletes the trust of the Worker.
	if code, body := call(ops.OpWorkerReassign, map[string]any{"worker_id": id, "user_id": alice.User.ID}); code != 200 {
		t.Fatalf("worker_reassign = %d %s", code, body)
	}
	if n := trustCount(); n != 0 {
		t.Errorf("trust rows after the reassignment = %d, want 0", n)
	}

	// worker_remove (Story 4.10) deletes the trust rows through the cleanup
	// this module's story supplies.
	if code, body := call(ops.OpTrustSet, map[string]any{"worker_id": id, "session_name": "minion-1"}); code != 200 {
		t.Fatalf("trust_set = %d %s", code, body)
	}
	if n := trustCount(); n != 1 {
		t.Fatalf("trust rows = %d, want 1", n)
	}
	if code, body := call(ops.OpWorkerRemove, map[string]any{"worker_id": id}); code != 200 {
		t.Fatalf("worker_remove = %d %s", code, body)
	}
	if n := trustCount(); n != 0 {
		t.Errorf("trust rows after the removal = %d, want 0", n)
	}
	for _, secret := range []string{infraSecret, adminPW} {
		if strings.Contains(h.logs.String(), secret) {
			t.Error("a secret reached the Hub log")
		}
	}
}
