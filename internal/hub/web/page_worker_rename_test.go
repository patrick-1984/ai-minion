package web

// Renaming a Worker from its page (worker_rename) and the Worker name of an
// invitation key (invitation_create worker_name).

import (
	"net/url"
	"strings"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/ops"
)

// The Rename form is offered to whoever may operate the Worker: its owner and
// an administrator (any Worker), never to someone who only sees it.
func TestWorkerPageOffersRenameToTheOwnerAndAdmins(t *testing.T) {
	bobOnline := wInfra
	bobOnline.Online = true
	shared := Dashboard{Workers: []WorkerRow{wPrivate, bobOnline}}
	s, _ := workerPageServer(t, shared, shared, wAliceInfo, wBobInfo)
	_, own := page(t, s, "/workers/w1", "alice:user")
	mustHave(t, own, `id="rename-section"`, `hx-post="/workers/w1/rename"`, `name="name" value="laptop"`, `data-control="rename"`)
	_, other := page(t, s, "/workers/w2", "alice:user")
	mustNotHave(t, other, `id="rename-section"`, `/workers/w2/rename`)
	_, adm := page(t, s, "/workers/w2?scope=all", "root:admin")
	mustHave(t, adm, `id="rename-section"`, `hx-post="/workers/w2/rename"`)
}

func TestRenamePostsWorkerRename(t *testing.T) {
	f := newFakeOps().reply(ops.OpWorkerRename, ops.WriteResult{Outcome: ops.OutcomeDelivered})
	connect(t, f)
	s := onbUser(t)
	id := ops.NewUUIDv7()
	rec := onbPost(t, s, "/workers/"+id+"/rename", url.Values{"name": {" phone "}})
	if rec.Code != 200 || rec.Header().Get("HX-Refresh") != "true" {
		t.Fatalf("rename = %d %v %s", rec.Code, rec.Header(), rec.Body.String())
	}
	c := f.called(ops.OpWorkerRename)
	if len(c) != 1 || c[0].Args["worker_id"] != id || c[0].Args["name"] != "phone" || c[0].Args["request_id"] == "" {
		t.Fatalf("worker_rename calls = %+v", c)
	}
	// An invalid name never reaches the Hub.
	for _, bad := range []string{"", "my phone", "a:b", strings.Repeat("x", ops.MaxWorkerNameLen+1)} {
		rec = onbPost(t, s, "/workers/"+id+"/rename", url.Values{"name": {bad}})
		mustHave(t, rec.Body.String(), "without spaces or colons")
	}
	if len(f.called(ops.OpWorkerRename)) != 1 {
		t.Errorf("an invalid name was posted")
	}
}

func TestRenameShowsTheRefusalOfTheHub(t *testing.T) {
	f := newFakeOps().on(ops.OpWorkerRename, func(map[string]any) (any, error) {
		return nil, &OpError{Status: 409, Code: "conflict", Message: "you already have a Worker with this name"}
	})
	connect(t, f)
	s := onbUser(t)
	rec := onbPost(t, s, "/workers/"+ops.NewUUIDv7()+"/rename", url.Values{"name": {"taken"}})
	if rec.Header().Get("HX-Refresh") != "" {
		t.Errorf("a refused rename reloads the page")
	}
	mustHave(t, rec.Body.String(), `role="alert"`, "already have a Worker with this name")
}

// The one name of "Add a Worker in one step" is the Worker's name too: the
// Hub stores it whatever name the Worker announces.
func TestAddWorkerJoinNamesTheWorkerOnTheHub(t *testing.T) {
	f := newJoinFixture(t)
	s := onbUser(t)
	if rec := onbPost(t, s, pathAddWorkerJoin, url.Values{"name": {"phone"}}); rec.Code != 200 {
		t.Fatalf("POST = %d %s", rec.Code, rec.Body)
	}
	c := f.ops.called(ops.OpInvitationCreate)
	if len(c) != 1 || c[0].Args["name"] != "phone" || c[0].Args["worker_name"] != "phone" {
		t.Fatalf("create calls = %+v", c)
	}
	// Without a name the Worker keeps its own: no worker_name is sent.
	if rec := onbPost(t, s, pathAddWorkerJoin, url.Values{}); rec.Code != 200 {
		t.Fatalf("POST = %d %s", rec.Code, rec.Body)
	}
	if c = f.ops.called(ops.OpInvitationCreate); len(c) != 2 || c[1].Args["worker_name"] != nil {
		t.Fatalf("create calls = %+v", c)
	}
}

func TestInvitationCreateSendsTheWorkerName(t *testing.T) {
	f := newFakeOps().on(ops.OpInvitationCreate, func(a map[string]any) (any, error) {
		wn, _ := a["worker_name"].(string)
		return ops.InvitationCreateResult{ID: ops.NewUUIDv7(), Key: "mnk_x.y", Name: "label", WorkerName: wn, Mode: ops.InvitationOneTime, Kind: ops.KindPrivate, TTL: ops.TTL1h}, nil
	})
	connect(t, f)
	s := onbUser(t)
	rec := onbPost(t, s, pathInvitations, url.Values{"mode": {ops.InvitationOneTime}, "ttl": {ops.TTL1h}, "name": {"label"}, "worker_name": {"tablet"}})
	if rec.Code != 200 {
		t.Fatalf("POST = %d %s", rec.Code, rec.Body)
	}
	c := f.called(ops.OpInvitationCreate)
	if len(c) != 1 || c[0].Args["worker_name"] != "tablet" || c[0].Args["name"] != "label" {
		t.Fatalf("create calls = %+v", c)
	}
	mustHave(t, rec.Body.String(), "data-invitation-worker-name", "<strong>tablet</strong>")
	rec = onbPost(t, s, pathInvitations, url.Values{"mode": {ops.InvitationOneTime}, "ttl": {ops.TTL1h}, "worker_name": {"has space"}})
	mustHave(t, rec.Body.String(), "without spaces or colons")
	if len(f.called(ops.OpInvitationCreate)) != 1 {
		t.Error("an invalid worker name was posted")
	}
}

func TestInvitationListShowsTheWorkerName(t *testing.T) {
	f := newFakeOps().reply(ops.OpInvitationList, ops.InvitationListResult{Invitations: []ops.InvitationInfo{
		{ID: ops.NewUUIDv7(), Name: "for the phone", WorkerName: "phone", Mode: ops.InvitationOneTime, Kind: ops.KindPrivate, TTL: ops.TTL1h, Status: ops.InvitationActive},
	}})
	connect(t, f)
	s := onbUser(t)
	rec := onbGet(t, s, pathInvitations)
	mustHave(t, rec.Body.String(), "data-invitation-worker-name", "(Worker: phone)", `name="worker_name"`)
}
