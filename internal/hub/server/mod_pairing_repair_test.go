package server

import (
	"context"
	"github.com/patrick-1984/ai-minion/internal/testutil/hubstore"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/auth/session"
	"github.com/patrick-1984/ai-minion/internal/hub/hubid/hubidtest"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/hub/workers"
	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/transport"
)

// RC1 release gate, finding 1: the administrator override of a repair (design
// 12.3) is decided with the call's principal on the existing Worker. An API key
// of an administrator that is scoped to the administrator's own user resource
// holds no operate on another owner's Worker and cannot complete its repair;
// the administrator's full session can.
func TestAScopedAdminKeyCannotCompleteAnotherOwnersRepair(t *testing.T) {
	t.Setenv("MINION_ADMIN_USERNAME", "root")
	t.Setenv("MINION_ADMIN_PASSWORD", "repair-scope-pw-1")
	var (
		st  *store.Store
		mgr *session.Manager
		svc *workers.Service
	)
	reg := productionRegistry(t)
	if err := reg.Register("grab", 90, func(_ context.Context, env *Env) error {
		var err error
		if st, err = Require[*store.Store](env); err != nil {
			return err
		}
		if svc, err = Require[*workers.Service](env); err != nil {
			return err
		}
		mgr, err = Require[*session.Manager](env)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	data := hubstore.Dir(t)
	hubidtest.Provision(t, data)
	h := startHub(t, &Config{DataDir: data}, reg)
	ah := &apiHub{h: h, hc: newHubClient(t, h, "https"), st: st, mgr: mgr}
	ctx := context.Background()

	alice := ah.user(t, "alice")
	root, err := st.UserByIdentifier(ctx, "root")
	if err != nil {
		t.Fatal(err)
	}
	key, err := identity.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	w := store.WorkerRow{ID: store.NewID(), Pub: key.Public(), Name: "stuck", OwnerID: alice, Kind: store.WorkerKindPrivate, CreatedAt: time.Now().UnixMilli()}
	if err := st.InsertWorker(ctx, nil, w); err != nil {
		t.Fatal(err)
	}
	if err := st.SetRepairWindow(ctx, w.ID, time.Now().Add(workers.RepairWindow).UnixMilli()); err != nil {
		t.Fatal(err)
	}

	tok, csrf := ah.login(t, "root")
	rootSession := asSession(tok, csrf)
	scoped, _ := ah.newKey(t, rootSession, map[string]any{
		"name": "own space", "resources": []map[string]any{{"kind": "user", "user_id": root.ID}}, "actions": []string{"operate"},
	})

	// dial runs the Worker in pair_code mode and waits until it is held.
	dial := func(code identity.PairingCode) <-chan error {
		t.Helper()
		done := make(chan error, 1)
		go func() {
			dctx, cancel := context.WithTimeout(ctx, 60*time.Second)
			defer cancel()
			conn, err := transport.Dial(dctx, "https://"+h.srv.Addr())
			if err != nil {
				done <- err
				return
			}
			res, err := transport.WorkerHandshake(conn, transport.WorkerConfig{
				Key: key, Name: "stuck", Ops: []string{"send_text"}, Mode: transport.ModePairCode,
				PairingCode: code, Timeout: 30 * time.Second,
			})
			if res != nil {
				_ = res.Channel.Close()
			}
			done <- err
		}()
		deadline := time.Now().Add(20 * time.Second)
		for svc.Pairings() == nil || !svc.Pairings().IsPending(code.PairingID()) {
			if time.Now().After(deadline) {
				t.Fatal("the Hub never held the Worker as pending")
			}
			time.Sleep(10 * time.Millisecond)
		}
		return done
	}
	claim := func(code identity.PairingCode, as map[string]string) apiResp {
		t.Helper()
		return ah.call(t, "POST", "/api/v1/ops/"+ops.OpPairingClaim, map[string]any{"request_id": ops.NewUUIDv7(), "code": code.String()}, as)
	}

	code := identity.NewPairingCode()
	done := dial(code)
	if r := claim(code, asKey(scoped)); r.code != proto.HTTPStatus(proto.CodeAlreadyPaired) || r.err.Code != proto.CodeAlreadyPaired {
		t.Fatalf("a scoped admin key completed another owner's repair: %d %s", r.code, r.body)
	}
	// The CPace handshake itself succeeds (the code is right); the Hub refuses
	// after it, so the handshake result says nothing here.
	if err := <-done; err != nil {
		t.Logf("the Worker side of the refused repair: %v", err)
	}
	cur, err := st.WorkerByID(ctx, w.ID)
	if err != nil || cur.RepairUntil == 0 || cur.OwnerID != alice {
		t.Fatalf("after the refused claim the Worker is %+v (%v)", cur, err)
	}

	code2 := identity.NewPairingCode()
	done = dial(code2)
	if r := claim(code2, rootSession); r.code != 200 {
		t.Fatalf("the administrator's session repair = %d %s", r.code, r.body)
	}
	if err := <-done; err != nil {
		t.Fatalf("the Worker handshake of the repair failed: %v", err)
	}
	cur, err = st.WorkerByID(ctx, w.ID)
	if err != nil || cur.RepairUntil != 0 || cur.OwnerID != alice {
		t.Fatalf("after the repair the Worker is %+v (%v)", cur, err)
	}
}
