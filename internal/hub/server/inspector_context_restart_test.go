package server

// A restart of the Hub is not another sign-in. The inspector keeps whole session views (drafts,
// requests on their way, unknown deliveries) and clears ALL of them when the identity context of
// the page changes. Browser sessions are persisted, so the same cookie is still signed in after a
// restart; the context must therefore be the same for it, or every restart would destroy what the
// person had typed and say that "another sign-in took over".
//
// This is a real two-PROCESS test: the parent starts this test binary twice as child processes on
// ONE data directory (the Hub runs, is stopped, and a second process starts a new Hub on the same
// files). The browser sessions are real (created by the session manager, sent as real cookies).
// What is compared is only what the pages and routes answer; no cookie leaves the private
// temporary directory and none is printed.

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/store"
)

const (
	restartPhaseEnv = "MINION_TEST_INSPECTOR_RESTART_PHASE"
	restartDirEnv   = "MINION_TEST_INSPECTOR_RESTART_DIR"
)

// restartFacts is what phase 1 leaves for phase 2: the cookies of real browser sessions (private
// file, mode 0600, inside the test's temporary directory) and what the first Hub answered.
type restartFacts struct {
	Worker    string            `json:"worker"`
	OwnerID   string            `json:"owner"`
	Cookies   map[string]string `json:"cookies"` // login name -> Cookie header
	Principal map[string]string `json:"principal"`
	Page      map[string]string `json:"page"`
	Context   map[string]string `json:"context"` // the resource check of Alice's session
	PID       int               `json:"pid"`
}

var restartPageCtx = regexp.MustCompile(`id="inspector"[^>]*data-context="([0-9a-f]{32})"`)

func (d *dashHub) principalContextOf(hdr map[string]string) string {
	d.t.Helper()
	code, body, _ := d.hc.do("GET", "/inspector/principal", nil, hdr)
	var a ctxAnswer
	if code != 200 || json.Unmarshal([]byte(body), &a) != nil || a.Context == "" {
		d.t.Fatalf("GET /inspector/principal = %d", code)
	}
	return a.Context
}

func (d *dashHub) pageContextOf(hdr map[string]string) string {
	d.t.Helper()
	m := restartPageCtx.FindStringSubmatch(d.page("/sessions", hdr))
	if m == nil {
		d.t.Fatal("no data-context on the inspector of /sessions")
	}
	return m[1]
}

// TestInspectorContextRestartChild is not a test of its own: it is the body of one process of the
// restart test below, and does nothing unless that test started it.
func TestInspectorContextRestartChild(t *testing.T) {
	phase, dir := os.Getenv(restartPhaseEnv), os.Getenv(restartDirEnv)
	if phase == "" || dir == "" {
		t.Skip("only runs as a child process of TestInspectorContextSurvivesHubRestart")
	}
	data := filepath.Join(dir, "data")
	facts := filepath.Join(dir, "facts.json")
	d := startDashHubAt(t, data)
	ctx := context.Background()

	switch phase {
	case "1":
		f := restartFacts{Cookies: map[string]string{}, Principal: map[string]string{}, Page: map[string]string{}, Context: map[string]string{}, PID: os.Getpid()}
		aliceID := d.user("alice")
		d.user("bob")
		f.OwnerID = aliceID
		logins := map[string]map[string]string{}
		logins["alice1"], _ = d.login("alice")
		logins["alice2"], _ = d.login("alice") // a second login of the same person
		logins["bob"], _ = d.login("bob")
		logins["root"], _ = d.login("root")
		// root acting as alice: the same kind of browser session, with an act-as target.
		row, err := d.st.UserByIdentifier(ctx, "root")
		if err != nil {
			t.Fatal(err)
		}
		is, err := d.mgr.Create(ctx, row, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := d.mgr.StartActAs(ctx, &is.Session, aliceID, time.Hour); err != nil {
			t.Fatal(err)
		}
		logins["rootAsAlice"] = asSession(is.Token, is.CSRFToken)

		f.Worker = d.enrol("lab", aliceID, store.WorkerKindPrivate)
		d.connect(f.Worker, "lab", aliceID, "private", map[string]string{"minion-1": "task"})
		for who, hdr := range logins {
			f.Cookies[who] = hdr["Cookie"]
			f.Principal[who] = d.principalContextOf(hdr)
			f.Page[who] = d.pageContextOf(hdr)
		}
		for _, who := range []string{"alice1", "alice2", "root", "rootAsAlice"} {
			code, body, _ := d.hc.do("GET", "/s/"+f.Worker+"/minion-1/context", nil, logins[who])
			if code != 200 {
				t.Fatalf("phase 1: the context of Alice's session as %s = %d %s", who, code, body)
			}
			var a ctxAnswer
			_ = json.Unmarshal([]byte(body), &a)
			f.Context[who] = a.Context
		}
		raw, _ := json.Marshal(f)
		if err := os.WriteFile(facts, raw, 0o600); err != nil {
			t.Fatal(err)
		}

	case "2":
		raw, err := os.ReadFile(facts)
		if err != nil {
			t.Fatal(err)
		}
		var f restartFacts
		if err := json.Unmarshal(raw, &f); err != nil {
			t.Fatal(err)
		}
		if f.PID == os.Getpid() {
			t.Fatal("phase 2 runs in the process of phase 1: not a restart")
		}
		header := func(who string) map[string]string { return map[string]string{"Cookie": f.Cookies[who]} }

		// The same persisted browser session, in a NEW process: still signed in, and the very same context.
		for who := range f.Cookies {
			if got := d.principalContextOf(header(who)); got != f.Principal[who] {
				t.Errorf("%s: the context after the restart differs from before (a restart looks like another sign-in)", who)
			}
			if got := d.pageContextOf(header(who)); got != f.Page[who] {
				t.Errorf("%s: the page context after the restart differs from before", who)
			}
			if f.Page[who] != f.Principal[who] {
				t.Errorf("%s: the page and the principal route disagree", who)
			}
		}
		// Every login, user and act-as setting is still told apart after the restart.
		seen := map[string]string{}
		for who, c := range f.Principal {
			if prev, dup := seen[c]; dup {
				t.Errorf("%s and %s share a context", who, prev)
			}
			seen[c] = who
		}
		// A login made AFTER the restart is another login: another context.
		fresh, _ := d.login("alice")
		cFresh := d.principalContextOf(fresh)
		for who, c := range f.Principal {
			if c == cFresh {
				t.Errorf("a new login of alice after the restart has the context of %s", who)
			}
		}

		// The resource check of Alice's session. The Worker has not reconnected yet and a Hub that just started has
		// no list of its sessions: the Hub cannot say that the session exists, so it says 404 (the page keeps what it
		// holds for a session that is refused while the sign-in is unchanged; see inspector.js). It never answers 200
		// from a cache, and it never makes the principal look different.
		ctxPath := "/s/" + f.Worker + "/minion-1/context"
		for _, who := range []string{"alice1", "alice2", "root", "rootAsAlice", "bob"} {
			if code, _, _ := d.hc.do("GET", ctxPath, nil, header(who)); code != 404 {
				t.Errorf("%s: the resource check before the Worker reconnected = %d, want 404 (nothing is confirmed from a cache)", who, code)
			}
		}
		// The Worker reconnects: the owner and an admin get 200 with the SAME context as before the restart (so the page
		// keeps what it holds and shows it again); Bob never may.
		d.connect(f.Worker, "lab", f.OwnerID, "private", map[string]string{"minion-1": "task"})
		for _, who := range []string{"alice1", "alice2", "root", "rootAsAlice"} {
			code, body, _ := d.hc.do("GET", ctxPath, nil, header(who))
			var a ctxAnswer
			_ = json.Unmarshal([]byte(body), &a)
			if code != 200 || a.Context != f.Context[who] || a.Online == nil || !*a.Online {
				t.Errorf("%s after the Worker reconnected: %d (context kept: %v, online: %v)", who, code, a.Context == f.Context[who], a.Online)
			}
		}
		if code, _, _ := d.hc.do("GET", ctxPath, nil, header("bob")); code != 404 {
			t.Errorf("bob may not read Alice's session, the check = %d, want 404", code)
		}
		// And a session that does not exist on a connected Worker is still a plain 404.
		if code, _, _ := d.hc.do("GET", "/s/"+f.Worker+"/nope/context", nil, header("alice1")); code != 404 {
			t.Errorf("a session that does not exist on a connected Worker = %d, want 404", code)
		}
		// Nothing secret is in any answer.
		for _, p := range []string{"/inspector/principal", ctxPath} {
			_, body, _ := d.hc.do("GET", p, nil, header("alice1"))
			for _, secret := range []string{strings.TrimPrefix(f.Cookies["alice1"], "minion_session="), f.OwnerID, "alice"} {
				if secret != "" && strings.Contains(body, secret) {
					t.Errorf("%s leaks a secret after the restart", p)
				}
			}
		}
	}
}

// TestInspectorContextSurvivesHubRestart runs two real processes on one data directory, with the
// same real session cookies, and requires the inspector context to stay the same for an unchanged
// login (and to still differ for another user, another login and another act-as target).
func TestInspectorContextSurvivesHubRestart(t *testing.T) {
	if testing.Short() {
		t.Skip("starts two test processes")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	run := func(phase string) {
		t.Helper()
		cmd := exec.Command(exe, "-test.run=^TestInspectorContextRestartChild$", "-test.count=1", "-test.timeout=120s")
		cmd.Env = append(os.Environ(), restartPhaseEnv+"="+phase, restartDirEnv+"="+dir)
		out, err := cmd.CombinedOutput()
		if err != nil {
			// The child's output is the test's own log; it holds no cookie (they are only ever written to facts.json).
			t.Fatalf("process of phase %s failed: %v\n%s", phase, err, out)
		}
		if strings.Contains(string(out), "SKIP") && !strings.Contains(string(out), "PASS") {
			t.Fatalf("process of phase %s did not run its body:\n%s", phase, out)
		}
	}
	run("1")
	// The first Hub is gone; its process has ended. Only the data directory is left.
	run("2")
}
