package agent

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/ops"
)

// TestSessionsListLeavesOutInstanceIDsThatWouldNotFit (review r2): 50 shared
// sessions with 1024-byte tasks fit the bound-master cap without instance ids
// and exceed it with them. The listing then comes without the ids, exactly as
// a release before them returned it, instead of being refused; a small
// listing keeps them.
func TestSessionsListLeavesOutInstanceIDsThatWouldNotFit(t *testing.T) {
	e := newEnv(t)
	e.ex.host = instanceHost{e.host, true}
	task := strings.Repeat("t", 1024)
	for i := range 50 {
		e.host.addSession(fmt.Sprintf("session-%02d-build", i), "bash")
	}
	e.host.mu.Lock()
	for i := range e.host.sessions {
		e.host.sessions[i].Task = task
		e.host.sessions[i].Instance = fmt.Sprintf("t1-%032x", i+1)
	}
	e.host.mu.Unlock()

	raw, err := e.call(ops.OpSessionsList, allActions, ops.SessionsListArgs{})
	if err != nil {
		t.Fatal(err)
	}
	var res ops.SessionsListResult
	if err := json.Unmarshal(raw, &res); err != nil || len(res.Sessions) != 50 {
		t.Fatalf("sessions_list: %d sessions, %v", len(res.Sessions), err)
	}
	if strings.Contains(string(raw), "instance_id") {
		t.Fatalf("an oversized listing kept its instance ids (%d bytes)", len(raw))
	}
	if testing.Verbose() {
		t.Logf("listing without ids: %d bytes", len(raw))
	}
	if len(raw) > maxListingWithInstanceIDs {
		t.Fatalf("the listing without ids is %d bytes; the fixture must fit the cap", len(raw))
	}
	for i := range res.Sessions {
		res.Sessions[i].InstanceID = fmt.Sprintf("t1-%032x", i+1)
	}
	with, err := json.Marshal(res)
	if err != nil || len(with) <= maxListingWithInstanceIDs {
		t.Fatalf("with ids the listing is %d bytes, %v; the fixture must exceed the cap", len(with), err)
	}

	// A listing that fits keeps every id.
	e.host.mu.Lock()
	e.host.sessions = e.host.sessions[:10]
	e.host.mu.Unlock()
	raw, err = e.call(ops.OpSessionsList, allActions, ops.SessionsListArgs{})
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(raw), `"instance_id"`); n != 10 {
		t.Fatalf("a listing that fits has %d instance ids, want 10", n)
	}
}
