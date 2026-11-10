package cli

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// runMCPUntilItEnds runs "minion mcp" with deps and args until it ends (a
// start that fails does) and returns its error and its log.
func runMCPUntilItEnds(t *testing.T, deps mcpDeps, args ...string) error {
	t.Helper()
	root := mcpRoot(deps)
	root.SetIn(strings.NewReader(""))
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	root.SetArgs(append([]string{"mcp"}, args...))
	return root.ExecuteContext(context.Background())
}

func uescapeAll(s string) string {
	var b strings.Builder
	for _, c := range s {
		fmt.Fprintf(&b, `\u%04x`, c)
	}
	return b.String()
}

// A credential typed into the Hub URL (a mistyped host or path) is refused at
// the start by both entry points, before any connection, any log line and any
// error message can show it.
func TestHubURLThatCarriesTheKeyIsRefusedByCLIAndMCPWithoutBeingShown(t *testing.T) {
	hub := newHubStub(t, func(w http.ResponseWriter, r *http.Request, s *hubStub) { stubJSON(w, 200, `{}`) })
	for name, hubURL := range map[string]string{
		"host":          "https://" + hub.key + ".invalid",
		"host and port": "https://" + hub.key + ".invalid:8443",
		"path":          "https://hub.invalid/" + hub.key,
		"upper case":    "https://" + strings.ToUpper(hub.key) + ".invalid",
	} {
		env := hub.env()
		env["MINION_HUB_URL"] = hubURL

		deps, stderr := hubMCPDeps(env, &stubBackend{})
		err := runMCPUntilItEnds(t, deps)
		if err == nil || !strings.Contains(err.Error(), "looks like an API key") {
			t.Errorf("%s: mcp start: %v", name, err)
		}
		shown := stderr.String()
		if err != nil {
			shown += err.Error()
		}
		if strings.Contains(strings.ToLower(shown), strings.ToLower(hub.key)) {
			t.Errorf("%s: the key is shown by minion mcp", name)
		}

		code, _, errs := runWith(t, depsWithSettings(localAnswers(`{}`), env), "", "sessions_list")
		if code != 1 || !strings.Contains(errs, "looks like an API key") || strings.Contains(strings.ToLower(errs), strings.ToLower(hub.key)) {
			t.Errorf("%s: command: exit %d, stderr %q", name, code, errs)
		}
	}
	if hub.hits.Load() != 0 {
		t.Error("a Hub was contacted")
	}
}

// Whatever Hub is configured, the start-up log of minion mcp says which kind of
// backend it uses and nothing of its address; the permanent warning of an
// insecure client stays and does not carry the URL either.
func TestMCPStartupLogNamesNoHubAddressAndKeepsTheInsecureWarning(t *testing.T) {
	hub := newHubStub(t, func(w http.ResponseWriter, r *http.Request, s *hubStub) { stubJSON(w, 200, `{"sessions":[]}`) })
	u, _ := url.Parse(hub.srv.URL)
	for _, insecure := range []bool{false, true} {
		env := hub.env()
		env["MINION_LOG_LEVEL"] = "debug"
		var args []string
		if insecure {
			delete(env, "MINION_HUB_CA")
			args = append(args, "--hub-insecure")
		}
		deps, stderr := hubMCPDeps(env, &stubBackend{})
		rpc(t, deps, args, listRequest,
			`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"sessions_list","arguments":{}}}`)
		log := stderr.String()
		if !strings.Contains(log, "calls a Hub over its REST API") {
			t.Errorf("insecure=%v: no backend line in the log: %q", insecure, log)
		}
		for _, secret := range []string{hub.key, u.Host, u.Hostname(), hub.srv.URL, "Authorization", "Bearer"} {
			if secret != "" && strings.Contains(log, secret) {
				t.Errorf("insecure=%v: the log shows %q:\n%s", insecure, secret, log)
			}
		}
		if got := strings.Contains(log, "--hub-insecure is on"); got != insecure {
			t.Errorf("insecure=%v: warning present = %v", insecure, got)
		}
	}
}

// An upstream error that spells the key with JSON escapes reaches neither the
// command line nor the MCP tool result.
func TestUpstreamErrorEscapedKeyReachesNeitherCLINorMCP(t *testing.T) {
	var key string
	hub := newHubStub(t, func(w http.ResponseWriter, r *http.Request, s *hubStub) {
		key = s.key
		stubJSON(w, 400, `{"error":{"code":"invalid_argument","message":"bad `+uescapeAll(s.key)+`","details":{"echo":"`+uescapeAll("Bearer "+s.key)+`","n":1}}}`)
	})
	code, out, errs := runWith(t, depsWithSettings(localAnswers(`{}`), hub.env()), "", "sessions_list", "--json")
	if code != 1 || !strings.Contains(errs, "invalid_argument") || strings.Contains(out+errs, key) {
		t.Errorf("command: exit %d, stdout %q, stderr %q", code, out, errs)
	}
	deps, stderr := hubMCPDeps(hub.env(), &stubBackend{})
	got, _ := rpc(t, deps, nil, listRequest,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"sessions_list","arguments":{}}}`)
	if !strings.Contains(string(got[3]), `"isError":true`) || strings.Contains(string(got[3])+stderr.String(), key) {
		t.Errorf("mcp: result %s, log %q", got[3], stderr.String())
	}
}

// --hub-worker (and MINION_HUB_WORKER and the config file) takes a Worker id.
// A name is refused by both entry points before anything is sent, never turned
// into a wrong target or the first Worker; an id is sent as worker_id.
func TestHubWorkerMustBeAnIDInCLIAndMCP(t *testing.T) {
	hub := newHubStub(t, func(w http.ResponseWriter, r *http.Request, s *hubStub) { stubJSON(w, 200, `{"sessions":[]}`) })

	code, _, errs := runWith(t, depsWithSettings(localAnswers(`{}`), hub.env()), "", "sessions_list", "--hub-worker", "named-worker")
	if code != 1 || !strings.Contains(errs, "Worker id") || strings.Contains(errs, "named-worker") {
		t.Errorf("flag with a name: exit %d, stderr %q", code, errs)
	}
	env := hub.env()
	env["MINION_HUB_WORKER"] = "named-worker"
	code, _, errs = runWith(t, depsWithSettings(localAnswers(`{}`), env), "", "sessions_list")
	if code != 1 || !strings.Contains(errs, "Worker id") {
		t.Errorf("env with a name: exit %d, stderr %q", code, errs)
	}
	deps, _ := hubMCPDeps(hub.env(), &stubBackend{})
	if err := runMCPUntilItEnds(t, deps, "--hub-worker", "named-worker"); err == nil || !strings.Contains(err.Error(), "Worker id") {
		t.Errorf("mcp with a name: %v", err)
	}
	if hub.hits.Load() != 0 {
		t.Error("a request was sent for a Worker name")
	}

	code, _, errs = runWith(t, depsWithSettings(localAnswers(`{}`), hub.env()), "", "sessions_list", "--hub-worker", hubWorkerID)
	if code != 0 {
		t.Fatalf("an id: exit %d, stderr %q", code, errs)
	}
	if reqs := hub.requests(); len(reqs) != 1 || reqs[0].Body["worker_id"] != hubWorkerID {
		t.Errorf("requests: %+v", reqs)
	}
	// A target that names its own Worker keeps it: the default never replaces it.
	const other = "01930000-0000-7000-8000-0000000000b2"
	code, _, errs = runWith(t, depsWithSettings(localAnswers(`{}`), hub.env()), "", "session_snapshot", "--target", other+":alpha", "--hub-worker", hubWorkerID)
	reqs := hub.requests()
	last := reqs[len(reqs)-1]
	tgt, _ := last.Body["target"].(map[string]any)
	if code != 0 || tgt["worker_id"] != other || tgt["session_name"] != "alpha" {
		t.Errorf("qualified target: exit %d, stderr %q, request %+v", code, errs, last)
	}
	// A target without a Worker gets the default.
	code, _, errs = runWith(t, depsWithSettings(localAnswers(`{}`), hub.env()), "", "session_snapshot", "--target", "alpha", "--hub-worker", hubWorkerID)
	reqs = hub.requests()
	last = reqs[len(reqs)-1]
	tgt, _ = last.Body["target"].(map[string]any)
	if code != 0 || tgt["worker_id"] != hubWorkerID || tgt["session_name"] != "alpha" {
		t.Errorf("target without a Worker: exit %d, stderr %q, request %+v", code, errs, last)
	}
}
