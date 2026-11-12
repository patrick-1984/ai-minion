//go:build e2e && unix

package e2e

import (
	"context"
	"html"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/testutil/tmuxtest"
	"github.com/patrick-1984/ai-minion/internal/transport"
	"github.com/patrick-1984/ai-minion/internal/worker/agent"
	"github.com/patrick-1984/ai-minion/internal/worker/agentdetect"
	wstore "github.com/patrick-1984/ai-minion/internal/worker/store"
	"github.com/patrick-1984/ai-minion/internal/worker/term/tmux"
)

// F-01 provisioning (T2 round 2): the call key of a Worker is created in the
// browser flow only. The signed-in owner binds a key to one of their Workers
// in the API key form; the key goes into call_api_key_file; the Worker then
// calls a session of another Worker through the real Hub, and the receiver
// delivers it only when its pipe_accept_from allows the sender.

type callPeer struct {
	wid    string
	tmux   *tmuxtest.Server
	policy *agent.Policy
	cc     *agent.CallClient
}

// startCallPeer starts a real Worker (connector, op server, executor over a
// private tmux with the session "work") owned by ownerID. keyFile is its
// call_api_key_file ("" for none).
func startCallPeer(t *testing.T, ph *shareHub, ownerID, name, keyFile string) *callPeer {
	t.Helper()
	dir := t.TempDir()
	st, err := wstore.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.SaveShare(wstore.ShareSelection{Mode: wstore.ShareAll}); err != nil {
		t.Fatal(err)
	}
	enr := agent.NewEnrollments(st, dir)
	key, err := enr.LoadOrCreateKey()
	if err != nil {
		t.Fatal(err)
	}
	if err := enr.Commit(context.Background(), agent.Enrollment{Pin: identity.Pin{Pub: ph.wsv.Identity().Public()}}); err != nil {
		t.Fatal(err)
	}
	p := &callPeer{wid: store.NewID(), tmux: tmuxtest.New(t)}
	if err := ph.st.InsertWorker(context.Background(), nil, store.WorkerRow{ID: p.wid, Pub: key.Public(), Name: name, OwnerID: ownerID,
		Kind: store.WorkerKindPrivate, CreatedAt: time.Now().UnixMilli()}); err != nil {
		t.Fatal(err)
	}
	// An agent session: a script named claude (which the detector knows) that
	// echoes what it is typed.
	claude := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(claude, []byte("#!/bin/sh\ncat\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	p.tmux.NewSession("work", claude)
	host, err := tmux.New(p.tmux.Socket())
	if err != nil {
		t.Fatal(err)
	}
	p.policy = agent.NewPolicy(st)
	ex, err := agent.NewExecutor(agent.Config{Host: host, Classifier: agentdetect.New(), State: st, Policy: p.policy, LockWait: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	srv, err := agent.NewOpServer(agent.OpServerConfig{Executor: ex, Policy: p.policy})
	if err != nil {
		t.Fatal(err)
	}
	obs, err := agent.NewHostObserver(agent.HostObserverConfig{Host: host, Classifier: agentdetect.New(), Policy: p.policy})
	if err != nil {
		t.Fatal(err)
	}
	pub, err := agent.NewEventPublisher(agent.EventConfig{Observer: obs, Interval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	// The call client as the hub-pipe module builds it: the key is read from
	// call_api_key_file on every call.
	var keySrc func() (string, error)
	if keyFile != "" {
		keySrc = func() (string, error) {
			k, err := identity.ReadAPIKeyFile(keyFile)
			return k.String(), err
		}
	}
	p.cc = agent.NewCallClient(agent.CallConfig{Key: keySrc})
	conn, err := agent.NewConnector(agent.ConnectorConfig{
		HubURL: "https://" + ph.addr, Name: name, Ops: ex.Ops, Features: ex.Features, Key: enr.LoadKey, Enrollment: enr, Policy: p.policy,
		Dialer:   agent.NewHubDialer(),
		Backoff:  &transport.Backoff{Initial: 5 * time.Millisecond, Max: 40 * time.Millisecond},
		IdlePoll: 20 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	conn.OnLink("op-server", srv.Serve)
	conn.OnLink("events", pub.Serve)
	conn.OnLink("hub-call", p.cc.Serve)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); conn.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		<-done
		_ = srv.Stop(context.Background())
	})
	waitFor(t, name+" online", func() bool { return ph.wsv.Registry().IsOnline(p.wid) })
	waitFor(t, name+" call stream", p.cc.Connected)
	waitFor(t, name+" session in the Hub", func() bool {
		ws, ok := ph.events.Sessions(p.wid)
		return ok && len(ws.Sessions) == 1
	})
	return p
}

func (p *callPeer) pane(t *testing.T) string {
	t.Helper()
	return p.tmux.Capture("=work:")
}

var apiKeyInHTML = regexp.MustCompile(`id="api-key" value="(mn_[^"]+)"`)

func TestCallKeyCreatedInTheBrowserFormAllowsACrossWorkerPipeSend(t *testing.T) {
	ph := startShareHub(t)
	ownerID, owner := (&shareE2E{t: t, ph: ph}).user("carol")
	keyFile := filepath.Join(t.TempDir(), "call.key") // call_api_key_file of the sender; written below
	sender := startCallPeer(t, ph, ownerID, "sender", keyFile)
	receiver := startCallPeer(t, ph, ownerID, "receiver", "")

	// 1. The signed-in owner creates the key in the browser form: bound to the
	// sending Worker, reaching one session of the receiver, message and read.
	form := url.Values{
		"name": {"sender-call"}, "resource": {"worker:" + receiver.wid}, "session": {"work"},
		"action": {"message", "read"}, "expires": {"30d"}, "bound_worker": {sender.wid},
	}
	code, page := ph.postForm("/api-keys", form.Encode(), owner)
	m := apiKeyInHTML.FindStringSubmatch(page)
	if code != 200 || m == nil {
		t.Fatalf("POST /api-keys = %d %s", code, page)
	}
	key := html.UnescapeString(m[1])
	if !strings.Contains(page, "bound") && !strings.Contains(page, "created") {
		t.Fatalf("unexpected page: %s", page)
	}
	rows, err := ph.st.ListAPIKeys(context.Background(), ownerID)
	if err != nil || len(rows) != 1 || rows[0].BoundWorkerID != sender.wid {
		t.Fatalf("stored key = %+v, %v; want one key bound to the sender", rows, err)
	}
	// A key still cannot mint keys.
	if c, body := ph.do("POST", "/api/v1/ops/apikey_create", map[string]any{"name": "x", "resources": []map[string]string{{"kind": "user", "user_id": ownerID}}, "actions": []string{"read"}}, asKey(key)); c == 200 {
		t.Fatalf("a key created a key: %s", body)
	}

	// 2. The key goes into the sender's call_api_key_file; the Worker's call
	// client reads it for each call.
	if err := identity.WritePrivate(keyFile, []byte(key+"\n")); err != nil {
		t.Fatal(err)
	}

	send := func(text string) error {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		var res ops.WriteResult
		err := sender.cc.Do(ctx, ops.OpPipeSend, map[string]any{
			"request_id": ops.NewUUIDv7(), "target": map[string]any{"worker_id": receiver.wid, "session_name": "work"}, "text": text,
		}, &res)
		if err == nil && res.Outcome != ops.OutcomeDelivered {
			t.Fatalf("outcome %+v", res)
		}
		return err
	}

	// 3. The receiver's default pipe_accept_from is none: refused, nothing typed.
	err = send("REFUSED-BY-DEFAULT")
	if pe, ok := err.(*proto.Error); !ok || pe.Code != proto.CodePipeOriginRefused {
		t.Fatalf("default pipe_accept_from: err = %v, want pipe_origin_refused", err)
	}
	if strings.Contains(receiver.pane(t), "REFUSED-BY-DEFAULT") {
		t.Fatal("a refused message reached the pane")
	}

	// 4. The receiver allows the owner's Workers: the same call succeeds.
	acc, err := config.ParsePipeAcceptFrom("owner-workers")
	if err != nil {
		t.Fatal(err)
	}
	receiver.policy.SetPipeAcceptFrom(acc)
	if err := send("HELLO-FROM-THE-SENDER"); err != nil {
		t.Fatalf("cross-Worker pipe_send with the browser-created call key: %v", err)
	}
	waitFor(t, "the message in the receiver's pane", func() bool { return strings.Contains(receiver.pane(t), "HELLO-FROM-THE-SENDER") })
}
