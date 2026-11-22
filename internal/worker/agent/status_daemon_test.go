package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
)

func loadSchema(t *testing.T, name string) *jsonschema.Resolved {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "docs", "schemas", name))
	if err != nil {
		t.Fatal(err)
	}
	var s jsonschema.Schema
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	r, err := s.Resolve(nil)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return r
}

func readFixture(t *testing.T, path ...string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(path...))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// reportFromReplyFixture runs the CLI path: reply bytes from the socket, then
// the status report.
func reportFromReplyFixture(t *testing.T, name string) (StatusReport, []byte) {
	t.Helper()
	var reply LiveStatusReply
	if err := json.Unmarshal(readFixture(t, "testdata", name), &reply); err != nil {
		t.Fatal(err)
	}
	live, err := reply.LiveStatus()
	if err != nil {
		t.Fatal(err)
	}
	in := pairedInputs(t)
	in.Version = "v0.18.0" // the CLI
	in.Live = fakeLive{ls: live}
	r := BuildStatus(context.Background(), in)
	_, js := render(t, r)
	return r, js
}

func TestStatusDaemonNewDaemonOldCLIFields(t *testing.T) {
	// An old CLI decodes the reply with the pre-contract struct; the new
	// daemon{} member must be ignored and nothing else may change.
	var old struct {
		StateDir string `json:"state_dir"`
		State    string `json:"state"`
		Instance string `json:"instance"`
		Paused   *bool  `json:"paused"`
	}
	if err := json.Unmarshal(readFixture(t, "testdata", "live_reply_new_daemon.json"), &old); err != nil {
		t.Fatal(err)
	}
	if old.State != "connected" || old.Instance == "" || old.Paused == nil {
		t.Fatalf("old reader lost fields: %+v", old)
	}
}

func TestStatusDaemonOldDaemonNewCLI(t *testing.T) {
	r, js := reportFromReplyFixture(t, "live_reply_old_daemon.json")
	if r.Daemon == nil || r.Daemon.Instance != "11111111-1111-4111-8111-111111111111" {
		t.Fatalf("daemon = %+v", r.Daemon)
	}
	if r.Daemon.Version != "" || r.Daemon.StartedAt != nil || r.Daemon.PID != 0 {
		t.Fatalf("an old daemon must not invent version, start or pid: %+v", r.Daemon)
	}
	if r.Version != "v0.18.0" || !r.Connection.Live {
		t.Fatalf("top-level version or connection.live changed: %q %+v", r.Version, r.Connection)
	}
	validateDaemon(t, js)
}

func TestStatusDaemonStates(t *testing.T) {
	for _, c := range []struct {
		fixture  string
		pending  bool
		paused   bool
		live     bool
		started  string
		hasPID   bool
		solo     bool
		approval string
	}{
		{"live_reply_new_daemon.json", false, false, true, "2026-11-10T11:55:00.123Z", true, false, ApprovalApproved},
		{"live_reply_pending.json", true, false, true, "2026-11-10T11:55:00.000Z", true, false, ApprovalPending},
		{"live_reply_paused.json", false, true, true, "2026-11-10T11:55:00.000Z", true, false, ApprovalApproved},
		{"live_reply_solo.json", false, false, true, "2026-11-10T11:55:00.000Z", false, true, ApprovalUnknown},
	} {
		t.Run(c.fixture, func(t *testing.T) {
			r, js := reportFromReplyFixture(t, c.fixture)
			if r.Daemon == nil || r.Daemon.Version != "v0.19.0" || r.Daemon.StartedAt == nil || (r.Daemon.PID != 0) != c.hasPID {
				t.Fatalf("daemon = %+v", r.Daemon)
			}
			b, _ := r.Daemon.StartedAt.MarshalJSON()
			if string(b) != `"`+c.started+`"` {
				t.Fatalf("started_at = %s", b)
			}
			if r.Version != "v0.18.0" {
				t.Fatalf("top-level version = %q: it stays the CLI's", r.Version)
			}
			if r.Connection.Live != c.live || r.Paused != c.paused {
				t.Fatalf("connection = %+v paused = %v", r.Connection, r.Paused)
			}
			if !c.solo && r.Approval != c.approval {
				t.Fatalf("approval = %q", r.Approval)
			}
			validateDaemon(t, js)
		})
	}
}

func TestStatusDaemonStoppedHasNone(t *testing.T) {
	in := pairedInputs(t)
	in.Live = fakeLive{err: ErrNoLiveWorker}
	r := BuildStatus(context.Background(), in)
	if r.Daemon != nil || r.Connection.Live {
		t.Fatalf("a stopped Worker has no daemon and no live connection: %+v %+v", r.Daemon, r.Connection)
	}
	_, js := render(t, r)
	var m map[string]json.RawMessage
	if err := json.Unmarshal(js, &m); err != nil {
		t.Fatal(err)
	}
	if _, ok := m["daemon"]; ok {
		t.Fatal("daemon{} must be absent when no daemon answered")
	}
	// A failing socket is not a daemon either.
	in.Live = fakeLive{err: errors.New("untrusted socket")}
	if BuildStatus(context.Background(), in).Daemon != nil {
		t.Fatal("daemon set although the socket failed")
	}
}

func TestStatusDaemonGolden(t *testing.T) {
	in := pairedInputs(t)
	in.Live = fakeLive{ls: LiveStatus{PairingKnown: true, ApprovalKnown: true,
		Daemon:    &LiveDaemon{Instance: "22222222-2222-4222-8222-222222222222", Version: "v0.19.0", StartedAt: statusTime.Add(-5 * time.Minute), PID: 4242},
		Connector: Status{State: ConnConnected, ConnectedSince: statusTime}}}
	text, js := render(t, BuildStatus(context.Background(), in))
	golden(t, "status_daemon.txt.golden", text)
	golden(t, "status_daemon.json.golden", js)
	validateDaemon(t, js)
}

func TestLiveStatusReplyCarriesDaemon(t *testing.T) {
	started := time.Date(2026, 11, 10, 11, 55, 0, 5, time.UTC)
	src := LiveStatusSource{Daemon: &LiveDaemon{Instance: "x", Version: "v0.19.0", StartedAt: started, PID: 9}}
	raw, err := statusOp(context.Background(), "/s", "inst-1", src)
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		Instance string      `json:"instance"`
		Daemon   *DaemonInfo `json:"daemon"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if m.Daemon == nil || m.Daemon.Instance != "inst-1" || m.Instance != "inst-1" || m.Daemon.Version != "v0.19.0" || m.Daemon.PID != 9 {
		t.Fatalf("reply = %s", raw)
	}
	var generic map[string]any
	_ = json.Unmarshal(raw, &generic)
	if err := loadSchema(t, "local-status-daemon.schema.json").Validate(generic["daemon"]); err != nil {
		t.Fatalf("daemon does not match its schema: %v\n%s", err, raw)
	}
	// Without a daemon source the reply has no daemon{}.
	raw, _ = statusOp(context.Background(), "/s", "inst-1", LiveStatusSource{})
	generic = nil
	if _ = json.Unmarshal(raw, &generic); generic["daemon"] != nil {
		t.Fatalf("daemon present without a source: %s", raw)
	}
}

func TestLiveStatusReplyRejectsBadDaemonStart(t *testing.T) {
	r := LiveStatusReply{State: "idle", Daemon: &DaemonInfo{Instance: "i", StartedAt: "yesterday"}}
	if _, err := r.LiveStatus(); err == nil {
		t.Fatal("want an error for an invalid started_at")
	}
}

func validateDaemon(t *testing.T, js []byte) {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(js, &m); err != nil {
		t.Fatal(err)
	}
	if err := loadSchema(t, "local-status-daemon.schema.json").Validate(m["daemon"]); err != nil {
		t.Fatalf("daemon does not match its schema: %v\n%s", err, js)
	}
}
