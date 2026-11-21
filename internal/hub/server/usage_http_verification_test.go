package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/hub/api"
	"github.com/patrick-1984/ai-minion/internal/hub/auth/session"
	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/hub/web"
	"github.com/patrick-1984/ai-minion/internal/ops"
)

func startUsageHTTPHub(t *testing.T) (*dashHub, *fakeKeys) {
	t.Helper()
	t.Setenv("MINION_ADMIN_USERNAME", "root")
	t.Setenv("MINION_ADMIN_PASSWORD", "usage-integration-pw-1")
	r := NewRegistry()
	for _, err := range []error{
		registerCore(r),
		registerStore(r),
		r.Register(settingsModuleName, settingsModuleOrder, initSettingsModule,
			DeclaresService[*SettingsService](), DeclaresService[config.PanelSource]()),
		r.Register("usage", 59, initUsage),
	} {
		if err != nil {
			t.Fatal(err)
		}
	}
	registerAuditModule(r)
	registerAuthz(r)
	registerAuth(r)
	registerHubIDModule(r)
	registerWorkersModule(r)
	registerAPI(r, ops.Default)
	registerActAs(r)
	registerRouteModule(r)
	registerEventsModule(r)
	registerWeb(r, web.DefaultRegistry)
	keys := &fakeKeys{known: map[string]api.KeyIdentity{}}
	d := &dashHub{t: t}
	if err := r.Register("usage-fixtures", 90, func(_ context.Context, env *Env) error {
		var err error
		if d.st, err = Require[*store.Store](env); err != nil {
			return err
		}
		if d.mgr, err = Require[*session.Manager](env); err != nil {
			return err
		}
		n, err := Require[*api.Authenticator](env)
		if err != nil {
			return err
		}
		return n.SetKeyAuthenticator(keys)
	}); err != nil {
		t.Fatal(err)
	}
	d.h = startHub(t, &Config{DataDir: t.TempDir()}, r)
	d.hc = newHubClient(t, d.h, "https")
	return d, keys
}

func TestUsageHTTPPrivateBucketsAndSessionMetadata(t *testing.T) {
	d, keys := startUsageHTTPHub(t)
	bobID := d.user("bob")
	rootID := d.userID("root")
	root, _ := d.login("root")
	bob, _ := d.login("bob")
	rootWorker := d.enrol("root-workstation", rootID, store.WorkerKindPrivate)
	bobWorker := d.enrol("bob-laptop", bobID, store.WorkerKindPrivate)
	now := time.Now()
	sample := now.UnixMilli()
	sharedAccount := ops.UsageID("account", "shared subscription")
	for _, fixture := range []struct{ owner, worker, session string }{
		{rootID, rootWorker, "root-private-session"},
		{bobID, bobWorker, "bob-private-session"},
	} {
		report := ops.UsageReport{Version: ops.UsageVersion, ObservedAt: sample, Complete: true}
		report.Sessions = []ops.UsageSession{{
			ID: ops.UsageID("session", fixture.worker, fixture.session), Name: fixture.session, AuthMode: "subscription", AccountID: sharedAccount,
			StartedAt: &sample, ObservedAt: sample, Source: "test-usage",
		}}
		report.Accounts = []ops.UsageAccount{{
			ID: sharedAccount, Provider: "OpenAI", Label: "Test subscription", AuthMode: "subscription", ObservedAt: sample, Source: "test-usage",
		}}
		if err := d.st.SaveUsage(context.Background(), fixture.worker, fixture.owner, report, now); err != nil {
			t.Fatal(err)
		}
	}

	call := func(headers map[string]string, op string, args any, want int, result any) string {
		t.Helper()
		code, body, _ := d.hc.do(http.MethodPost, ops.RESTPrefix+op, args, headers)
		if code != want {
			t.Fatalf("%s returned %d, want %d: %s", op, code, want, body)
		}
		if result != nil {
			if err := json.Unmarshal([]byte(body), result); err != nil {
				t.Fatalf("decode %s: %v", op, err)
			}
		}
		return body
	}
	var result ops.UsageListResult
	call(root, ops.OpUsageList, ops.UsageListArgs{Section: "sessions"}, http.StatusOK, &result)
	if len(result.Sessions) != 2 {
		t.Fatalf("domain metadata: %+v", result.Sessions)
	}
	result = ops.UsageListResult{}
	call(bob, ops.OpUsageList, ops.UsageListArgs{Section: "sessions"}, http.StatusOK, &result)
	if len(result.Sessions) != 1 || result.Sessions[0].Name != "bob-private-session" {
		t.Fatalf("Worker-owner metadata: %+v", result.Sessions)
	}
	bobSession := result.Sessions[0].ID
	result = ops.UsageListResult{}
	call(root, ops.OpUsageList, ops.UsageListArgs{Section: "sessions", WorkerID: bobWorker}, http.StatusOK, &result)
	if len(result.Sessions) != 1 || result.Sessions[0].ID != bobSession {
		t.Fatalf("Worker filter: %+v", result.Sessions)
	}
	if page := d.page("/usage", root); !strings.Contains(page, "Test subscription") {
		t.Fatal("the real Hub Usage page did not render the subscription")
	}

	var private ops.UsageBucketSetResult
	call(bob, ops.OpUsageBucketSet, ops.UsageBucketSetArgs{Action: "save", Kind: "session", Label: "Bob private bucket"}, http.StatusOK, &private)
	call(bob, ops.OpUsageBucketSet, ops.UsageBucketSetArgs{Action: "assign", Kind: "session", BucketID: private.ID, Members: []string{bobSession}}, http.StatusOK, nil)
	if body := call(root, ops.OpUsageBucketList, ops.UsageBucketListArgs{}, http.StatusOK, nil); strings.Contains(body, private.ID) {
		t.Fatal("an administrator saw Bob's private buckets")
	}
	result = ops.UsageListResult{}
	call(root, ops.OpUsageList, ops.UsageListArgs{Section: "sessions", BucketID: private.ID}, http.StatusOK, &result)
	if len(result.Sessions) != 0 {
		t.Fatal("an administrator used a private bucket ID as a membership oracle")
	}
	call(root, ops.OpUsageBucketSet, ops.UsageBucketSetArgs{Action: "delete", Kind: "session", BucketID: private.ID}, http.StatusNotFound, nil)

	// One account can be observed on several Workers. The administrator may
	// clear a preferred source, while Bob cannot change a poll shared with
	// a Worker outside his authorized set.
	call(root, ops.OpUsageCollectorSet, ops.UsageCollectorSetArgs{AccountID: sharedAccount}, http.StatusOK, nil)
	call(root, ops.OpUsageCollectorSet, ops.UsageCollectorSetArgs{AccountID: sharedAccount, WorkerID: rootWorker}, http.StatusOK, nil)
	call(bob, ops.OpUsageCollectorSet, ops.UsageCollectorSetArgs{AccountID: sharedAccount}, http.StatusNotFound, nil)

	privateAccount := ops.UsageAccountSetArgs{AccountID: sharedAccount, Label: "Bob private label", Position: 7}
	call(bob, ops.OpUsageAccountSet, privateAccount, http.StatusOK, nil)
	result = ops.UsageListResult{}
	call(bob, ops.OpUsageList, ops.UsageListArgs{Section: "accounts"}, http.StatusOK, &result)
	if len(result.Accounts) != 1 || result.Accounts[0].Alias != privateAccount.Label || result.Accounts[0].Position != 7 {
		t.Fatalf("private account presentation: %+v", result.Accounts)
	}
	if c := result.Accounts[0].Collector; c != nil && (c.LastSuccess != nil || c.NextDue != nil || c.PreferredWorker != "" || c.ActiveWorker != "") {
		t.Fatal("account viewer saw scheduler details from outside the viewer's scope")
	}
	if body := call(root, ops.OpUsageList, ops.UsageListArgs{Section: "accounts"}, http.StatusOK, nil); strings.Contains(body, privateAccount.Label) {
		t.Fatal("an administrator saw another user's private account label")
	}

	keys.mu.Lock()
	keys.known["mn_usage_scoped_fixture"] = api.KeyIdentity{
		KeyID: "usage-scoped", Principal: authz.NewAPIKeyPrincipal(rootID,
			keyScope{"": {ops.ActionRead}, bobWorker: {ops.ActionRead}}, ""),
	}
	keys.mu.Unlock()
	key := asKey("mn_usage_scoped_fixture")
	result = ops.UsageListResult{}
	call(key, ops.OpUsageList, ops.UsageListArgs{Section: "sessions"}, http.StatusOK, &result)
	if len(result.Sessions) != 1 || result.Sessions[0].ID != bobSession || result.Sessions[0].BucketID != "" {
		t.Fatalf("restricted key metadata scope: %+v", result.Sessions)
	}
	call(key, ops.OpUsageBucketList, ops.UsageBucketListArgs{}, http.StatusForbidden, nil)

	// Act-as changes the metadata audience but does not open the target's
	// organization, even though the same browser holds the admin cookie.
	call(root, ops.OpActAsStart, map[string]any{"request_id": ops.NewUUIDv7(), "user_id": bobID, "duration_minutes": 5}, http.StatusOK, nil)
	result = ops.UsageListResult{}
	call(root, ops.OpUsageList, ops.UsageListArgs{Section: "sessions"}, http.StatusOK, &result)
	if len(result.Sessions) != 1 || result.Sessions[0].ID != bobSession || result.Sessions[0].BucketID != "" {
		t.Fatalf("act-as metadata and membership: %+v", result.Sessions)
	}
	call(root, ops.OpUsageBucketList, ops.UsageBucketListArgs{}, http.StatusForbidden, nil)
	call(root, ops.OpUsageList, ops.UsageListArgs{Section: "sessions", BucketID: private.ID}, http.StatusForbidden, nil)
	call(root, ops.OpUsageAccountSet, privateAccount, http.StatusForbidden, nil)
	if body := call(root, ops.OpUsageList, ops.UsageListArgs{Section: "accounts"}, http.StatusOK, nil); strings.Contains(body, privateAccount.Label) {
		t.Fatal("act-as exposed another user's private account label")
	}
}

func TestUsageHTTPMetadataDoesNotRouteOfflineConsole(t *testing.T) {
	d, _ := startUsageHTTPHub(t)
	bobID := d.user("bob")
	root, _ := d.login("root")
	bob, _ := d.login("bob")
	worker := d.enrol("bob-laptop", bobID, store.WorkerKindPrivate)
	ctx := context.Background()
	now := time.Now()
	report := ops.UsageReport{Version: ops.UsageVersion, Complete: true, ObservedAt: now.UnixMilli(),
		Sessions: []ops.UsageSession{{ID: ops.UsageID("session", "offline"), Name: "offline-session", AuthMode: "unknown", Source: "fixture", ObservedAt: now.UnixMilli()}},
	}
	if err := d.st.SaveUsage(ctx, worker, bobID, report, now); err != nil {
		t.Fatal(err)
	}
	code, body, _ := d.hc.do(http.MethodGet, ops.RESTPrefix+ops.OpUsageList+"?section=sessions", nil, root)
	if code != http.StatusOK || !strings.Contains(body, "offline-session") {
		t.Fatalf("offline usage: %d %s", code, body)
	}
	for _, headers := range []map[string]string{root, bob} {
		code, body, _ := d.hc.do(http.MethodPost, ops.RESTPrefix+ops.OpSessionSnapshot,
			map[string]any{"target": map[string]any{"worker_id": worker, "session_name": "offline-session"}}, headers)
		if code == http.StatusOK || !strings.Contains(body, "worker_offline") {
			t.Fatalf("console snapshot was not refused for an offline Worker: %d %s", code, body)
		}
	}
}
