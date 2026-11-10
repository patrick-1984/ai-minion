//go:build unix

package cli

import (
	"context"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/testutil/tmuxtest"
	"github.com/patrick-1984/ai-minion/internal/worker/agent"
	"github.com/patrick-1984/ai-minion/internal/worker/store"
	"github.com/patrick-1984/ai-minion/internal/worker/term"
	"github.com/patrick-1984/ai-minion/internal/worker/term/tmux"
)

// privateTmux starts a tmux server on a private socket with three sessions and
// returns it with the sessions in the order the wizard lists them. The test
// never touches the default tmux server: TMUX and TMUX_PANE are unset and every
// command names the private socket.
func privateTmux(t *testing.T) (*tmuxtest.Server, []term.Session) {
	t.Helper()
	for _, k := range []string{"TMUX", "TMUX_PANE"} {
		t.Setenv(k, "")
		if err := os.Unsetenv(k); err != nil {
			t.Fatal(err)
		}
	}
	srv := tmuxtest.New(t)
	for _, n := range []string{"alpha", "beta", "gamma"} {
		srv.NewSession(n, "cat")
	}
	h, err := tmux.New(srv.Socket())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	list, err := h.ListSessions(ctx)
	if err != nil || len(list) != 3 {
		t.Fatalf("sessions %v err %v", list, err)
	}
	return srv, list
}

func numberOf(t *testing.T, list []term.Session, name string) string {
	t.Helper()
	for i, s := range list {
		if s.Name == name {
			return strconv.Itoa(i + 1)
		}
	}
	t.Fatalf("no session %q in %v", name, list)
	return ""
}

func sessionsAfter(t *testing.T, socket string) []term.Session {
	t.Helper()
	h, err := tmux.New(socket)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	list, err := h.ListSessions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return list
}

// Integration (real tmux, private socket): "choose now" ticks two of three
// sessions in the wizard; the choice is what the store and the Worker's share
// policy then say, and no session was renamed, killed or changed.
func TestWorkerSetupChooseNowPersistsTheSelectionWithRealTmux(t *testing.T) {
	srv, list := privateTmux(t)
	r := newSetupRig(t, true)
	r.deps.sessions = nil // the real tmux lister
	r.env["MINION_TMUX_SOCKET"] = srv.Socket()

	k := newKeys(t)
	k.typeLines("2", numberOf(t, list, "alpha")+" "+numberOf(t, list, "gamma"), "", "") // choose; tick; done; Hub: Solo
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := r.run(ctx, k.r); err != nil {
		t.Fatalf("worker setup: %v\n%s", err, r.stdout)
	}
	out := r.stdout.String()
	for _, want := range []string{"I see 3 tmux sessions:", "[x] ", "Sharing: share only 2 sessions: alpha, gamma."} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}

	st, err := store.OpenContext(ctx, r.state)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	sel, err := st.LoadShareContext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got := append([]string(nil), sel.Sessions...)
	sort.Strings(got)
	if !sel.Configured || sel.Mode != store.ShareSelect || strings.Join(got, ",") != "alpha,gamma" {
		t.Fatalf("persisted selection = %+v, want select [alpha gamma]", sel)
	}
	// The Worker's own policy reads the same store: the unchosen session is hidden.
	pol := agent.NewPolicy(st)
	for name, want := range map[string]bool{"alpha": true, "beta": false, "gamma": true} {
		if shared, err := pol.Shared(ctx, name); err != nil || shared != want {
			t.Errorf("Shared(%q) = %v, %v; want %v", name, shared, err, want)
		}
	}
	// No session was renamed or killed.
	after := sessionsAfter(t, srv.Socket())
	if len(after) != len(list) {
		t.Fatalf("sessions before %d, after %d", len(list), len(after))
	}
	for i := range list {
		if after[i].Name != list[i].Name || !after[i].Created.Equal(list[i].Created) {
			t.Errorf("session %d changed: %+v -> %+v", i, list[i], after[i])
		}
	}
}

// "Configure later from the Hub" shares nothing until somebody chooses: the
// policy fails closed for every real session.
func TestWorkerSetupLaterFailsClosedWithRealTmux(t *testing.T) {
	srv, _ := privateTmux(t)
	r := newSetupRig(t, true)
	r.deps.sessions = nil
	r.env["MINION_TMUX_SOCKET"] = srv.Socket()
	k := newKeys(t)
	k.typeLines("3", "") // later; Hub: Solo
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := r.run(ctx, k.r); err != nil {
		t.Fatalf("worker setup: %v\n%s", err, r.stdout)
	}
	st, err := store.OpenContext(ctx, r.state)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	pol := agent.NewPolicy(st)
	for _, name := range []string{"alpha", "beta", "gamma"} {
		if shared, err := pol.Shared(ctx, name); err != nil || shared {
			t.Errorf("Shared(%q) = %v, %v; later must share nothing", name, shared, err)
		}
	}
	sel, _ := st.LoadShareContext(ctx)
	if !sel.Configured || sel.Mode != store.ShareLater {
		t.Errorf("selection = %+v", sel)
	}
	if strings.Contains(strings.ToLower(r.stdout.String()), "already configured") {
		t.Errorf("the wizard claimed a configuration that does not exist:\n%s", r.stdout)
	}
}
