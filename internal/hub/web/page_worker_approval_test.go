package web

// F-02: the Workers list, the Worker page and the two buttons of a Worker that
// waits for approval.

import (
	"bytes"
	"net/url"
	"strings"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/ops"
)

var wPending = WorkerRow{
	ID: "w9", Name: "stolen-box", Owner: "alice", Kind: ops.WorkerKindPrivate, Online: true, Version: "2.0",
	Pending: true, Fingerprint: "fp" + strings.Repeat("a", 50), EnrollAddr: "203.0.113.77",
}

func TestWorkersListShowsThePendingBannerFactsAndButtons(t *testing.T) {
	d := Dashboard{Workers: []WorkerRow{wPrivate, wPending}}
	v := newDashboardView(d, false)
	if v.PendingWorkers != 1 || v.OnlineWorkers != 1 {
		t.Fatalf("pending = %d online = %d: a pending Worker is not an online Worker", v.PendingWorkers, v.OnlineWorkers)
	}
	var buf bytes.Buffer
	if err := fragmentTemplates.ExecuteTemplate(&buf, "workers_results", v); err != nil {
		t.Fatal(err)
	}
	body := buf.String()
	mustHave(t, body,
		"data-pending-banner", "1 worker is waiting for approval",
		"pending approval", wPending.Fingerprint, "203.0.113.77", "stolen-box",
		`hx-post="/workers/w9/approve"`, `name="fingerprint" value="`+wPending.Fingerprint+`"`,
		`hx-post="/workers/w9/remove"`, ">Approve<", ">Remove<")
	// No "New session" for it, but its approved neighbour keeps the button.
	if strings.Count(body, `data-control="new-session"`) != 1 {
		t.Errorf("new-session buttons = %d, want only the approved Worker's", strings.Count(body, `data-control="new-session"`))
	}
	if len(v.Attention) == 0 || !strings.Contains(v.Attention[0].Text, "waiting for approval") {
		t.Errorf("attention = %+v", v.Attention)
	}
}

func TestWorkerPageOffersApprovalToTheOperatorOnly(t *testing.T) {
	d := Dashboard{Workers: []WorkerRow{wPending}}
	for _, operate := range []bool{true, false} {
		v := newWorkerView(d, false, "w9", operate)
		out, err := renderWorkerFragment(v)
		if err != nil {
			t.Fatal(err)
		}
		mustHave(t, out, "pending approval", wPending.Fingerprint, "203.0.113.77", "data-pending-banner")
		if operate {
			mustHave(t, out, `hx-post="/workers/w9/approve"`, `hx-post="/workers/w9/remove"`)
		} else {
			mustNotHave(t, out, `hx-post="/workers/w9/approve"`, `hx-post="/workers/w9/remove"`)
		}
		mustNotHave(t, out, `data-control="new-session"`)
	}
}

func TestApproveAndRemoveButtonsCallTheCatalogOperations(t *testing.T) {
	f := newFakeOps().reply(ops.OpWorkerApprove, ops.WriteResult{Outcome: ops.OutcomeDelivered}).
		reply(ops.OpWorkerRemove, ops.WriteResult{Outcome: ops.OutcomeDelivered})
	connect(t, f)
	s := onbUser(t)
	fp := strings.Repeat("a", 52)
	rec := onbPost(t, s, "/workers/"+ops.NewUUIDv7()+"/approve", url.Values{"fingerprint": {fp}})
	if rec.Code != 200 {
		t.Fatalf("approve = %d %s", rec.Code, rec.Body.String())
	}
	c := f.called(ops.OpWorkerApprove)
	if len(c) != 1 || c[0].Args["fingerprint"] != fp || c[0].Args["worker_id"] == "" {
		t.Fatalf("worker_approve calls = %+v", c)
	}
	if rec = onbPost(t, s, "/workers/"+ops.NewUUIDv7()+"/remove", url.Values{}); rec.Code != 200 {
		t.Fatalf("remove = %d %s", rec.Code, rec.Body.String())
	}
	if len(f.called(ops.OpWorkerRemove)) != 1 {
		t.Error("worker_remove was not called")
	}
}

func TestInvitationsPageShowsTheUpgradeNotice(t *testing.T) {
	list := ops.InvitationListResult{Invitations: []ops.InvitationInfo{{
		ID: "0190a1b2-c3d4-7e5f-8a6b-0000000000f1", Name: "old-key", Mode: ops.InvitationReusable, Kind: ops.KindPrivate,
		Status: ops.InvitationActive, Notice: "Limited by the upgrade to 0.6.0: this key had no expiry.",
	}, {
		ID: "0190a1b2-c3d4-7e5f-8a6b-0000000000f2", Name: "infrastructure key file", Mode: ops.InvitationReusable, Kind: ops.KindInfrastructure,
		Status: ops.InvitationActive, Source: "infra_file",
	}}}
	connect(t, newFakeOps().reply(ops.OpInvitationList, list))
	body := mainOf(t, onbGet(t, onbAdmin(t), pathInvitations).Body.String())
	mustHave(t, body, "data-invitation-notice", "Limited by the upgrade to 0.6.0", "old-key", "MINION_INFRA_INVITE_KEY_FILE", "starts as <strong>pending</strong>")
}

func TestInvitationCreateSendsMaxUsesOnlyForReusableKeys(t *testing.T) {
	f := newFakeOps().reply(ops.OpInvitationCreate, ops.InvitationCreateResult{Key: sampleKey, Mode: ops.InvitationReusable, Kind: ops.KindPrivate})
	connect(t, f)
	s := onbUser(t)
	onbPost(t, s, pathInvitations, url.Values{"mode": {"reusable"}, "ttl": {"1w"}, "max_uses": {"25"}})
	onbPost(t, s, pathInvitations, url.Values{"mode": {"one_time"}, "ttl": {"1h"}, "max_uses": {"25"}})
	c := f.called(ops.OpInvitationCreate)
	if len(c) != 2 || c[0].Args["max_uses"] != float64(25) {
		t.Fatalf("calls = %+v", c)
	}
	if _, ok := c[1].Args["max_uses"]; ok {
		t.Errorf("max_uses was sent for a one-time key: %+v", c[1].Args)
	}
	rec := onbPost(t, s, pathInvitations, url.Values{"mode": {"reusable"}, "ttl": {"1w"}, "max_uses": {"many"}})
	mustHave(t, rec.Body.String(), "whole number")
}
