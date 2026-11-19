package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/spf13/cobra"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/worker/agent"
	"github.com/patrick-1984/ai-minion/internal/worker/store"
	"github.com/patrick-1984/ai-minion/internal/worker/term"
)

type shareSessions []string

func (s shareSessions) ListSessions(context.Context) ([]term.Session, error) {
	var out []term.Session
	for _, n := range s {
		out = append(out, term.Session{Name: n})
	}
	return out, nil
}

type shareRig struct {
	dir  string
	env  map[string]string
	deps workerShareDeps
}

func newShareRig(t *testing.T, sessions ...string) *shareRig {
	t.Helper()
	r := &shareRig{dir: t.TempDir()}
	r.env = map[string]string{"MINION_STATE_DIR": r.dir, "MINION_TMUX_SOCKET": r.dir + "/tmux.sock"}
	r.deps = defaultWorkerShareDeps()
	r.deps.lookupEnv = func(k string) (string, bool) { v, ok := r.env[k]; return v, ok }
	r.deps.sessions = func(string) agent.StatusSessions { return shareSessions(sessions) }
	r.deps.call = func(context.Context, string, any) (json.RawMessage, error) { return nil, errors.New("no Worker") }
	return r
}

func (r *shareRig) run(t *testing.T, args ...string) (map[string]any, int) {
	t.Helper()
	root := &cobra.Command{Use: "minion", SilenceUsage: true, SilenceErrors: true}
	workerCommand(root).AddCommand(newWorkerShareCmd(r.deps))
	var out, errOut bytes.Buffer
	code := execute(context.Background(), root, append(append([]string{"worker", "share"}, args...), "--json"), &out, &errOut)
	var obj map[string]any
	if err := json.Unmarshal(out.Bytes(), &obj); err != nil {
		t.Fatalf("%v: stdout %q: %v (stderr %q)", args, out.String(), err, errOut.String())
	}
	return obj, code
}

func (r *shareRig) saved(t *testing.T) store.ShareState {
	t.Helper()
	st, err := store.Open(r.dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	s, err := st.LoadShareStateContext(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func strs(v any) []string {
	var out []string
	for _, x := range v.([]any) {
		out = append(out, x.(string))
	}
	return out
}

func TestSelectEmptyAllowedNothingShared(t *testing.T) {
	r := newShareRig(t, "a", "b")
	if _, code := r.run(t, "set", "select", "a", "b"); code != ExitOK {
		t.Fatalf("set select a b: %d", code)
	}
	obj, code := r.run(t, "set", "select")
	if code != ExitOK || obj["rule"] != "select" || len(obj["selected"].([]any)) != 0 {
		t.Fatalf("set select (none) = %v, %d", obj, code)
	}
	for _, s := range obj["sessions"].([]any) {
		if s.(map[string]any)["shared"] == true {
			t.Fatalf("a session is shared: %v", obj)
		}
	}
}

func TestAddRemoveAtomic(t *testing.T) {
	r := newShareRig(t, "a", "b", "c")
	r.run(t, "set", "none")
	before := r.saved(t).Revision
	obj, code := r.run(t, "add", "a", "b")
	if code != ExitOK || obj["rule"] != "select" || !slices.Equal(strs(obj["selected"]), []string{"a", "b"}) {
		t.Fatalf("add = %v, %d", obj, code)
	}
	if got := r.saved(t).Revision; got != before+1 {
		t.Fatalf("revision %d -> %d, want one commit", before, got)
	}
	obj, _ = r.run(t, "remove", "a")
	if !slices.Equal(strs(obj["selected"]), []string{"b"}) {
		t.Fatalf("remove = %v", obj)
	}
	// Adding under all changes nothing; removing under all needs select.
	r.run(t, "set", "all")
	rev := r.saved(t).Revision
	if _, code := r.run(t, "add", "x"); code != ExitOK || r.saved(t).Revision != rev {
		t.Fatalf("add under all: %d", code)
	}
	if _, code := r.run(t, "remove", "a"); code != ExitUsage {
		t.Fatalf("remove under all: exit %d, want 2", code)
	}
}

// A Hub share_set between the CLI's read and its change is detected (27).
func TestConcurrentHubShareSetDetected(t *testing.T) {
	r := newShareRig(t, "a")
	obj, _ := r.run(t, "list")
	rev := uint64(obj["revision"].(float64))
	st, err := store.Open(r.dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CommitShareContext(context.Background(), store.ShareSelection{Mode: store.ShareNone}); err != nil {
		t.Fatal(err)
	}
	_ = st.Close()
	obj, code := r.run(t, "set", "all", "--if-revision", strings.TrimSpace(jsonNum(rev)))
	if code != ExitStateChanged || obj["code"] != "state_changed" {
		t.Fatalf("stale set = %v, %d", obj, code)
	}
	if r.saved(t).Mode != store.ShareNone {
		t.Fatal("the stale change was applied")
	}
}

func jsonNum(n uint64) string { b, _ := json.Marshal(n); return string(b) }

// A share_existing in the environment sets the rule again at every start:
// the change is saved and the command exits 22, naming the source.
func TestEnvShareExistingOverriddenBy(t *testing.T) {
	r := newShareRig(t, "a")
	r.env["MINION_SHARE_EXISTING"] = "all"
	obj, code := r.run(t, "set", "none")
	if code != ExitNotInForce || !strings.Contains(obj["message"].(string), "env") {
		t.Fatalf("set none = %v, %d", obj, code)
	}
	if r.saved(t).Mode != store.ShareNone {
		t.Fatal("the change was not saved")
	}
}

// The commands never write share_existing; they delete it first. A crash
// between the file and the database leaves the narrower database choice,
// which the next start keeps.
func TestStaleSetAllInterruptedNarrowSurvives(t *testing.T) {
	r := newShareRig(t, "a", "b")
	r.run(t, "set", "select", "a")
	cfg, err := config.LoadFile(r.dir + "/config.json")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Set(config.KeyShareExisting, "all") // a stale wider mode in the file
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	r.deps.beforeCommit = func() error { return errors.New("killed") }
	if _, code := r.run(t, "set", "all"); code == ExitOK {
		t.Fatal("the interrupted change reported success")
	}
	if got := readConfig(t, r.dir); got["share_existing"] != "" {
		t.Fatalf("share_existing still in the file: %v", got)
	}
	// The next start applies the file (nothing) to the database: the
	// narrower selection stands.
	if s := r.saved(t); s.Mode != store.ShareSelect || !slices.Equal(s.Sessions, []string{"a"}) {
		t.Fatalf("after the crash: %+v", s)
	}
	r.deps.beforeCommit = nil
	if _, code := r.run(t, "set", "all"); code != ExitOK || readConfig(t, r.dir)["share_existing"] != "" {
		t.Fatalf("set all: %d, file %v", code, readConfig(t, r.dir))
	}
}

// A running Worker gets the op; another schema offline is 28.
func TestNewerCLIAgainstOlderWorker(t *testing.T) {
	r := newShareRig(t, "a")
	r.deps.openStore = func(context.Context, string) (*store.Store, error) {
		return nil, store.ErrSchemaMismatch
	}
	if obj, code := r.run(t, "set", "none"); code != ExitSchemaMismatch || obj["code"] != "schema_mismatch" {
		t.Fatalf("stopped older Worker = %v, %d", obj, code)
	}
	// Running: the CLI never opens the database; the Worker applies it.
	var ops []string
	r.deps.lockState = func(string) (io.Closer, error) { return nil, agent.ErrStateInUse }
	r.deps.call = func(_ context.Context, op string, _ any) (json.RawMessage, error) {
		ops = append(ops, op)
		switch op {
		case agent.OpLocalStatus:
			return json.Marshal(agent.LiveStatusReply{StateDir: r.dir, Instance: "i1"})
		case "worker_share_list+share_local.v1", "worker_share_apply+share_local.v1":
			return json.Marshal(agent.ShareListResult{Rule: "none", Revision: 4, Inventory: "ok", OverriddenBy: []string{}})
		}
		return nil, proto.NewError(proto.CodeUnsupportedOp, op)
	}
	if obj, code := r.run(t, "set", "none"); code != ExitOK || obj["rule"] != "none" {
		t.Fatalf("running Worker = %v, %d", obj, code)
	}
	if !slices.Contains(ops, "worker_share_apply+share_local.v1") {
		t.Fatalf("ops = %v", ops)
	}
}

// Gate 1: against a running Worker, "set select" asks for an exact
// replacement and never lists first: the names it computed from an earlier
// list could miss what the Hub saved since, and a selection too large to list
// must still be replaceable.
func TestLiveSetSelectReplacesWithoutListing(t *testing.T) {
	r := newShareRig(t, "work")
	var ops []string
	var applied agent.LocalShareApplyArgs
	r.deps.lockState = func(string) (io.Closer, error) { return nil, agent.ErrStateInUse }
	r.deps.call = func(_ context.Context, op string, args any) (json.RawMessage, error) {
		ops = append(ops, op)
		switch {
		case op == agent.OpLocalStatus:
			return json.Marshal(agent.LiveStatusReply{StateDir: r.dir, Instance: "i1"})
		case strings.HasPrefix(op, agent.OpLocalShareApply+"+"):
			applied = args.(agent.LocalShareApplyArgs)
			return json.Marshal(agent.ShareListResult{Rule: "select", Selected: []string{"work"}, Revision: 9, Inventory: "ok", OverriddenBy: []string{}})
		}
		return nil, proto.NewError(proto.CodeUnsupportedOp, op)
	}
	if obj, code := r.run(t, "set", "select", "work"); code != ExitOK || obj["rule"] != "select" {
		t.Fatalf("set select work = %v, %d", obj, code)
	}
	for _, op := range ops {
		if strings.HasPrefix(op, agent.OpLocalShareList) {
			t.Fatalf("set select listed the selection first: %v", ops)
		}
	}
	if !applied.Replace || applied.Mode != "select" || !slices.Equal(applied.Add, []string{"work"}) || len(applied.Remove) != 0 {
		t.Fatalf("apply args = %+v", applied)
	}
	want := "worker_share_apply+share_local.v1+share_replace.v1"
	if !slices.Contains(ops, want) {
		t.Fatalf("ops = %v; the replacement must require %s", ops, agent.FeatureShareReplace)
	}
}

// W6a with a real database: a newer CLI never moves an older Worker's database
// out of its schema. A stopped older Worker is exit 28 and its stored state
// and ledger are exactly as they were when it starts again; a running one
// handles the change itself.
func TestNewerCLIDoesNotUpgradeOlderWorkerDatabase(t *testing.T) {
	r := newShareRig(t, "a")
	// The older Worker's database: this binary's migrations without the last.
	src := os.DirFS("../worker/store")
	older := fstest.MapFS{}
	entries, err := fs.ReadDir(src, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	for _, n := range names[:len(names)-1] {
		b, err := fs.ReadFile(src, "migrations/"+n)
		if err != nil {
			t.Fatal(err)
		}
		older["migrations/"+n] = &fstest.MapFile{Data: b}
	}
	oldOpen := func() *store.Store {
		st, err := store.Open(r.dir, store.WithMigrations(older, "migrations"))
		if err != nil {
			t.Fatal(err)
		}
		return st
	}
	st := oldOpen()
	if _, err := st.ApplyShareChangeContext(context.Background(), store.ShareChange{Mode: store.ShareSelect, Add: []string{"keep"}}); err != nil {
		t.Fatal(err)
	}
	_ = st.Close()
	dbFile := filepath.Join(r.dir, store.DBFile)
	before, err := os.ReadFile(dbFile)
	if err != nil {
		t.Fatal(err)
	}
	// Stopped: the production openStore (NoMigrate) refuses with exit 28.
	if obj, code := r.run(t, "set", "all"); code != ExitSchemaMismatch || obj["code"] != "schema_mismatch" {
		t.Fatalf("stopped older Worker = %v, %d", obj, code)
	}
	if after, err := os.ReadFile(dbFile); err != nil || !bytes.Equal(after, before) {
		t.Fatalf("the refused command changed the older Worker's database file (%v)", err)
	}
	// The older Worker starts again: its schema and its state are as stored.
	st = oldOpen()
	got, err := st.LoadShareStateContext(context.Background())
	_ = st.Close()
	if err != nil || got.Mode != store.ShareSelect || !slices.Equal(got.Sessions, []string{"keep"}) {
		t.Fatalf("the older Worker's state after the command = %+v, %v", got, err)
	}
	// Running: the CLI never opens the database; the Worker gets the op.
	var ops []string
	r.deps.lockState = func(string) (io.Closer, error) { return nil, agent.ErrStateInUse }
	r.deps.call = func(_ context.Context, op string, _ any) (json.RawMessage, error) {
		ops = append(ops, op)
		if op == agent.OpLocalStatus {
			return json.Marshal(agent.LiveStatusReply{StateDir: r.dir, Instance: "i1"})
		}
		return json.Marshal(agent.ShareListResult{Rule: "all", Revision: 4, Inventory: "ok", OverriddenBy: []string{}})
	}
	if _, code := r.run(t, "set", "all"); code != ExitOK || !slices.Contains(ops, "worker_share_apply+share_local.v1") {
		t.Fatalf("running older Worker: exit %d, ops %v", code, ops)
	}
	if after, err := os.ReadFile(dbFile); err != nil || !bytes.Equal(after, before) {
		t.Fatalf("the live command touched the database file (%v)", err)
	}
}

// Gate-1 C: a refused change leaves the restrictive file rule in place
// (offline: stale explicit revision).
func TestShareRefusedChangeKeepsFileRule(t *testing.T) {
	r := newShareRig(t, "a")
	r.env["MINION_STATE_DIR"] = r.dir
	// the file says none, the database has been moved to all by "the Hub"
	if _, code := r.run(t, "set", "all"); code != ExitOK {
		t.Fatalf("set all = %d", code)
	}
	f, _ := config.LoadFile(r.dir + "/" + workerConfigName)
	f.Set(config.KeyShareExisting, "none")
	if err := f.Save(); err != nil {
		t.Fatal(err)
	}
	obj, code := r.run(t, "add", "a", "--if-revision", "999")
	if code != ExitStateChanged {
		t.Fatalf("stale add = %d %v", code, obj)
	}
	g, _ := config.LoadFile(r.dir + "/" + workerConfigName)
	if v, ok, _ := g.Lookup(config.KeyShareExisting); !ok || v != "none" {
		t.Fatalf("the file rule was removed by a refused change: %q %v", v, ok)
	}
}

// An explicit stale revision is refused even where the change is a no-op.
func TestShareNoOpChecksExplicitRevision(t *testing.T) {
	r := newShareRig(t, "a")
	if _, code := r.run(t, "set", "all"); code != ExitOK {
		t.Fatal(code)
	}
	if obj, code := r.run(t, "add", "a", "--if-revision", "999"); code != ExitStateChanged {
		t.Fatalf("no-op with stale revision = %d %v", code, obj)
	}
}

// Live: the revision read is sent with the apply when the caller gave none,
// and a replaced Worker ends the command with 27 before the file changes.
func TestShareLiveSendsReadRevisionAndChecksInstance(t *testing.T) {
	r := newShareRig(t, "a", "b")
	lockHeld := func(string) (io.Closer, error) { return nil, agent.ErrStateInUse }
	r.deps.lockState = lockHeld
	inst := "i1"
	var applied agent.LocalShareApplyArgs
	r.deps.call = func(_ context.Context, op string, args any) (json.RawMessage, error) {
		switch {
		case op == agent.OpLocalStatus:
			return json.Marshal(agent.LiveStatusReply{StateDir: r.dir, Instance: inst})
		case strings.Contains(op, agent.OpLocalShareList):
			return json.Marshal(agent.ShareListResult{Rule: "select", Selected: []string{"a"}, Revision: 7})
		default:
			applied = args.(agent.LocalShareApplyArgs)
			return json.Marshal(agent.ShareListResult{Rule: "select", Selected: []string{"b"}, Revision: 8})
		}
	}
	if obj, code := r.run(t, "add", "b"); code != ExitOK || applied.IfRevision != 7 || !slices.Equal(applied.Add, []string{"b"}) {
		t.Fatalf("code %d %v applied %+v", code, obj, applied)
	}
	// The Worker is replaced between the read and the apply.
	calls := 0
	r.deps.call = func(_ context.Context, op string, args any) (json.RawMessage, error) {
		switch {
		case op == agent.OpLocalStatus:
			calls++
			in := "i1"
			if calls > 1 {
				in = "i2"
			}
			return json.Marshal(agent.LiveStatusReply{StateDir: r.dir, Instance: in})
		case strings.Contains(op, agent.OpLocalShareList):
			return json.Marshal(agent.ShareListResult{Rule: "select", Selected: []string{"a"}, Revision: 7})
		}
		t.Fatal("apply sent to a replaced Worker")
		return nil, nil
	}
	if obj, code := r.run(t, "add", "b"); code != ExitStateChanged {
		t.Fatalf("replaced Worker = %d %v", code, obj)
	}
}
