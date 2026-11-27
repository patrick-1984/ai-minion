package web

import (
	"context"
	"crypto/rand"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/hub/api"
	"github.com/patrick-1984/ai-minion/internal/hub/audit"
	"github.com/patrick-1984/ai-minion/internal/hub/auth/session"
	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// The Add grant form submitted to the REAL grant_set handler over a real
// store (v0.26.0): no fake answers. The page's browser session is the
// API's identity, as on the Hub.

// realGrantAuthn makes the browser session of the page the API caller: the
// test session names the user, the store knows its id.
type realGrantAuthn struct{ ids map[string]string }

func (a realGrantAuthn) Authenticate(r *http.Request) (api.Identity, error) {
	s, ok := session.FromContext(r.Context())
	if !ok || a.ids[s.Username] == "" {
		return api.Identity{}, api.NewError(proto.CodeUnauthenticated, "sign in first")
	}
	id := a.ids[s.Username]
	return api.Identity{Principal: authz.NewSessionPrincipal(id), Actor: id, Via: api.ViaSession}, nil
}

type realGrantLookups struct {
	users map[string]authz.UserState
	st    *store.Store
}

func (l realGrantLookups) User(_ context.Context, id string) (authz.UserState, error) {
	u, ok := l.users[id]
	if !ok {
		return authz.UserState{}, authz.ErrNotFound
	}
	return u, nil
}

func (l realGrantLookups) WorkerOwner(ctx context.Context, id string) (string, error) {
	o, err := l.st.WorkerOwner(ctx, id)
	if err != nil {
		return "", authz.ErrNotFound
	}
	return o, nil
}

type realGrantStack struct {
	t        *testing.T
	st       *store.Store
	s        *Server
	src, tgt store.WorkerRow
}

// newRealGrantStack: alice owns the source Worker "work-laptop" (session
// wb-2) and the Android target Worker "home-pc" (sessions minion-5,
// minion-6, minion-7); root is an administrator.
func newRealGrantStack(t *testing.T) *realGrantStack {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()
	ids := map[string]string{}
	users := map[string]authz.UserState{}
	for name, role := range map[string]string{"root": "admin", "alice": "user"} {
		u := store.UserRow{ID: store.NewID(), Username: name, PasswordHash: "x", CredentialVersion: 1, CreatedAt: 1, UpdatedAt: 1}
		if err := st.CreateUser(ctx, store.CreateUserParams{User: u, Role: role}); err != nil {
			t.Fatal(err)
		}
		ids[name], users[u.ID] = u.ID, authz.UserState{Role: authz.Role(role), Active: true}
	}
	worker := func(name string) store.WorkerRow {
		pub := make([]byte, 32)
		if _, err := rand.Read(pub); err != nil {
			t.Fatal(err)
		}
		w := store.WorkerRow{ID: store.NewID(), Pub: pub, Name: name, OwnerID: ids["alice"], Kind: store.WorkerKindPrivate, CreatedAt: 1}
		if err := st.InsertWorker(ctx, nil, w); err != nil {
			t.Fatal(err)
		}
		return w
	}
	k := &realGrantStack{t: t, st: st, src: worker("work-laptop"), tgt: worker("home-pc")}

	cat := ops.NewRegistry()
	for _, n := range []string{ops.OpGrantList, ops.OpGrantSet, ops.OpGrantRevoke, ops.OpGrantApprove} {
		op, ok := ops.Lookup(n)
		if !ok {
			t.Fatalf("ops.Default lacks %s", n)
		}
		cat.Register(op)
	}
	handlers := api.NewRegistry(cat)
	list := func(ctx context.Context) ([]api.GrantWorker, error) {
		rows, err := st.ListWorkers(ctx)
		if err != nil {
			return nil, err
		}
		out := make([]api.GrantWorker, 0, len(rows))
		for _, w := range rows {
			out = append(out, api.GrantWorker{ID: w.ID, Name: w.Name, OwnerID: w.OwnerID, Owner: "alice", Approved: true, CanBeSource: true})
		}
		return out, nil
	}
	az := authz.New(realGrantLookups{users, st}, realGrantLookups{users, st})
	lg := audit.New(st)
	if err := api.RegisterGrants(handlers, st, list, az, lg); err != nil {
		t.Fatal(err)
	}
	root := http.NewServeMux()
	d := &api.Dispatcher{Catalog: cat, Handlers: handlers, Auth: realGrantAuthn{ids}, Authz: az, Audit: lg,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if _, err := api.NewRouter(d, root); err != nil {
		t.Fatal(err)
	}
	dash := Dashboard{
		Workers: []WorkerRow{
			{ID: k.src.ID, Name: "work-laptop", Owner: "alice", Kind: ops.WorkerKindPrivate, Online: true},
			{ID: k.tgt.ID, Name: "home-pc", Owner: "alice", Kind: ops.WorkerKindPrivate, Online: true, Android: true},
		},
		Sessions: []SessionRow{
			{WorkerID: k.src.ID, Worker: "work-laptop", Owner: "alice", Name: "wb-2", Code: "1-2", Kind: "shell", Online: true},
			{WorkerID: k.tgt.ID, Worker: "home-pc", Owner: "alice", Name: "minion-5", Code: "5-8", Kind: "shell", Online: true},
			{WorkerID: k.tgt.ID, Worker: "home-pc", Owner: "alice", Name: "minion-6", Kind: "shell", Online: true},
			{WorkerID: k.tgt.ID, Worker: "home-pc", Owner: "alice", Name: "minion-7", Kind: "shell", Online: true},
		},
	}
	k.s, _ = listServer(t, dash, dash)
	root.Handle("/", k.s)
	connect(t, NewHandlerCaller(root))
	return k
}

func (k *realGrantStack) grants() []store.SessionGrantRow {
	k.t.Helper()
	rows, err := k.st.ListSessionGrants(context.Background(), store.SessionGrantFilter{})
	if err != nil {
		k.t.Fatal(err)
	}
	return rows
}

// screenshotForm is the owner's report: from wb-2 on work-laptop, Control,
// Receive files and Send files ticked, no end, the note "to android".
func (k *realGrantStack) screenshotForm() url.Values {
	return url.Values{"source_scope": {"session"}, "source_session": {k.src.ID + "|wb-2"}, "level": {"control"},
		"files_pull": {"1"}, "files_push": {"1"}, "until": {""}, "note": {"to android"}}
}

// Both cases of the owner's report save in one step, with the boxes, and the
// list shows the saved rows.
func TestGrantAddScreenshotCasesSaveThroughTheRealHandler(t *testing.T) {
	k := newRealGrantStack(t)
	page := get(t, k.s, pathAccessGrants, "X-Test-Session", "alice:user").Body.String()
	mustHave(t, page, `<option value="`+k.src.ID+`|wb-2">1-2 · wb-2</option>`,
		`<select name="target_sessions" id="grant-add-target-sessions" multiple size="8"`,
		`<option value="`+k.tgt.ID+`|minion-5" data-worker="`+k.tgt.ID+`" data-android="home-pc">5-8 · minion-5</option>`,
		`<input type="radio" name="level" id="grant-add-level-view" value="view" checked>`,
		`<input type="checkbox" name="files_pull" id="grant-add-files-pull" value="1"> Receive files from target</label>`,
		`<input type="checkbox" name="files_push" id="grant-add-files-push" value="1" data-needs-control disabled> Send files to target <span class="help grant-needs-control">Requires Control</span>`,
		`Includes sessions this Worker shares later.`, `id="grant-add-submit">Add grant</button>`)
	mustNotHave(t, page, "Confirm and add", "verified source", `value="everyone"`)

	// Screenshot 2: one named target session. The live summary names it.
	named := k.screenshotForm()
	named.Set("target_scope", "sessions")
	named.Set("target_sessions", k.tgt.ID+"|minion-5")
	sum := post(t, k.s, pathAccessGrants+"/summary", "alice:user", named)
	mustHave(t, sum.Body.String(), "<strong>wb-2 can control minion-5 on home-pc and receive and send files.</strong>")
	if n := len(k.grants()); n != 0 {
		t.Fatalf("the summary wrote %d grants", n)
	}
	rec := post(t, k.s, pathAccessGrants+"/add", "alice:user", named)
	mustHave(t, rec.Body.String(), `role="status">Saved: wb-2 can control minion-5 on home-pc and receive and send files.</p>`)
	if tr := rec.Header().Get("HX-Trigger"); !strings.Contains(tr, `"grants-changed"`) || !strings.Contains(tr, `"ids"`) {
		t.Fatalf("HX-Trigger = %q", tr)
	}
	gs := k.grants()
	if len(gs) != 1 || !gs[0].FilesPull || !gs[0].FilesPush || gs[0].AppsInstall || gs[0].TargetSession != "minion-5" ||
		gs[0].SourceSession != "wb-2" || gs[0].Level != authz.LevelControl || gs[0].Note != "to android" || gs[0].ExpiresAt != nil {
		t.Fatalf("screenshot 2 = %+v", gs)
	}

	// Screenshot 1: every shared session of the target Worker.
	wide := k.screenshotForm()
	wide.Set("target_scope", "worker")
	wide.Set("target_worker_id", k.tgt.ID)
	mustHave(t, post(t, k.s, pathAccessGrants+"/summary", "alice:user", wide).Body.String(),
		"wb-2 can control every shared session on home-pc and receive and send files.")
	rec = post(t, k.s, pathAccessGrants+"/add", "alice:user", wide)
	mustHave(t, rec.Body.String(), `role="status">Saved:`)
	gs = k.grants()
	if len(gs) != 2 {
		t.Fatalf("grants = %+v (%s)", gs, rec.Body)
	}
	var w store.SessionGrantRow
	for _, g := range gs {
		if g.TargetSession == "" {
			w = g
		}
	}
	if !w.FilesPull || !w.FilesPush || w.TargetWorkerID != k.tgt.ID || w.Level != authz.LevelControl {
		t.Fatalf("screenshot 1 = %+v", w)
	}

	// The list shows the saved rows with their boxes.
	list := get(t, k.s, pathAccessGrants, "X-Test-Session", "alice:user").Body.String()
	mustHave(t, list, `<tr id="grant-`+w.ID+`"`, `Every shared session on <strong>home-pc</strong>`,
		`<strong>minion-5</strong> @ home-pc`, `<span class="chip grant-perm">Receive files</span> <span class="chip grant-perm">Send files</span>`,
		`Note: to android`)
}

// Many targets in one step, install on its own, the boxes that need
// Control, a selection kept by a refusal, and everyone for administrators.
func TestGrantAddManyTargetsInstallAndEveryoneThroughTheRealHandler(t *testing.T) {
	k := newRealGrantStack(t)
	many := url.Values{"source_scope": {"session"}, "source_session": {k.src.ID + "|wb-2"}, "target_scope": {"sessions"},
		"target_sessions": {k.tgt.ID + "|minion-6", k.tgt.ID + "|minion-7"}, "level": {"view"}, "files_pull": {"1"}}
	rec := post(t, k.s, pathAccessGrants+"/add", "alice:user", many)
	mustHave(t, rec.Body.String(), "Saved 2 grants: wb-2 can view minion-6 on home-pc and minion-7 on home-pc and receive files.")
	if gs := k.grants(); len(gs) != 2 || !gs[0].FilesPull || !gs[1].FilesPull || gs[0].ID == gs[1].ID {
		t.Fatalf("grants = %+v", gs)
	}

	// Send files below Control is refused and nothing is saved.
	push := url.Values{"source_scope": {"session"}, "source_session": {k.src.ID + "|wb-2"}, "target_scope": {"sessions"},
		"target_sessions": {k.tgt.ID + "|minion-5"}, "level": {"message"}, "files_push": {"1"}}
	mustHave(t, post(t, k.s, pathAccessGrants+"/add", "alice:user", push).Body.String(),
		`<p class="form-error" role="alert">Send files to target requires Control.</p>`)
	if n := len(k.grants()); n != 2 {
		t.Fatalf("a refused grant was saved: %d", n)
	}

	// Install apps alone, on the Android target, at Control.
	inst := url.Values{"source_scope": {"worker"}, "source_worker_id": {k.src.ID}, "target_scope": {"sessions"},
		"target_sessions": {k.tgt.ID + "|minion-5"}, "level": {"control"}, "apps_install": {"1"}}
	mustHave(t, post(t, k.s, pathAccessGrants+"/summary", "alice:user", inst).Body.String(),
		"Every shared session on work-laptop can control minion-5 on home-pc and install apps on home-pc.")
	post(t, k.s, pathAccessGrants+"/add", "alice:user", inst)
	var ig store.SessionGrantRow
	for _, g := range k.grants() {
		if g.Scope() == store.GrantSourceWorker {
			ig = g
		}
	}
	if !ig.AppsInstall || ig.FilesPull || ig.FilesPush || ig.SourceSession != "" {
		t.Fatalf("install alone = %+v", ig)
	}

	// Everyone is for administrators: a user's request is refused before
	// the Hub, an administrator's is saved.
	all := url.Values{"source_scope": {"everyone"}, "target_scope": {"sessions"}, "target_sessions": {k.tgt.ID + "|minion-5"}, "level": {"control"}}
	mustHave(t, post(t, k.s, pathAccessGrants+"/add", "alice:user", all).Body.String(), "Only an administrator can grant from everyone.")
	page := get(t, k.s, pathAccessGrants, "X-Test-Session", "root:admin").Body.String()
	mustHave(t, page, `<input type="radio" name="source_scope" id="grant-add-from-everyone" value="everyone"> Everyone`)
	mustHave(t, post(t, k.s, pathAccessGrants+"/summary", "root:admin", all).Body.String(), "Everyone can control minion-5 on home-pc.")
	rec = post(t, k.s, pathAccessGrants+"/add", "root:admin", all)
	mustHave(t, rec.Body.String(), "Saved: Everyone can control minion-5 on home-pc.")
	var eg store.SessionGrantRow
	for _, g := range k.grants() {
		if g.Scope() == store.GrantSourceEveryone {
			eg = g
		}
	}
	if eg.ID == "" || eg.FilesPull || eg.FilesPush || eg.AppsInstall || eg.Level != authz.LevelControl {
		t.Fatalf("everyone = %+v", eg)
	}
	list := get(t, k.s, pathAccessGrants, "X-Test-Session", "root:admin").Body.String()
	mustHave(t, list, `<strong>Everyone</strong> <span class="muted">every shared session on every eligible Worker</span>`,
		`Every shared session on <strong>work-laptop</strong>`, `<span class="chip grant-perm">Install apps on home-pc</span>`)

	// A missing choice names it and sends nothing.
	n := len(k.grants())
	mustHave(t, post(t, k.s, pathAccessGrants+"/add", "alice:user", url.Values{"source_scope": {"session"}, "source_session": {k.src.ID + "|wb-2"},
		"target_scope": {"sessions"}, "level": {"view"}}).Body.String(), "Choose at least one target session.")
	if len(k.grants()) != n {
		t.Fatal("an Add without a target was saved")
	}
}

// Edit sends the level with the three boxes: a cleared box is saved as off.
func TestGrantEditBoxesThroughTheRealHandler(t *testing.T) {
	k := newRealGrantStack(t)
	f := k.screenshotForm()
	f.Set("target_scope", "sessions")
	f.Set("target_sessions", k.tgt.ID+"|minion-5")
	post(t, k.s, pathAccessGrants+"/add", "alice:user", f)
	g := k.grants()[0]
	list := get(t, k.s, pathAccessGrants, "X-Test-Session", "alice:user").Body.String()
	mustHave(t, list, `<input type="checkbox" name="files_push" id="grant-`+g.ID+`-files-push" value="1" data-needs-control checked> Send files to target`,
		`id="grant-`+g.ID+`-apps-install"`, `<input type="hidden" name="flags" value="1">`)
	rev := g.Revision
	rec := post(t, k.s, pathAccessGrants+"/set-level", "alice:user", url.Values{"id": {g.ID}, "if_revision": {strconv.FormatUint(rev, 10)}, "level": {"control"},
		"flags": {"1"}, "apps_offered": {"1"}, "files_pull": {"1"}})
	mustHave(t, rec.Body.String(), `role="status">Saved</p>`)
	got, err := k.st.SessionGrant(context.Background(), g.ID)
	if err != nil || !got.FilesPull || got.FilesPush || got.AppsInstall {
		t.Fatalf("edited = %+v (%v)", got, err)
	}
	// Send files below Control is refused in the row.
	rec = post(t, k.s, pathAccessGrants+"/set-level", "alice:user", url.Values{"id": {g.ID}, "if_revision": {strconv.FormatUint(got.Revision, 10)}, "level": {"view"},
		"flags": {"1"}, "files_push": {"1"}})
	mustHave(t, rec.Body.String(), "Nothing was changed: Send files to target requires Control.")
}
