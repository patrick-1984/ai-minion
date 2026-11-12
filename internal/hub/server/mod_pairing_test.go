package server

import (
	"context"
	"encoding/json"
	"github.com/patrick-1984/ai-minion/internal/testutil/hubstore"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/audit"
	"github.com/patrick-1984/ai-minion/internal/hub/auth/session"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/hub/workers"
	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/transport"
)

// productionRegistry copies every module of the DefaultRegistry, so the test
// runs the modules a real Hub runs, with the same order and wiring.
func productionRegistry(t *testing.T) *Registry {
	t.Helper()
	r := NewRegistry()
	for _, m := range DefaultRegistry.sorted() {
		var opts []ModuleOption
		for _, d := range m.declares {
			d := d
			opts = append(opts, func(m *module) {
				if !m.declaresType(d) {
					m.declares = append(m.declares, d)
				}
			})
		}
		if err := r.Register(m.name, m.order, m.init, opts...); err != nil {
			t.Fatal(err)
		}
	}
	return r
}

func TestPairingModuleIsRegisteredAfterAPIAndWorkers(t *testing.T) {
	names := DefaultRegistry.Names()
	pi, wi, ai := slices.Index(names, "pairing"), slices.Index(names, "workers"), slices.Index(names, "api")
	if pi < 0 || wi < 0 || ai < 0 || pi < wi || pi < ai {
		t.Errorf("modules = %v", names)
	}
}

func TestPairingModuleNamesMissingServices(t *testing.T) {
	r := NewRegistry()
	registerPairingAPIModule(r)
	err := NewKernel(r).Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), `"pairing"`) {
		t.Errorf("err = %v", err)
	}
}

// TestProductionHubPairsAWorkerThroughAnAuthenticatedRESTClaim is the
// integration test of the production wiring of Story 4.8: the Hub is started
// from its own modules only. A Worker connects in pair_code mode, a logged-in
// user claims the code over REST, and the Worker is created for that user.
// Without EnablePairing the Worker is refused with invalid_argument; without
// the REST handler the claim is unsupported_op.
func TestProductionHubPairsAWorkerThroughAnAuthenticatedRESTClaim(t *testing.T) {
	t.Setenv("MINION_ADMIN_USERNAME", "root")
	t.Setenv("MINION_ADMIN_PASSWORD", "pairing-integration-pw-1")
	var (
		svc *workers.Service
		st  *store.Store
		lg  *audit.Log
	)
	reg := productionRegistry(t)
	if err := reg.Register("grab", 90, func(_ context.Context, env *Env) error {
		var err error
		if svc, err = Require[*workers.Service](env); err != nil {
			return err
		}
		if st, err = Require[*store.Store](env); err != nil {
			return err
		}
		lg, err = Require[*audit.Log](env)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	h := startHub(t, &Config{DataDir: hubstore.Dir(t)}, reg)
	hc := newHubClient(t, h, "https")

	code := identity.NewPairingCode()
	claimBody := func(c string) map[string]any {
		return map[string]any{"request_id": ops.NewUUIDv7(), "code": c}
	}

	// An unauthenticated claim is refused before it reaches the service.
	if status, body, _ := hc.do("POST", "/api/v1/ops/pairing_claim", claimBody(code.String()), nil); status != http.StatusUnauthorized {
		t.Fatalf("unauthenticated claim = %d %s", status, body)
	}

	// The Worker dials with no flag and no pin, and is held as pending.
	key, err := identity.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	type outcome struct {
		res *transport.WorkerResult
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		conn, err := transport.Dial(ctx, "https://"+h.srv.Addr())
		if err != nil {
			done <- outcome{err: err}
			return
		}
		res, err := transport.WorkerHandshake(conn, transport.WorkerConfig{
			Key: key, Name: "raspberry", Ops: []string{"send_text"}, Mode: transport.ModePairCode,
			PairingCode: code, Timeout: 30 * time.Second,
		})
		done <- outcome{res: res, err: err}
	}()
	deadline := time.Now().Add(20 * time.Second)
	for svc == nil || svc.Pairings() == nil || !svc.Pairings().IsPending(code.PairingID()) {
		select {
		case o := <-done:
			t.Fatalf("the Worker was refused before it was held as pending: %v", o.err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("the Hub never held the Worker as pending")
		}
		time.Sleep(10 * time.Millisecond)
	}

	status, body, csrf := hc.login("root", "pairing-integration-pw-1")
	if status != 200 || csrf == "" {
		t.Fatalf("login = %d %s", status, body)
	}
	hdr := map[string]string{session.CSRFHeader: csrf}

	// A wrong code is a pairing_code_invalid with the attempts left.
	wrong := identity.NewPairingCode()
	if status, body, _ := hc.do("POST", "/api/v1/ops/pairing_claim", claimBody(wrong.String()), hdr); status != proto.HTTPStatus(proto.CodePairingCodeInvalid) {
		t.Fatalf("claim of an unknown code = %d %s", status, body)
	}

	status, body, _ = hc.do("POST", "/api/v1/ops/pairing_claim", claimBody(code.String()), hdr)
	if status != 200 {
		t.Fatalf("claim = %d %s", status, body)
	}
	var res ops.WriteResult
	if err := json.Unmarshal([]byte(body), &res); err != nil || res.Outcome != ops.OutcomeDelivered {
		t.Fatalf("claim result = %s (%v)", body, err)
	}

	o := <-done
	if o.err != nil {
		t.Fatalf("the Worker handshake failed: %v", o.err)
	}
	defer o.res.Channel.Close()
	frame, err := proto.ReadFrame(o.res.Channel, proto.StreamFrameLimit)
	if err != nil {
		t.Fatal(err)
	}
	var welcome workers.Welcome
	if err := json.Unmarshal(frame, &welcome); err != nil || welcome.Type != proto.TypeWelcome {
		t.Fatalf("welcome = %s (%v)", frame, err)
	}
	// The owner name is the username, not the user id: the lookup is wired.
	if welcome.Owner != "root" || welcome.Name != "raspberry" || welcome.Kind != store.WorkerKindPrivate {
		t.Errorf("welcome = %+v", welcome)
	}
	admin, err := st.UserByIdentifier(context.Background(), "root")
	if err != nil {
		t.Fatal(err)
	}
	row, err := st.WorkerByPub(context.Background(), key.Public())
	if err != nil || row.OwnerID != admin.ID || row.ID != welcome.WorkerID {
		t.Fatalf("worker row = %+v (%v), want owner %s", row, err, admin.ID)
	}
	page, err := lg.List(context.Background(), audit.Filter{Action: "worker.pair"})
	if err != nil || len(page.Events) != 1 || page.Events[0].Target != row.ID {
		t.Errorf("audit = %+v (%v)", page.Events, err)
	}
	if strings.Contains(h.logs.String(), code.Canonical()) {
		t.Errorf("the pairing code reached the log:\n%s", h.logs)
	}
}
