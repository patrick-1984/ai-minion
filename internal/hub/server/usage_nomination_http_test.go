package server

import (
	"net/http"
	"strings"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/hub/api"
	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/ops"
)

// nominationScope is a key scope: the same actions on every resource, and
// whether it reaches all of the owner's resources (a full-scope key).
type nominationScope struct {
	actions []ops.Action
	all     bool
}

func (s nominationScope) Actions(authz.ResolvedResource) []ops.Action { return s.actions }
func (s nominationScope) AllResources() bool                          { return s.all }

func TestUsageNominationAuthority(t *testing.T) {
	d, keys := startUsageHTTPHub(t)
	bobID := d.user("bob")
	rootID := d.userID("root")
	root, _ := d.login("root")
	bob, _ := d.login("bob")
	call := func(h map[string]string, op string, args any, want int) string {
		t.Helper()
		code, body, _ := d.hc.do(http.MethodPost, ops.RESTPrefix+op, args, h)
		if code != want {
			t.Fatalf("%s returned %d, want %d: %s", op, code, want, body)
		}
		return body
	}
	save := func(h map[string]string, kind, label string) string {
		t.Helper()
		var res ops.UsageBucketSetResult
		body := call(h, ops.OpUsageBucketSet, ops.UsageBucketSetArgs{Action: "save", Kind: kind, Label: label}, http.StatusOK)
		if _, after, ok := strings.Cut(body, `"id":"`); ok {
			res.ID, _, _ = strings.Cut(after, `"`)
		}
		return res.ID
	}
	rb := save(root, "session", "Team")
	rp := save(root, "provider", "Provider")
	bb := save(bob, "session", "Bob")
	nominate := func(scope, kind, id string) ops.UsageNominationSetArgs {
		return ops.UsageNominationSetArgs{Action: "nominate", Scope: scope, Kind: kind, BucketID: id}
	}
	clear := func(scope, kind string) ops.UsageNominationSetArgs {
		return ops.UsageNominationSetArgs{Action: "clear", Scope: scope, Kind: kind}
	}

	// A user publishes to their own Workers; the domain is for administrators.
	call(bob, ops.OpUsageNominationSet, nominate("user", "session", bb), http.StatusOK)
	call(bob, ops.OpUsageNominationSet, nominate("domain", "session", bb), http.StatusForbidden)
	call(bob, ops.OpUsageNominationList, ops.UsageNominationListArgs{}, http.StatusForbidden)
	if body := call(bob, ops.OpUsageBucketList, ops.UsageBucketListArgs{}, http.StatusOK); !strings.Contains(body, `"nominated":["user"]`) {
		t.Fatalf("own flags: %s", body)
	}

	// Keys: one with only read and operate, one restricted to a Worker, one with manage.
	for token, scope := range map[string]nominationScope{
		"mn_nom_operate":    {actions: []ops.Action{ops.ActionRead, ops.ActionOperate}, all: true},
		"mn_nom_restricted": {actions: []ops.Action{ops.ActionRead, ops.ActionOperate, ops.ActionManage}, all: false},
		"mn_nom_manage":     {actions: []ops.Action{ops.ActionRead, ops.ActionOperate, ops.ActionManage}, all: true},
	} {
		keys.mu.Lock()
		keys.known[token] = api.KeyIdentity{KeyID: token, Principal: authz.NewAPIKeyPrincipal(rootID, scope, "")}
		keys.mu.Unlock()
	}
	call(asKey("mn_nom_operate"), ops.OpUsageNominationSet, nominate("user", "session", rb), http.StatusOK)
	call(asKey("mn_nom_operate"), ops.OpUsageNominationSet, nominate("domain", "session", rb), http.StatusForbidden)
	call(asKey("mn_nom_operate"), ops.OpUsageNominationSet, clear("domain", "session"), http.StatusForbidden)
	call(asKey("mn_nom_operate"), ops.OpUsageNominationList, ops.UsageNominationListArgs{}, http.StatusForbidden)
	call(asKey("mn_nom_restricted"), ops.OpUsageNominationSet, nominate("domain", "session", rb), http.StatusForbidden)
	call(asKey("mn_nom_restricted"), ops.OpUsageNominationSet, nominate("user", "session", rb), http.StatusForbidden)
	call(asKey("mn_nom_manage"), ops.OpUsageNominationSet, nominate("domain", "provider", rp), http.StatusOK)

	// The administrator's own session publishes and sees who did what.
	call(root, ops.OpUsageNominationSet, nominate("domain", "session", rb), http.StatusOK)
	body := call(root, ops.OpUsageNominationList, ops.UsageNominationListArgs{}, http.StatusOK)
	for _, want := range []string{`"publisher_name":"root"`, `"label":"Team"`, `"label":"Provider"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("slot summary lacks %s: %s", want, body)
		}
	}
	if strings.Contains(body, "Bob") {
		t.Fatalf("summary leaked a private bucket: %s", body)
	}

	// Editing a bucket that is published to everyone needs the same authority.
	call(asKey("mn_nom_operate"), ops.OpUsageBucketSet, ops.UsageBucketSetArgs{Action: "save", Kind: "session", BucketID: rb, Label: "Renamed"}, http.StatusForbidden)
	call(asKey("mn_nom_operate"), ops.OpUsageBucketSet, ops.UsageBucketSetArgs{Action: "delete", Kind: "session", BucketID: rb}, http.StatusForbidden)
	call(asKey("mn_nom_manage"), ops.OpUsageBucketSet, ops.UsageBucketSetArgs{Action: "save", Kind: "session", BucketID: rb, Label: "Renamed"}, http.StatusOK)

	// Act-as changes nothing about it: neither the target's slots nor the domain.
	call(root, ops.OpActAsStart, map[string]any{"request_id": ops.NewUUIDv7(), "user_id": bobID, "duration_minutes": 5}, http.StatusOK)
	call(root, ops.OpUsageNominationSet, nominate("user", "session", bb), http.StatusForbidden)
	call(root, ops.OpUsageNominationSet, nominate("domain", "session", rb), http.StatusForbidden)
	call(root, ops.OpUsageNominationSet, clear("domain", "session"), http.StatusForbidden)
	call(root, ops.OpUsageNominationList, ops.UsageNominationListArgs{}, http.StatusForbidden)
	call(root, ops.OpUsageBucketSet, ops.UsageBucketSetArgs{Action: "delete", Kind: "session", BucketID: rb}, http.StatusForbidden)
	call(root, ops.OpUsagePublicationSet, ops.UsagePublicationSetArgs{AllowRepublish: true}, http.StatusForbidden)

	// The source owner's permission is his own setting, default off.
	call(bob, ops.OpUsagePublicationSet, ops.UsagePublicationSetArgs{AllowRepublish: true}, http.StatusOK)
	if body := call(bob, ops.OpUsageBucketList, ops.UsageBucketListArgs{}, http.StatusOK); !strings.Contains(body, `"allow_republish":true`) {
		t.Fatalf("consent flag: %s", body)
	}
}
