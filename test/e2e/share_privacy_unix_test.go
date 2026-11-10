//go:build e2e && unix

package e2e

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/api"
	"github.com/patrick-1984/ai-minion/internal/hub/workers"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/pipe"
	wstore "github.com/patrick-1984/ai-minion/internal/worker/store"
)

// Story 4.15, privacy hardening, composed: the default Hub (strict: it keeps the
// share rule of every Worker in its database) with a real Worker over a private
// tmux. History that the Hub stored of a session is shown only while the
// Worker's current rule shares it: a change made on the Worker, a share_set
// through the Hub followed by a reconnect, and a Hub that starts from its
// database all leave the unshared session's history hidden, while the session
// that is still shared keeps working.

const privateReply = "SYNTHETIC-PRIVATE-REPLY"

// assertBetaIsPrivate asks every public surface of the Hub, and the Worker's
// own, about the unshared session beta and checks that none of them shows it:
// pipe_read, pipe_wait, snapshot, capture, the session list op, the REST session
// list.
func (e *shareE2E) assertBetaIsPrivate() {
	e.t.Helper()
	for _, op := range []string{ops.OpPipeRead, ops.OpSessionSnapshot, ops.OpSessionCapture} {
		if code, body := e.read(e.owner, op, "beta"); code != 403 || strings.Contains(body, privateReply) || strings.Contains(body, secretText) {
			e.t.Fatalf("%s of the unshared session = %d %s", op, code, body)
		}
	}
	code, body := e.rest(e.owner, ops.OpPipeWait, map[string]any{"target": map[string]any{"worker_id": e.wid, "session_name": "beta"}, "timeout_ms": 100})
	if code != 403 || strings.Contains(body, privateReply) {
		e.t.Fatalf("pipe_wait of the unshared session = %d %s", code, body)
	}
	if code, body := e.rest(e.owner, ops.OpSessionsList, map[string]any{"worker_id": e.wid}); code != 200 || strings.Contains(body, `"beta"`) {
		e.t.Fatalf("sessions_list = %d %s", code, body)
	}
	if code, body := e.get(e.owner, api.SessionsPath); code != 200 || strings.Contains(body, `"beta"`) {
		e.t.Fatalf("the session list of the Hub = %d %s", code, body)
	}
}

func (e *shareE2E) putTurn(session, text string) {
	e.t.Helper()
	id, err := pipe.NewTurnID(time.Now())
	if err != nil {
		e.t.Fatal(err)
	}
	if _, err := e.ph.st.Pipe().PutTurn(context.Background(), pipe.Turn{TurnID: id, Peer: pipe.Peer{WorkerID: e.wid, SessionName: session}, Text: text, At: time.Now()}); err != nil {
		e.t.Fatal(err)
	}
}

// reconnect replaces the Worker's connection and waits for the new connection's
// snapshot: the Hub's event feed says worker_resynced when a snapshot replaced
// the Worker's state, which cannot be the old connection's.
func (e *shareE2E) reconnect() {
	e.t.Helper()
	sub := e.ph.events.Subscribe(256)
	defer sub.Close()
	e.ph.wsv.Registry().Disconnect(e.wid)
	deadline := time.After(15 * time.Second)
	for {
		select {
		case ev, ok := <-sub.C():
			if !ok {
				e.t.Fatalf("the event subscription ended: %v", sub.Err())
			}
			if ev.Type == workers.EventWorkerResynced && ev.WorkerID == e.wid {
				waitFor(e.t, "the Worker online again", func() bool {
					ws, ok := e.ph.events.Sessions(e.wid)
					return ok && ws.Online
				})
				return
			}
		case <-deadline:
			e.t.Fatal("the Worker did not reconnect with a new snapshot")
		}
	}
}

func (e *shareE2E) historyHidden(session string) bool {
	e.t.Helper()
	code, body := e.read(e.owner, ops.OpPipeRead, session)
	if code == 200 && strings.Contains(body, privateReply) {
		return false
	}
	return code == 403
}

func (e *shareE2E) historyShown(session string) bool {
	e.t.Helper()
	code, body := e.read(e.owner, ops.OpPipeRead, session)
	return code == 200 && strings.Contains(body, privateReply+"-"+session)
}

func TestShareE2EPrivateHistoryFollowsTheWorkersRule(t *testing.T) {
	for _, scenario := range []string{"changed-on-the-worker", "hub-share-set-then-reconnect", "hub-share-set-then-hub-restart"} {
		t.Run(scenario, func(t *testing.T) {
			e := startShareE2E(t, nil)
			e.putTurn("alpha", privateReply+"-alpha")
			e.putTurn("beta", privateReply+"-beta")
			if !e.historyShown("alpha") || !e.historyShown("beta") {
				t.Fatal("setup: both sessions are shared")
			}

			switch scenario {
			case "changed-on-the-worker":
				// The owner unshares beta on the Worker's own machine: no Hub call.
				if err := e.wstore.SaveShare(wstore.ShareSelection{Mode: wstore.ShareSelect, Sessions: []string{"alpha"}}); err != nil {
					t.Fatal(err)
				}
				waitFor(t, "the Hub to learn the rule from the Worker", func() bool { return e.historyHidden("beta") })
			default:
				if code, _, body := e.shareSet(e.owner, "select", "alpha"); code != 200 {
					t.Fatalf("share_set = %d %s", code, body)
				}
				if !e.historyHidden("beta") {
					t.Fatal("history of beta was readable after share_set returned")
				}
			}
			e.assertBetaIsPrivate()

			switch scenario {
			case "hub-share-set-then-reconnect", "changed-on-the-worker":
				// The connection is replaced: the Worker's new snapshot lists only alpha.
				e.reconnect()
				if ws, _ := e.ph.events.Sessions(e.wid); len(ws.Sessions) != 1 || ws.Sessions[0].Name != "alpha" {
					t.Fatalf("the new snapshot = %+v, want only alpha", ws.Sessions)
				}
				if !e.historyHidden("beta") {
					t.Fatal("history of beta became readable after the Worker reconnected")
				}
			case "hub-share-set-then-hub-restart":
				// A Hub that starts from its database, before any Worker has
				// connected to it, still hides what the Worker stopped sharing.
				m, err := e.ph.st.LoadWorkerShares(context.Background())
				if err != nil || m[e.wid].Rule.Mode != "select" || len(m[e.wid].Rule.Names) != 1 || m[e.wid].Rule.Names[0] != "alpha" {
					t.Fatalf("the rule in the Hub database = %+v %v", m[e.wid], err)
				}
				fresh := workers.NewEventHub(workers.EventHubOptions{Share: e.ph.st})
				if !fresh.Fenced(e.wid, "beta") || fresh.Fenced(e.wid, "alpha") {
					t.Fatal("a Hub restarted from its database shows the wrong sessions")
				}
			}
			if code, body := e.read(e.owner, ops.OpPipeRead, "alpha"); code != 200 || !strings.Contains(body, privateReply+"-alpha") {
				t.Fatalf("the session that is still shared lost its history: %d %s", code, body)
			}

			// Sharing everything again serves what was kept: the rule decides.
			if code, _, body := e.shareSet(e.owner, "all"); code != 200 {
				t.Fatalf("share_set all = %d %s", code, body)
			}
			if !e.historyShown("beta") {
				t.Fatal("history of beta stayed hidden after the owner shared everything again")
			}
		})
	}
}

// An acknowledged unshare holds against reads that run at the same moment as
// the changes: a bounded race of share_set none/all with readers of the history
// (the real Hub, Worker and tmux). A read that started after share_set none was
// acknowledged, with no later share_set dispatched before the read ended, never
// returns the content.
func TestShareE2EAcknowledgedUnshareHoldsUnderConcurrentReads(t *testing.T) {
	e := startShareE2E(t, nil)
	e.putTurn("beta", privateReply+"-beta")

	var mu sync.Mutex
	gen, privateStable := 0, false
	stop := make(chan struct{})
	var wg sync.WaitGroup
	violations := make(chan string, 16)

	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				mu.Lock()
				g0, p0 := gen, privateStable
				mu.Unlock()
				code, body := e.read(e.owner, ops.OpPipeRead, "beta")
				mu.Lock()
				g1 := gen
				mu.Unlock()
				if p0 && g0 == g1 && code == 200 && strings.Contains(body, privateReply) {
					select {
					case violations <- body:
					default:
					}
					return
				}
			}
		}()
	}
	deadline := time.Now().Add(4 * time.Second)
	for i := 0; time.Now().Before(deadline); i++ {
		mode, names := "none", []string(nil)
		if i%2 == 1 {
			mode = "all"
		}
		mu.Lock()
		gen++
		privateStable = false
		mu.Unlock()
		code, _, body := e.shareSet(e.owner, mode, names...)
		if code != 200 {
			t.Errorf("share_set %s = %d %s", mode, code, body)
			break
		}
		mu.Lock()
		gen++
		privateStable = mode == "none"
		mu.Unlock()
		time.Sleep(30 * time.Millisecond)
	}
	close(stop)
	wg.Wait()
	select {
	case body := <-violations:
		t.Fatalf("a read that started after the unshare was acknowledged returned the private history: %s", body)
	default:
	}
}
