package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/hub/api"
	"github.com/patrick-1984/ai-minion/internal/hub/auth/session"
	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/hub/web"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// actAsHub is a Hub with the auth, API, act-as and web modules, an admin
// ("root") and two plain users, each with one Worker.
type actAsHub struct {
	*apiHub
	rootW, aliceW, bobW string
	aliceID, bobID      string
}

func startActAsHub(t *testing.T) *actAsHub {
	t.Helper()
	t.Setenv("MINION_ADMIN_USERNAME", "root")
	t.Setenv("MINION_ADMIN_PASSWORD", "actas-integration-pw-1")
	ah := &apiHub{workers: fakeWorkers{}, keys: &fakeKeys{known: map[string]api.KeyIdentity{}}}

	cat := ops.NewRegistry()
	for _, n := range []string{ops.OpAuditList, ops.OpActAsStart, ops.OpActAsStop} {
		op, ok := ops.Lookup(n)
		if !ok {
			t.Fatalf("no op %s", n)
		}
		cat.Register(op)
	}
	// Two probe ops with the shape of share_get and share_set (A-8).
	probes := apiTestCatalog(t)
	for _, n := range []string{probeShareGet, probeShareSet} {
		op, ok := probes.Lookup(n)
		if !ok {
			t.Fatalf("no probe op %s", n)
		}
		cat.Register(op)
	}

	r := NewRegistry()
	for _, err := range []error{
		registerCore(r),
		registerStore(r),
		r.Register(settingsModuleName, settingsModuleOrder, initSettingsModule,
			DeclaresService[*SettingsService](), DeclaresService[config.PanelSource]()),
	} {
		if err != nil {
			t.Fatal(err)
		}
	}
	registerAuditModule(r)
	registerAuthz(r)
	registerAuth(r)
	registerAPI(r, cat)
	registerActAs(r)
	registerWeb(r, web.DefaultRegistry)
	err := r.Register("probe", 60, func(_ context.Context, env *Env) error {
		az, err := Require[*authz.Authorizer](env)
		if err != nil {
			return err
		}
		if err := az.SetWorkerLookup(ah.workers); err != nil {
			return err
		}
		if ah.authn, err = Require[*api.Authenticator](env); err != nil {
			return err
		}
		if err := ah.authn.SetKeyAuthenticator(ah.keys); err != nil {
			return err
		}
		if ah.st, err = Require[*store.Store](env); err != nil {
			return err
		}
		if ah.mgr, err = Require[*session.Manager](env); err != nil {
			return err
		}
		reg, err := Require[*api.Registry](env)
		if err != nil {
			return err
		}
		if err := api.HandleTyped(reg, probeShareGet, func(context.Context, *api.Call, probeShareArgs) (probeShareResult, error) {
			return probeShareResult{Mode: "select"}, nil
		}); err != nil {
			return err
		}
		return api.HandleTyped(reg, probeShareSet, func(context.Context, *api.Call, probeShareSetArgs) (ops.WriteResult, error) {
			return ops.WriteResult{Outcome: ops.OutcomeDelivered}, nil
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	ah.h = startHub(t, &Config{DataDir: t.TempDir()}, r)
	ah.hc = newHubClient(t, ah.h, "https")

	h := &actAsHub{apiHub: ah, rootW: store.NewID(), aliceW: store.NewID(), bobW: store.NewID()}
	h.aliceID, h.bobID = ah.user(t, "alice"), ah.user(t, "bob")
	root, err := ah.st.UserByIdentifier(context.Background(), "root")
	if err != nil {
		t.Fatal(err)
	}
	ah.workers[h.rootW], ah.workers[h.aliceW], ah.workers[h.bobW] = root.ID, h.aliceID, h.bobID
	return h
}

func (h *actAsHub) startBody(userID string, minutes int) map[string]any {
	return map[string]any{"request_id": ops.NewUUIDv7(), "user_id": userID, "duration_minutes": minutes}
}

func (h *actAsHub) page(t *testing.T, token string) string {
	t.Helper()
	code, body, _ := h.hc.do("GET", "/", nil, map[string]string{"Cookie": session.CookieName + "=" + token})
	if code != 200 {
		t.Fatalf("GET / = %d: %s", code, body)
	}
	return body
}

func (h *actAsHub) shareGet(t *testing.T, worker string, hdr map[string]string) apiResp {
	t.Helper()
	return h.call(t, "POST", "/api/v1/ops/"+probeShareGet, map[string]any{"worker_id": worker}, hdr)
}

// auditEvents returns the audit events of one action, newest first.
func (h *actAsHub) auditEvents(t *testing.T, action string, hdr map[string]string) []ops.AuditEvent {
	t.Helper()
	r := h.call(t, "POST", "/api/v1/ops/audit_list", map[string]any{"action": action}, hdr)
	if r.code != 200 {
		t.Fatalf("audit_list = %d %s", r.code, r.body)
	}
	var res ops.AuditListResult
	if err := json.Unmarshal([]byte(r.body), &res); err != nil {
		t.Fatal(err)
	}
	return res.Events
}

func TestActAsBrowserSessionFlow(t *testing.T) {
	h := startActAsHub(t)
	rootTok, rootCSRF := h.login(t, "root")
	root := asSession(rootTok, rootCSRF)
	rootUser, _ := h.st.UserByIdentifier(context.Background(), "root")

	// Before: the admin sees every Worker, and the page has no banner.
	for _, w := range []string{h.rootW, h.aliceW, h.bobW} {
		if r := h.shareGet(t, w, root); r.code != 200 {
			t.Fatalf("admin on worker %s = %d %s", w, r.code, r.body)
		}
	}
	if body := h.page(t, rootTok); strings.Contains(body, "Acting as") {
		t.Fatal("the page shows an act-as banner before act-as started")
	}

	// Start: 30 minutes as alice.
	if r := h.call(t, "POST", "/api/v1/ops/act_as_start", h.startBody(h.aliceID, 30), root); r.code != 200 {
		t.Fatalf("act_as_start = %d %s", r.code, r.body)
	}

	// Subsequent requests of this browser session resolve in alice's space.
	if r := h.shareGet(t, h.aliceW, root); r.code != 200 {
		t.Errorf("alice's Worker while acting as alice = %d %s", r.code, r.body)
	}
	for name, w := range map[string]string{"the admin's own Worker": h.rootW, "bob's Worker": h.bobW} {
		if r := h.shareGet(t, w, root); r.code != 404 || r.err.Code != proto.CodeNotFound {
			t.Errorf("%s while acting as alice = %d %s, want not_found", name, r.code, r.body)
		}
	}
	// The admin's own permissions do not apply: alice cannot read the audit log.
	if r := h.call(t, "POST", "/api/v1/ops/audit_list", map[string]any{}, root); r.code != 403 {
		t.Errorf("audit_list while acting as alice = %d %s, want 403", r.code, r.body)
	}
	// Nor can act-as be nested: starting needs manage, which alice lacks (and
	// she holds nothing on bob's account, so it is not found).
	if r := h.call(t, "POST", "/api/v1/ops/act_as_start", h.startBody(h.bobID, 5), root); r.code != 404 {
		t.Errorf("nested act_as_start = %d %s, want 404", r.code, r.body)
	}

	// The banner is on every page and names both users and the time left.
	body := h.page(t, rootTok)
	for _, want := range []string{
		"Acting as alice (admin root) - 30 min left",
		`<form data-json-action="/api/v1/ops/act_as_stop"><button type="submit" class="btn-secondary">Exit</button></form>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q:\n%s", want, body)
		}
	}

	// A write while acting is audited with actor and acting_as.
	if r := h.call(t, "POST", "/api/v1/ops/"+probeShareSet, map[string]any{
		"request_id": ops.NewUUIDv7(), "worker_id": h.aliceW, "mode": "all"}, root); r.code != 200 {
		t.Fatalf("write as alice = %d %s", r.code, r.body)
	}

	// Exit with the exact request the banner button sends: "{}".
	if r := h.call(t, "POST", "/api/v1/ops/act_as_stop", map[string]any{}, root); r.code != 200 {
		t.Fatalf("act_as_stop = %d %s", r.code, r.body)
	}
	if body := h.page(t, rootTok); strings.Contains(body, "Acting as") {
		t.Error("the banner survived the exit")
	}
	if r := h.shareGet(t, h.rootW, root); r.code != 200 {
		t.Errorf("the admin's own Worker after the exit = %d %s", r.code, r.body)
	}

	// The audit trail: every action while acting names actor and acting_as.
	writes := h.auditEvents(t, probeShareSet, root)
	if len(writes) != 1 || writes[0].Actor != rootUser.ID || writes[0].ActingAs != h.aliceID || writes[0].Outcome != "success" {
		t.Errorf("write audit = %+v, want actor root and acting_as alice", writes)
	}
	stops := h.auditEvents(t, "act_as_stop", root)
	if len(stops) != 1 || stops[0].Actor != rootUser.ID || stops[0].ActingAs != h.aliceID || stops[0].Target != h.aliceID {
		t.Errorf("stop audit = %+v", stops)
	}
	starts := h.auditEvents(t, "act_as_start", root)
	var startOK, startDenied bool
	for _, e := range starts {
		if e.Actor != rootUser.ID {
			t.Errorf("start audit with another actor: %+v", e)
		}
		switch {
		case e.Outcome == "success" && e.ActingAs == "" && e.Target == h.aliceID:
			startOK = true
		case e.Outcome == "denied" && e.ActingAs == h.aliceID:
			startDenied = true
		}
	}
	if !startOK || !startDenied {
		t.Errorf("start audit = %+v, want a success on alice and a denied nested attempt made as alice", starts)
	}
}

func TestActAsEndsAtExpiryAndIsAudited(t *testing.T) {
	h := startActAsHub(t)
	rootTok, rootCSRF := h.login(t, "root")
	root := asSession(rootTok, rootCSRF)
	rootUser, _ := h.st.UserByIdentifier(context.Background(), "root")

	if r := h.call(t, "POST", "/api/v1/ops/act_as_start", h.startBody(h.bobID, 0), root); r.code != 200 {
		t.Fatalf("act_as_start = %d %s", r.code, r.body)
	}
	// The default is one hour.
	if body := h.page(t, rootTok); !strings.Contains(body, "Acting as bob (admin root) - 60 min left") {
		t.Fatalf("default duration: banner missing or wrong:\n%s", body)
	}
	// Move the period into the past, as the passing of time would.
	past := store.NowMS() - 1000
	if _, err := h.st.DB().Exec(`UPDATE act_as_sessions SET started_at = ?, expires_at = ?`, past-3600_000, past); err != nil {
		t.Fatal(err)
	}
	if body := h.page(t, rootTok); strings.Contains(body, "Acting as") {
		t.Error("the banner outlived the expiry")
	}
	if r := h.shareGet(t, h.rootW, root); r.code != 200 {
		t.Errorf("after the expiry the session is the admin's again: %d %s", r.code, r.body)
	}
	if r := h.shareGet(t, h.bobW, root); r.code != 200 { // admin sees all again
		t.Errorf("admin on bob's Worker after the expiry = %d %s", r.code, r.body)
	}
	exp := h.auditEvents(t, session.AuditActionActAsExpire, root)
	if len(exp) != 1 || exp[0].Actor != rootUser.ID || exp[0].ActingAs != h.bobID || exp[0].Target != h.bobID {
		t.Errorf("expire audit = %+v, want one event: actor root, acting_as bob", exp)
	}
	// The Exit button after the expiry is harmless and reports nothing to stop.
	r := h.call(t, "POST", "/api/v1/ops/act_as_stop", map[string]any{}, root)
	if r.code != 200 || !strings.Contains(r.body, ops.WarnNotActing) {
		t.Errorf("stop after expiry = %d %s", r.code, r.body)
	}
}

// Deactivating the target while the admin acts as that user must not wedge
// the browser session: the next request ends the act-as, audited once, and the
// Exit button and the other operations work as the admin's own.
func TestActAsEndsWhenTheTargetIsDeactivatedWhileActing(t *testing.T) {
	h := startActAsHub(t)
	rootTok, rootCSRF := h.login(t, "root")
	root := asSession(rootTok, rootCSRF)
	rootUser, _ := h.st.UserByIdentifier(context.Background(), "root")

	if r := h.call(t, "POST", "/api/v1/ops/act_as_start", h.startBody(h.bobID, 30), root); r.code != 200 {
		t.Fatalf("act_as_start = %d %s", r.code, r.body)
	}
	if body := h.page(t, rootTok); !strings.Contains(body, "Acting as bob (admin root)") {
		t.Fatalf("no banner while acting:\n%s", body)
	}
	if _, err := h.st.DB().Exec(`UPDATE users SET active = 0 WHERE id = ?`, h.bobID); err != nil {
		t.Fatal(err)
	}

	// The next page has no banner.
	if body := h.page(t, rootTok); strings.Contains(body, "Acting as") {
		t.Errorf("the banner survived the deactivation of the target:\n%s", body)
	}
	// The admin's own space is back, and nothing answers 403 any more.
	if r := h.shareGet(t, h.rootW, root); r.code != 200 {
		t.Errorf("the admin's own Worker after the deactivation = %d %s", r.code, r.body)
	}
	if r := h.call(t, "POST", "/api/v1/ops/audit_list", map[string]any{}, root); r.code != 200 {
		t.Errorf("audit_list after the deactivation = %d %s, want 200", r.code, r.body)
	}
	r := h.call(t, "POST", "/api/v1/ops/act_as_stop", map[string]any{}, root)
	if r.code != 200 || !strings.Contains(r.body, ops.WarnNotActing) {
		t.Errorf("Exit after the deactivation = %d %s, want 200 with a not-acting warning", r.code, r.body)
	}
	// The end is audited once, with both identities.
	end := h.auditEvents(t, session.AuditActionActAsEnd, root)
	if len(end) != 1 || end[0].Actor != rootUser.ID || end[0].ActingAs != h.bobID || end[0].Target != h.bobID || end[0].Outcome != "success" {
		t.Errorf("act_as.end audit = %+v, want one event: actor root, acting_as bob", end)
	}
	var rows int
	if err := h.st.DB().QueryRow(`SELECT count(*) FROM act_as_sessions`).Scan(&rows); err != nil || rows != 0 {
		t.Errorf("act-as rows = %d, %v; want 0", rows, err)
	}
}

func TestActAsStartRefusals(t *testing.T) {
	h := startActAsHub(t)
	rootTok, rootCSRF := h.login(t, "root")
	root := asSession(rootTok, rootCSRF)
	rootUser, _ := h.st.UserByIdentifier(context.Background(), "root")
	aliceTok, aliceCSRF := h.login(t, "alice")

	// A plain user cannot act as anyone.
	for _, target := range []string{h.bobID, h.aliceID} { // another user, and oneself
		if r := h.call(t, "POST", "/api/v1/ops/act_as_start", h.startBody(target, 5), asSession(aliceTok, aliceCSRF)); r.code != 403 && r.code != 404 {
			t.Errorf("a plain user starting act-as = %d %s, want 403 or 404", r.code, r.body)
		}
	}
	// Unknown user, oneself, a deactivated user, and a duration over 8 hours.
	if r := h.call(t, "POST", "/api/v1/ops/act_as_start", h.startBody(store.NewID(), 5), root); r.code != 404 {
		t.Errorf("unknown user = %d %s, want 404", r.code, r.body)
	}
	if r := h.call(t, "POST", "/api/v1/ops/act_as_start", h.startBody(rootUser.ID, 5), root); r.code != 400 {
		t.Errorf("oneself = %d %s, want 400", r.code, r.body)
	}
	if r := h.call(t, "POST", "/api/v1/ops/act_as_start", h.startBody(h.bobID, 481), root); r.code != 400 {
		t.Errorf("481 minutes = %d %s, want 400", r.code, r.body)
	}
	if _, err := h.st.DB().Exec(`UPDATE users SET active = 0 WHERE id = ?`, h.bobID); err != nil {
		t.Fatal(err)
	}
	if r := h.call(t, "POST", "/api/v1/ops/act_as_start", h.startBody(h.bobID, 5), root); r.code != 404 && r.code != 409 {
		t.Errorf("deactivated user = %d %s, want 404 or 409", r.code, r.body)
	}
	// An API key cannot start act-as, even for an admin.
	h.keys.mu.Lock()
	h.keys.known["mn_actas_probe_key"] = api.KeyIdentity{KeyID: "k1", Principal: authz.NewAPIKeyPrincipal(rootUser.ID, everythingScope{}, "")}
	h.keys.mu.Unlock()
	if r := h.call(t, "POST", "/api/v1/ops/act_as_start", h.startBody(h.aliceID, 5), asKey("mn_actas_probe_key")); r.code != 403 {
		t.Errorf("act_as_start with an API key = %d %s, want 403", r.code, r.body)
	}
	var rows int
	if err := h.st.DB().QueryRow(`SELECT count(*) FROM act_as_sessions`).Scan(&rows); err != nil || rows != 0 {
		t.Errorf("act-as rows after refused starts = %d, %v; want 0", rows, err)
	}
}

func TestActAsDoesNotSurviveLogoutOrAnotherBrowser(t *testing.T) {
	h := startActAsHub(t)
	rootTok, rootCSRF := h.login(t, "root")
	root := asSession(rootTok, rootCSRF)
	otherTok, otherCSRF := h.login(t, "root") // a second browser of the same admin
	other := asSession(otherTok, otherCSRF)

	if r := h.call(t, "POST", "/api/v1/ops/act_as_start", h.startBody(h.aliceID, 10), root); r.code != 200 {
		t.Fatalf("act_as_start = %d %s", r.code, r.body)
	}
	// Act-as belongs to one browser session: the other one still is the admin.
	if r := h.shareGet(t, h.bobW, other); r.code != 200 {
		t.Errorf("the admin's other browser lost its own space: %d %s", r.code, r.body)
	}
	if body := h.page(t, otherTok); strings.Contains(body, "Acting as") {
		t.Error("the banner shows in a browser that is not acting")
	}
	// Logging out removes the act-as state with the session.
	if code, body, _ := h.hc.do("POST", "/auth/logout", map[string]any{}, root); code != 200 {
		t.Fatalf("logout = %d %s", code, body)
	}
	var rows int
	if err := h.st.DB().QueryRow(`SELECT count(*) FROM act_as_sessions`).Scan(&rows); err != nil || rows != 0 {
		t.Errorf("act-as rows after logout = %d, %v; want 0", rows, err)
	}
	if r := h.shareGet(t, h.bobW, root); r.code != 401 {
		t.Errorf("a logged-out cookie = %d %s, want 401", r.code, r.body)
	}
}
