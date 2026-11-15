package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// The JSON status carries what troubleshooting needs: connector state and
// reason, Hub URL, last error and its time, enrollment, Worker id and the
// approval state.
func TestStatusJSONCarriesTheTroubleshootingFields(t *testing.T) {
	in := pairedInputs(t)
	in.Enrollment = fakeEnrollment{}
	in.Live = fakeLive{ls: LiveStatus{ApprovalKnown: true, Connector: Status{
		State: ConnConnected, ConnectedSince: statusTime,
		Welcome: HubWelcome{WorkerID: "w-live", Pending: true},
	}}}
	_, js := render(t, BuildStatus(context.Background(), in))
	var got struct {
		HubURL     string `json:"hub_url"`
		Paired     *bool  `json:"paired"`
		WorkerID   string `json:"worker_id"`
		Approval   string `json:"approval"`
		Connection struct {
			State string `json:"state"`
		} `json:"connection"`
	}
	if err := json.Unmarshal(js, &got); err != nil {
		t.Fatal(err)
	}
	if got.HubURL == "" || got.Paired == nil || got.WorkerID != "w-live" || got.Approval != ApprovalPending || got.Connection.State != "connected" {
		t.Errorf("status = %s", js)
	}

	in = pairedInputs(t)
	in.Live = fakeLive{ls: LiveStatus{Connector: Status{
		State: ConnBackoff, Reason: "", LastError: "dial: refused", LastErrorAt: statusTime,
	}}}
	_, js = render(t, BuildStatus(context.Background(), in))
	var off map[string]any
	if err := json.Unmarshal(js, &off); err != nil {
		t.Fatal(err)
	}
	conn, _ := off["connection"].(map[string]any)
	if off["worker_id"] != "w1" || off["approval"] != ApprovalUnknown || off["paired"] != true ||
		conn["last_error"] != "dial: refused" || conn["last_error_at"] == nil || conn["state"] != "backoff" {
		t.Errorf("status = %s", js)
	}
}

// The live reply of the local socket carries the Worker id and the approval
// state of the current connection.
func TestLiveStatusReplyCarriesWorkerIDAndPending(t *testing.T) {
	reply := NewLiveStatusReply("/s", LiveStatus{Connector: Status{
		State: ConnConnected, Welcome: HubWelcome{WorkerID: "w9", Pending: true},
	}})
	raw, err := json.Marshal(reply)
	if err != nil {
		t.Fatal(err)
	}
	var back LiveStatusReply
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	ls, err := back.LiveStatus()
	if err != nil || ls.Connector.Welcome.WorkerID != "w9" || !ls.Connector.Welcome.Pending || !ls.ApprovalKnown {
		t.Errorf("live = %+v, %v (wire %s)", ls, err, raw)
	}

	// An approved Worker sends "pending": false, which is known.
	raw, _ = json.Marshal(NewLiveStatusReply("/s", LiveStatus{Connector: Status{State: ConnConnected}}))
	if !strings.Contains(string(raw), `"pending":false`) {
		t.Errorf("an approved Worker's reply = %s, want pending false", raw)
	}
}

// A running Worker of an older version answers without worker_id and pending:
// the approval is then unknown, not approved.
func TestStatusApprovalIsUnknownForAnOlderWorkerReply(t *testing.T) {
	old := []byte(`{"state_dir":"/s","state":"connected","connected_since":"2026-11-10T12:30:00Z","owner":"alice","hub_preview_only":false,"pairing_known":true}`)
	var reply LiveStatusReply
	if err := json.Unmarshal(old, &reply); err != nil {
		t.Fatal(err)
	}
	ls, err := reply.LiveStatus()
	if err != nil {
		t.Fatal(err)
	}
	in := pairedInputs(t)
	in.Live = fakeLive{ls: ls}
	r := BuildStatus(context.Background(), in)
	if r.Approval != ApprovalUnknown || r.Connection.State != "connected" {
		t.Errorf("approval = %q, state = %q; want unknown, connected", r.Approval, r.Connection.State)
	}

	// The same reply from a current Worker says approved.
	cur := []byte(`{"state_dir":"/s","state":"connected","worker_id":"w1","pending":false,"hub_preview_only":false,"pairing_known":true}`)
	if err := json.Unmarshal(cur, &reply); err != nil {
		t.Fatal(err)
	}
	if ls, err = reply.LiveStatus(); err != nil {
		t.Fatal(err)
	}
	in.Live = fakeLive{ls: ls}
	if r := BuildStatus(context.Background(), in); r.Approval != ApprovalApproved {
		t.Errorf("approval = %q, want approved", r.Approval)
	}
}
