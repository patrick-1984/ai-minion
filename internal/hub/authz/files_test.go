package authz_test

import (
	"context"
	"fmt"
	"slices"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/hub/auth/apikey"
	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

var (
	fileRead  = set(ops.ActionFileRead)
	fileWrite = set(ops.ActionFileWrite)
)

// fileHeld returns whether p may pull from and push into res, by Decide.
func fileHeld(t *testing.T, a *authz.Authorizer, p authz.Principal, res authz.Resource) (pull, push bool) {
	t.Helper()
	ctx := context.Background()
	return a.Decide(ctx, p, fileRead, res) == nil, a.Decide(ctx, p, fileWrite, res) == nil
}

// The authz table of WP6: principal kind x file flag x level x source proof.
func TestFileActionsTable(t *testing.T) {
	a := dgAuthz(grantStore())
	flags := func(g authz.DirectGrant, pull, push bool) authz.DirectGrant {
		g.FilesPull, g.FilesPush = pull, push
		return g
	}
	verified := func(grants ...authz.DirectGrant) authz.Principal {
		return authz.NewWorkerSessionPrincipal("A1", "main", nil, grants).WithVerifiedFiles("main", nil)
	}
	claimed := func(grants ...authz.DirectGrant) authz.Principal {
		return authz.NewWorkerSessionPrincipal("A1", "main", nil, grants)
	}
	x := sess("W2", "x")
	type tc struct {
		name       string
		p          authz.Principal
		res        authz.Resource
		pull, push bool
	}
	var cases []tc
	for level := authz.LevelView; level <= authz.LevelControl; level++ {
		for _, fl := range []struct{ pull, push bool }{{false, false}, {true, false}, {false, true}, {true, true}} {
			g := flags(ownerGrant("g", "W2", "x", level), fl.pull, fl.push)
			cases = append(cases,
				tc{fmt.Sprintf("verified level %d pull=%t push=%t", level, fl.pull, fl.push), verified(g), x,
					fl.pull, fl.push && level == authz.LevelControl},
				tc{fmt.Sprintf("claimed level %d pull=%t push=%t", level, fl.pull, fl.push), claimed(g), x, false, false})
		}
	}
	both := flags(ownerGrant("g", "W2", "x", authz.LevelControl), true, true)
	expired := both
	expired.ExpiresAt = authz.ExpiryAt(dgNow)
	suspended := both
	suspended.Suspended = true
	cases = append(cases,
		tc{"verified, other session of the target", verified(both), sess("W2", "y"), false, false},
		tc{"verified, Worker resource", verified(both), authz.WorkerResource{ID: "W2"}, false, false},
		tc{"verified, expired grant", verified(expired), x, false, false},
		tc{"verified, suspended grant", verified(suspended), x, false, false},
		tc{"verified, wildcard grant", verified(flags(ownerGrant("g", "W2", "", 3), true, true)), sess("W2", "later"), true, true},
		tc{"verified admin grant", verified(flags(adminGrant("g", "W3", "build", 3), true, false)), sess("W3", "build"), true, false},
		tc{"verified without grants: own Worker", verified(), sess("A1", "s"), false, false},
		tc{"Worker identity on its own Worker", authz.NewWorkerPrincipal("A1"), sess("A1", "s"), false, false},
		tc{"session-control grant", authz.NewWorkerGrantPrincipal("A1", grantScope(tEverything)), x, false, false},
		tc{"owner", authz.NewSessionPrincipal("alice"), x, true, true},
		tc{"owner, foreign session", authz.NewSessionPrincipal("alice"), sess("W3", "build"), false, false},
		tc{"admin", authz.NewSessionPrincipal("admin"), sess("W3", "build"), true, true},
		tc{"act-as the owner", authz.NewActAsPrincipal("admin", "alice", dgNow.Add(1e12)), x, true, true},
		tc{"API key, everything, all four actions", authz.NewAPIKeyPrincipal("alice", everything(allFour...), ""), x, false, false},
		tc{"API key, everything, file_read named", authz.NewAPIKeyPrincipal("alice", everything(ops.ActionFileRead), ""), x, true, false},
		tc{"API key, everything, both named", authz.NewAPIKeyPrincipal("alice", everything(ops.ActionFileRead, ops.ActionFileWrite), ""), x, true, true},
		tc{"API key, both named, foreign session", authz.NewAPIKeyPrincipal("alice", everything(ops.ActionFileRead, ops.ActionFileWrite), ""), sess("W3", "build"), false, false},
		tc{"admin key, everything, all four", authz.NewAPIKeyPrincipal("admin", everything(allFour...), ""), sess("W3", "build"), false, false},
	)
	for _, c := range cases {
		pull, push := fileHeld(t, a, c.p, c.res)
		if pull != c.pull || push != c.push {
			t.Errorf("%s: pull=%t push=%t, want pull=%t push=%t", c.name, pull, push, c.pull, c.push)
		}
	}
}

// The stored API key scope (apikey.Scope) of a key made before v0.24 names
// no file action, also for everything.
func TestFileActionsStoredKeyScope(t *testing.T) {
	a := dgAuthz(grantStore())
	old := apikey.NewScope([]apikey.Grant{{Kind: ops.KeyResourceEverything}},
		[]ops.Action{ops.ActionRead, ops.ActionOperate, ops.ActionMessage, ops.ActionManage})
	if pull, push := fileHeld(t, a, authz.NewAPIKeyPrincipal("admin", old, ""), sess("W2", "x")); pull || push {
		t.Errorf("an everything key from v0.23 holds a file action: pull=%t push=%t", pull, push)
	}
	named := apikey.NewScope([]apikey.Grant{{Kind: ops.KeyResourceWorker, WorkerID: "W2"}}, []ops.Action{ops.ActionFileRead})
	if pull, push := fileHeld(t, a, authz.NewAPIKeyPrincipal("alice", named, ""), sess("W2", "x")); !pull || push {
		t.Errorf("a key naming file_read: pull=%t push=%t", pull, push)
	}
}

// A user-managed switch that is off removes the file actions of people.
func TestFileActionsUserManaged(t *testing.T) {
	a := dgAuthz(grantStore())
	a.SetUserManagedLookup(func(context.Context, string) (bool, error) { return false, nil })
	if pull, push := fileHeld(t, a, authz.NewSessionPrincipal("alice"), sess("W2", "x")); pull || push {
		t.Errorf("owner on a Worker that is not user-managed: pull=%t push=%t", pull, push)
	}
}

// Master acceptances: the row that matched is named, the wildcard row too, a
// flag applies only through its own row, and the master scope bounds it.
func TestFileAcceptanceDecisions(t *testing.T) {
	a := dgAuthz(grantStore())
	ctx := context.Background()
	scope := grantScope(tW2, tW3Build)
	wild := authz.FileAcceptance{ID: "acc-w2", TargetWorkerID: "W2", Sessions: nil, FilesPull: true}
	limited := authz.FileAcceptance{ID: "acc-w3", TargetWorkerID: "W3", Sessions: []string{"build"}, FilesPull: true, FilesPush: true}
	p := authz.NewBoundMasterPrincipal("A1", scope).WithVerifiedFiles("main", []authz.FileAcceptance{wild, limited})

	owner, basis, err := a.DecideFile(ctx, p, ops.FileDirectionPull, authz.SessionResource{WorkerID: "W2", Name: "x"})
	if err != nil || owner != "alice" || basis.AcceptanceID != "acc-w2" || basis.Standing || len(basis.GrantIDs) != 0 {
		t.Fatalf("pull through the wildcard row: %q %+v %v", owner, basis, err)
	}
	if _, _, err := a.DecideFile(ctx, p, ops.FileDirectionPush, authz.SessionResource{WorkerID: "W2", Name: "x"}); code(t, err) != proto.CodeForbidden {
		t.Errorf("push through a pull-only row: %v", err)
	}
	// W3 is bob's: the master scope reaches it only for an admin owner, so
	// alice's master holds nothing there although the row has both flags.
	if pull, push := fileHeld(t, a, p, sess("W3", "build")); pull || push {
		t.Errorf("row beyond the owner's reach: pull=%t push=%t", pull, push)
	}
	// The same rows for a master of an admin's Worker (W1).
	pa := authz.NewBoundMasterPrincipal("W1", scope).WithVerifiedFiles("main", []authz.FileAcceptance{wild, limited})
	_, basis, err = a.DecideFile(ctx, pa, ops.FileDirectionPush, authz.SessionResource{WorkerID: "W3", Name: "build"})
	if err != nil || basis.AcceptanceID != "acc-w3" {
		t.Fatalf("push through the limited row: %+v %v", basis, err)
	}
	// A session the limited row does not list.
	if pull, push := fileHeld(t, a, pa, sess("W3", "other")); pull || push {
		t.Errorf("session outside the row: pull=%t push=%t", pull, push)
	}
	// The wildcard row's flag does not apply to W3 (another row) and W3's
	// row does not apply to W2.
	if pull, push := fileHeld(t, a, pa, sess("W2", "x")); !pull || push {
		t.Errorf("W2 through its own row only: pull=%t push=%t", pull, push)
	}
	// No acceptance flags: the master reaches the session but holds no file
	// action; and the same principal without verification holds none.
	if pull, push := fileHeld(t, a, authz.NewBoundMasterPrincipal("A1", scope), sess("W2", "x")); pull || push {
		t.Errorf("unverified master: pull=%t push=%t", pull, push)
	}
	if pull, push := fileHeld(t, a, authz.NewBoundMasterPrincipal("A1", scope).WithVerifiedFiles("main", nil), sess("W2", "x")); pull || push {
		t.Errorf("verified master without flags: pull=%t push=%t", pull, push)
	}
	// A row whose scope part is missing (accepted, but the master scope does
	// not reach the session) gives nothing.
	narrow := authz.NewBoundMasterPrincipal("A1", grantScope(tW3Build)).WithVerifiedFiles("main", []authz.FileAcceptance{wild})
	if pull, _ := fileHeld(t, a, narrow, sess("W2", "x")); pull {
		t.Error("a row outside the master scope authorized a pull")
	}
}

// Direct grants and acceptances together: the decision names the grants.
func TestDecideFileBasisGrants(t *testing.T) {
	a := dgAuthz(grantStore())
	ctx := context.Background()
	g1 := ownerGrant("g1", "W2", "", 1)
	g1.FilesPull = true
	g2 := ownerGrant("g2", "W2", "x", 3)
	g2.FilesPull, g2.FilesPush = true, true
	g3 := ownerGrant("g3", "W2", "x", 3) // no flags
	p := authz.NewWorkerSessionPrincipal("A1", "main", nil, []authz.DirectGrant{g1, g2, g3}).WithVerifiedFiles("main", nil)
	_, basis, err := a.DecideFile(ctx, p, ops.FileDirectionPull, authz.SessionResource{WorkerID: "W2", Name: "x"})
	if err != nil || !slices.Equal(basis.GrantIDs, []string{"g1", "g2"}) || basis.AcceptanceID != "" {
		t.Fatalf("pull basis = %+v, %v", basis, err)
	}
	_, basis, err = a.DecideFile(ctx, p, ops.FileDirectionPush, authz.SessionResource{WorkerID: "W2", Name: "x"})
	if err != nil || !slices.Equal(basis.GrantIDs, []string{"g2"}) {
		t.Fatalf("push basis = %+v, %v", basis, err)
	}
	if ids, err := a.GrantIDs(ctx, p, fileWrite, sess("W2", "x")); err != nil || !slices.Equal(ids, []string{"g2"}) {
		t.Errorf("GrantIDs(file_write) = %v, %v", ids, err)
	}
	if ok, err := a.GrantedByScope(ctx, p, fileRead, sess("W2", "x")); err != nil || !ok {
		t.Errorf("GrantedByScope(file_read) = %v, %v", ok, err)
	}
	// Standing principals.
	_, basis, err = a.DecideFile(ctx, authz.NewSessionPrincipal("alice"), ops.FileDirectionPush, authz.SessionResource{WorkerID: "W2", Name: "x"})
	if err != nil || !basis.Standing || len(basis.GrantIDs) != 0 {
		t.Errorf("owner basis = %+v, %v", basis, err)
	}
	// Refusals.
	if _, _, err := a.DecideFile(ctx, authz.NewSessionPrincipal("bob"), ops.FileDirectionPull, authz.SessionResource{WorkerID: "W2", Name: "x"}); code(t, err) != proto.CodeNotFound {
		t.Errorf("stranger: %v", err)
	}
	if _, _, err := a.DecideFile(ctx, authz.NewAPIKeyPrincipal("alice", everything(ops.ActionRead), ""), ops.FileDirectionPull,
		authz.SessionResource{WorkerID: "W2", Name: "x"}); code(t, err) != proto.CodeForbidden {
		t.Errorf("read-only key: %v", err)
	}
	if _, _, err := a.DecideFile(ctx, authz.NewSessionPrincipal("alice"), "sideways", authz.SessionResource{WorkerID: "W2", Name: "x"}); code(t, err) != proto.CodeInvalidArgument {
		t.Errorf("unknown direction: %v", err)
	}
}

func TestWithVerifiedFiles(t *testing.T) {
	p := authz.NewWorkerSessionPrincipal("A1", "main", nil, nil)
	if _, ok := p.WithVerifiedFiles("other", nil).VerifiedFiles(); ok {
		t.Error("a session other than the principal's was verified")
	}
	if _, ok := p.WithVerifiedFiles("", nil).VerifiedFiles(); ok {
		t.Error("an empty session was verified")
	}
	if s, ok := p.WithVerifiedFiles("main", nil).VerifiedFiles(); !ok || s != "main" {
		t.Errorf("VerifiedFiles = %q %v", s, ok)
	}
	if _, ok := authz.NewSessionPrincipal("alice").WithVerifiedFiles("main", nil).VerifiedFiles(); ok {
		t.Error("a person carries file authority")
	}
	if _, ok := p.VerifiedFiles(); ok {
		t.Error("WithVerifiedFiles changed the original principal")
	}
}

// The matrix of the Hub layers for both ends (FR-10, FR-18).
func TestCheckFileEnds(t *testing.T) {
	w := func(mode string, preview bool) authz.FileEnd {
		return authz.FileEnd{WorkerID: "w", Mode: mode, PreviewOnly: preview}
	}
	rest := authz.FileEnd{}
	cases := []struct {
		name           string
		policy, dir    string
		admin          bool
		target, caller authz.FileEnd
		want           proto.Code
	}{
		{"defaults pull", "", ops.FileDirectionPull, false, w("", false), w("", false), ""},
		{"defaults push", "on", ops.FileDirectionPush, false, w("on", false), w("on", false), ""},
		{"Hub off", "off", ops.FileDirectionPull, true, w("", false), rest, proto.CodeFilesDisabled},
		{"pull_only refuses push", "pull_only", ops.FileDirectionPush, false, w("", false), rest, proto.CodeFilesDisabled},
		{"pull_only allows pull", "pull_only", ops.FileDirectionPull, false, w("", false), rest, ""},
		{"push_only refuses pull", "push_only", ops.FileDirectionPull, false, w("", false), rest, proto.CodeFilesDisabled},
		{"admins_only, user", "admins_only", ops.FileDirectionPull, false, w("", false), rest, proto.CodeFilesDisabled},
		{"admins_only, admin", "admins_only", ops.FileDirectionPush, true, w("", false), rest, ""},
		{"unknown policy", "maybe", ops.FileDirectionPull, true, w("", false), rest, proto.CodeFilesDisabled},
		{"pull: target send_only", "on", ops.FileDirectionPull, false, w("send_only", false), w("", false), ""},
		{"pull: target receive_only", "on", ops.FileDirectionPull, false, w("receive_only", false), w("", false), proto.CodeFilesDisabled},
		{"pull: caller send_only", "on", ops.FileDirectionPull, false, w("", false), w("send_only", false), proto.CodeFilesDisabled},
		{"pull: caller receive_only", "on", ops.FileDirectionPull, false, w("", false), w("receive_only", false), ""},
		{"pull: target preview_only sends", "on", ops.FileDirectionPull, false, w("", true), w("", false), ""},
		{"pull: caller preview_only cannot receive", "on", ops.FileDirectionPull, false, w("", false), w("", true), proto.CodePreviewOnly},
		{"push: target preview_only", "on", ops.FileDirectionPush, false, w("", true), w("", false), proto.CodePreviewOnly},
		{"push: target send_only", "on", ops.FileDirectionPush, false, w("send_only", false), w("", false), proto.CodeFilesDisabled},
		{"push: caller receive_only", "on", ops.FileDirectionPush, false, w("", false), w("receive_only", false), proto.CodeFilesDisabled},
		{"push: caller off", "on", ops.FileDirectionPush, false, w("", false), w("off", false), proto.CodeFilesDisabled},
		{"push: REST caller", "on", ops.FileDirectionPush, false, w("receive_only", false), rest, ""},
		{"target off", "on", ops.FileDirectionPull, false, w("off", false), rest, proto.CodeFilesDisabled},
		{"unknown Worker mode", "on", ops.FileDirectionPull, false, w("sometimes", false), rest, proto.CodeFilesDisabled},
		{"no target", "on", ops.FileDirectionPull, false, rest, rest, proto.CodeInvalidArgument},
		{"unknown direction", "on", "both", false, w("", false), rest, proto.CodeInvalidArgument},
	}
	for _, c := range cases {
		if got := code(t, authz.CheckFileEnds(c.policy, c.dir, c.admin, c.target, c.caller)); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}

// The apps_install flag (v0.25.0): app_install on the target's Worker, only
// for a verified session, only at level Control, never through files_push,
// and only through its own grant or acceptance row.
func TestAppInstallFlag(t *testing.T) {
	a := dgAuthz(grantStore())
	ctx := context.Background()
	install := set(ops.ActionAppInstall)
	w2 := authz.WorkerResource{ID: "W2"}
	grant := func(level int, apps, push bool) authz.DirectGrant {
		g := ownerGrant("g", "W2", "x", level)
		g.AppsInstall, g.FilesPush = apps, push
		return g
	}
	verified := func(g authz.DirectGrant) authz.Principal {
		return authz.NewWorkerSessionPrincipal("A1", "main", nil, []authz.DirectGrant{g}).WithVerifiedFiles("main", nil)
	}
	for _, c := range []struct {
		name string
		p    authz.Principal
		res  authz.Resource
		want bool
	}{
		{"verified control grant with the flag", verified(grant(authz.LevelControl, true, false)), w2, true},
		{"flag off by default", verified(grant(authz.LevelControl, false, false)), w2, false},
		{"files_push does not imply it", verified(grant(authz.LevelControl, false, true)), w2, false},
		{"flag below control", verified(grant(authz.LevelControl-1, true, false)), w2, false},
		{"claimed session", authz.NewWorkerSessionPrincipal("A1", "main", nil, []authz.DirectGrant{grant(authz.LevelControl, true, false)}), w2, false},
		{"another Worker", verified(grant(authz.LevelControl, true, false)), authz.WorkerResource{ID: "W3"}, false},
		{"owner", authz.NewSessionPrincipal("alice"), w2, true},
		{"admin", authz.NewSessionPrincipal("admin"), w2, true},
		{"foreign user", authz.NewSessionPrincipal("bob"), w2, false},
		{"API key with everything else", authz.NewAPIKeyPrincipal("alice", everything(append(allFour, ops.ActionFileRead, ops.ActionFileWrite)...), ""), w2, false},
		{"API key naming app_install", authz.NewAPIKeyPrincipal("alice", everything(ops.ActionAppInstall), ""), w2, true},
		{"Worker identity", authz.NewWorkerPrincipal("W2"), w2, false},
	} {
		if got := a.Decide(ctx, c.p, install, c.res) == nil; got != c.want {
			t.Errorf("%s: app_install allowed=%t, want %t", c.name, got, c.want)
		}
	}
	// A master acceptance: only a row with the flag, for its own Worker.
	scope := grantScope(tW2, tW3Build)
	off := authz.FileAcceptance{ID: "acc-w2", TargetWorkerID: "W2", FilesPush: true}
	on := off
	on.AppsInstall = true
	if a.Decide(ctx, authz.NewBoundMasterPrincipal("A1", scope).WithVerifiedFiles("main", []authz.FileAcceptance{off}), install, w2) == nil {
		t.Error("an acceptance without apps_install allows app_install")
	}
	if err := a.Decide(ctx, authz.NewBoundMasterPrincipal("A1", scope).WithVerifiedFiles("main", []authz.FileAcceptance{on}), install, w2); err != nil {
		t.Errorf("an acceptance with apps_install: %v", err)
	}
	if a.Decide(ctx, authz.NewBoundMasterPrincipal("A1", scope), install, w2) == nil {
		t.Error("a master without verified authority allows app_install")
	}
}

// The basis of an app install (the audit's grant_id): the apps_install grants
// that decide it, else the acceptance row with the flag; never a grant or row
// without the flag, and GrantIDs names the same grants for app_install.
func TestAppInstallBasis(t *testing.T) {
	a := dgAuthz(grantStore())
	ctx := context.Background()
	install := set(ops.ActionAppInstall)
	w2 := authz.WorkerResource{ID: "W2"}
	flagged := ownerGrant("g-apps", "W2", "x", authz.LevelControl)
	flagged.AppsInstall = true
	plain := ownerGrant("g-plain", "W2", "x", authz.LevelControl)
	plain.FilesPush = true
	p := authz.NewWorkerSessionPrincipal("A1", "main", nil, []authz.DirectGrant{plain, flagged}).WithVerifiedFiles("main", nil)
	b, err := a.AppInstallBasis(ctx, p, w2)
	if err != nil || !slices.Equal(b.GrantIDs, []string{"g-apps"}) || b.AcceptanceID != "" || b.Standing {
		t.Fatalf("basis of an apps_install grant = %+v, %v", b, err)
	}
	if ids, err := a.GrantIDs(ctx, p, install, w2); err != nil || !slices.Equal(ids, []string{"g-apps"}) {
		t.Fatalf("GrantIDs of app_install = %v, %v", ids, err)
	}
	// A claimed session: the flag does not count.
	claimed := authz.NewWorkerSessionPrincipal("A1", "main", nil, []authz.DirectGrant{flagged})
	if b, err := a.AppInstallBasis(ctx, claimed, w2); err != nil || b.Any() {
		t.Fatalf("basis of a claimed session = %+v, %v", b, err)
	}
	if ids, err := a.GrantIDs(ctx, claimed, install, w2); err != nil || len(ids) != 0 {
		t.Fatalf("GrantIDs of a claimed session = %v, %v", ids, err)
	}

	scope := grantScope(tW2, tW3Build)
	off := authz.FileAcceptance{ID: "acc-off", TargetWorkerID: "W2", FilesPush: true}
	on := authz.FileAcceptance{ID: "acc-w2", TargetWorkerID: "W2", AppsInstall: true}
	master := authz.NewBoundMasterPrincipal("A1", scope).WithVerifiedFiles("main", []authz.FileAcceptance{off, on})
	if b, err := a.AppInstallBasis(ctx, master, w2); err != nil || len(b.GrantIDs) != 0 || b.AcceptanceID != "acc-w2" {
		t.Fatalf("basis of an acceptance = %+v, %v", b, err)
	}
	if b, err := a.AppInstallBasis(ctx, master, authz.WorkerResource{ID: "W3"}); err != nil || b.Any() {
		t.Fatalf("basis on a Worker the row does not name = %+v, %v", b, err)
	}
	if b, err := a.AppInstallBasis(ctx, authz.NewSessionPrincipal("alice"), w2); err != nil || !b.Standing || len(b.GrantIDs) != 0 {
		t.Fatalf("basis of the owner = %+v, %v", b, err)
	}
	if b, err := a.AppInstallBasis(ctx, authz.NewSessionPrincipal("bob"), w2); err != nil || b.Any() {
		t.Fatalf("basis of a foreign user = %+v, %v", b, err)
	}
}
