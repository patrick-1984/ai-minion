//go:build unix && tmux

package keep

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/testutil/tmuxtest"
	"github.com/patrick-1984/ai-minion/internal/worker/store"
	"github.com/patrick-1984/ai-minion/internal/worker/term"
	"github.com/patrick-1984/ai-minion/internal/worker/term/tmux"
)

// keepRig is a private tmux server with a fake claude and a shell wrapper that
// puts it on the PATH (a login shell would reset the PATH from the system
// profile), a Worker store and a home folder.
type keepRig struct {
	t      *testing.T
	srv    *tmuxtest.Server
	h      *tmux.Host
	st     *store.Store
	home   string
	bin    string
	called string // the fake claude appends its arguments here
}

func newKeepRig(t *testing.T) *keepRig {
	t.Helper()
	srv := tmuxtest.New(t)
	h, err := tmux.New(srv.Socket())
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	r := &keepRig{t: t, srv: srv, h: h, st: st, home: t.TempDir(), bin: t.TempDir()}
	r.called = filepath.Join(r.bin, "claude.calls")
	writeExec(t, filepath.Join(r.bin, "claude"), "#!/bin/sh\necho \"$*\" >> '"+r.called+"'\nwhile :; do sleep 1; done\n")
	writeExec(t, filepath.Join(r.bin, "sh"), "#!/bin/sh\nPATH='"+r.bin+"':$PATH; export PATH\n"+
		"if [ \"$1\" = -lc ]; then exec /bin/sh -c \"$2\"; fi\nexec /bin/sh \"$@\"\n")
	r.tmux("set-option", "-g", "default-shell", filepath.Join(r.bin, "sh"))
	return r
}

func writeExec(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
}

func (r *keepRig) tmux(args ...string) string {
	r.t.Helper()
	out, err := r.srv.Tmux(args...)
	if err != nil {
		r.t.Fatalf("tmux %v: %v: %s", args, err, out)
	}
	return out
}

func (r *keepRig) keeper() *Keeper {
	r.t.Helper()
	k, err := New(Config{Host: r.h, Layouts: r.h, Store: r.st, Home: r.home})
	if err != nil {
		r.t.Fatal(err)
	}
	return k
}

func (r *keepRig) instance(name string) string {
	r.t.Helper()
	all, err := r.h.ListSessions(context.Background())
	if err != nil {
		r.t.Fatal(err)
	}
	for _, s := range all {
		if s.Name == name {
			return s.Instance
		}
	}
	return ""
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func realPath(t *testing.T, p string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// A kept session that is missing at the start of the Worker is recreated with
// its windows, splits and folders (a folder that is gone falls back to the
// home folder) and its claude resumes the saved conversation; an existing
// session of a kept name is not touched; the restore is audited and runs only
// once per Worker start.
func TestKeeperSavesAndRestoresAKeptSession(t *testing.T) {
	ctx := context.Background()
	r := newKeepRig(t)
	dir1, dir2 := realPath(t, t.TempDir()), realPath(t, t.TempDir())

	// "kept": window 0 with a shell and a claude on a known conversation,
	// window 1 ("logs") in a folder that will be gone.
	r.tmux("new-session", "-d", "-s", "kept", "-x", "160", "-y", "48", "-c", dir1)
	r.tmux("split-window", "-h", "-t", "=kept:0", "-c", dir1, "--", "/bin/sh", filepath.Join(r.bin, "claude"), "--resume", convA)
	r.tmux("new-window", "-d", "-t", "=kept:", "-n", "logs", "-c", dir2)
	waitFor(t, "the first claude", func() bool { b, _ := os.ReadFile(r.called); return len(b) > 0 })
	_ = os.Remove(r.called)

	// "busy": kept, and still running when the Worker starts again.
	r.tmux("new-session", "-d", "-s", "busy", "-c", dir1)
	r.tmux("set-option", "-t", "=busy:", "@mark", "untouched")
	busyPID := strings.TrimSpace(r.tmux("display-message", "-p", "-t", "=busy:", "#{pane_pid}"))

	k := r.keeper()
	for _, n := range []string{"kept", "busy"} {
		if err := k.Keep(ctx, n, r.instance(n)); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := r.st.KeptSessions(ctx)
	if err != nil || len(rows) != 2 {
		t.Fatalf("keep list = %+v, %v", rows, err)
	}
	var saved term.SessionLayout
	for _, row := range rows {
		if row.Name == "kept" {
			if err := json.Unmarshal([]byte(row.Layout), &saved); err != nil {
				t.Fatalf("saved layout %q: %v", row.Layout, err)
			}
		}
	}
	if len(saved.Windows) != 2 || len(saved.Windows[0].Panes) != 2 || len(saved.Windows[1].Panes) != 1 || saved.Windows[1].Name != "logs" {
		t.Fatalf("saved layout = %+v", saved)
	}
	if p := saved.Windows[0].Panes[1]; p.Tool != ToolClaude || p.Conversation != convA || p.Cwd != dir1 {
		t.Fatalf("claude pane saved as %+v", p)
	}
	if p := saved.Windows[0].Panes[0]; p.Tool != "" || p.Cwd != dir1 {
		t.Fatalf("shell pane saved as %+v", p)
	}
	if strings.Contains(string(mustJSON(t, saved)), "untouched") {
		t.Fatal("a session option leaked into the layout")
	}

	// The device restarts: "kept" is gone, so is the folder of its window 1.
	r.tmux("kill-session", "-t", "=kept")
	if err := os.RemoveAll(dir2); err != nil {
		t.Fatal(err)
	}

	k2 := r.keeper() // the Worker starts again
	if !k2.Tick(ctx) {
		t.Fatal("the restore pass waited for a host that is up")
	}
	out := r.tmux("list-panes", "-s", "-t", "=kept:", "-F", "#{window_index}|#{window_name}|#{pane_current_path}|#{pane_start_command}")
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 3 {
		t.Fatalf("restored panes:\n%s", out)
	}
	if !strings.Contains(lines[0], "|"+dir1+"|") || !strings.Contains(lines[1], "|"+dir1+"|") {
		t.Fatalf("window 0 folders:\n%s", out)
	}
	if !strings.Contains(lines[2], "|logs|"+realPath(t, r.home)+"|") {
		t.Fatalf("window 1 (folder gone) is not in the home folder:\n%s", out)
	}
	if !strings.Contains(lines[1], "claude --resume '"+convA+"'") || strings.Contains(lines[0], "claude") {
		t.Fatalf("tool command lines:\n%s", out)
	}
	waitFor(t, "the restored claude", func() bool {
		b, _ := os.ReadFile(r.called)
		return strings.Contains(string(b), "--resume "+convA)
	})
	// The existing session is untouched.
	if got := strings.TrimSpace(r.tmux("display-message", "-p", "-t", "=busy:", "#{pane_pid} #{@mark}")); got != busyPID+" untouched" {
		t.Fatalf("busy = %q, want %q", got, busyPID+" untouched")
	}
	audit, err := r.st.ListAudit(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, a := range audit {
		if a.Op == AuditOpRestore {
			n++
			if a.Target != ":kept" || a.Outcome != "delivered" || a.Origin != "local" {
				t.Fatalf("audit row = %+v", a)
			}
		}
	}
	if n != 1 {
		t.Fatalf("%d restore audit rows, want 1: %+v", n, audit)
	}

	// Closed while the Worker runs: not recreated before the next start.
	r.tmux("kill-session", "-t", "=kept")
	k2.Tick(ctx)
	if _, err := r.srv.Tmux("has-session", "-t", "=kept"); err == nil {
		t.Fatal("a kept session closed while the Worker runs was recreated")
	}
	if names, _ := r.st.KeptNames(ctx); !names["kept"] {
		t.Fatal("the keep flag of a closed session was dropped")
	}
	// Forgotten: the next start does not bring it back.
	if err := k2.Unkeep(ctx, "kept"); err != nil {
		t.Fatal(err)
	}
	if done, err := r.keeper().Restore(ctx); err != nil || len(done) != 0 {
		t.Fatalf("restore after unkeep: %v %v", done, err)
	}
}

// The flag follows a rename of the same terminal session.
func TestKeeperFollowsARename(t *testing.T) {
	ctx := context.Background()
	r := newKeepRig(t)
	r.tmux("new-session", "-d", "-s", "old")
	k := r.keeper()
	if err := k.Keep(ctx, "old", r.instance("old")); err != nil {
		t.Fatal(err)
	}
	r.tmux("rename-session", "-t", "=old", "new")
	k.Tick(ctx)
	names, err := r.st.KeptNames(ctx)
	if err != nil || !names["new"] || names["old"] {
		t.Fatalf("names after rename = %v, %v", names, err)
	}
}

// ServerID names the running server, also without a session, and another
// value for another server.
func TestServerID(t *testing.T) {
	ctx := context.Background()
	r := newKeepRig(t)
	id, err := r.h.ServerID(ctx)
	if err != nil || id == "" {
		t.Fatalf("ServerID without a session = %q, %v", id, err)
	}
	other := newKeepRig(t)
	if id2, err := other.h.ServerID(ctx); err != nil || id2 == id {
		t.Fatalf("ServerID of another server = %q, %v (first %q)", id2, err, id)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
