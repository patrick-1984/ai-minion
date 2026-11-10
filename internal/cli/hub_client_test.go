package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/patrick-1984/ai-minion/internal/client"
	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/mcp"
	"github.com/patrick-1984/ai-minion/internal/ops"
)

// hubWorkerID is the Worker id the tests give to --hub-worker: the Hub accepts
// only a UUIDv7 in worker_id.
const hubWorkerID = "01930000-0000-7000-8000-0000000000a1"

// hubStub is a Hub over TLS that records the requests it receives.
type hubStub struct {
	srv     *httptest.Server
	key     string
	keyFile string
	caFile  string
	hits    atomic.Int32
	mu      sync.Mutex
	reqs    []stubReq
}

type stubReq struct {
	Path, Auth string
	Body       map[string]any
}

func newHubStub(t *testing.T, h func(w http.ResponseWriter, r *http.Request, s *hubStub)) *hubStub {
	t.Helper()
	s := &hubStub{key: identity.NewAPIKey().String()}
	s.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.hits.Add(1)
		var body map[string]any
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		s.mu.Lock()
		s.reqs = append(s.reqs, stubReq{r.URL.Path, r.Header.Get("Authorization"), body})
		s.mu.Unlock()
		h(w, r, s)
	}))
	t.Cleanup(s.srv.Close)
	dir := t.TempDir()
	s.keyFile = filepath.Join(dir, "api.key")
	s.caFile = filepath.Join(dir, "ca.pem")
	mustWrite(t, s.keyFile, []byte(s.key+"\n"))
	mustWrite(t, s.caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: s.srv.Certificate().Raw}))
	return s
}

func mustWrite(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func (s *hubStub) requests() []stubReq {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]stubReq(nil), s.reqs...)
}

func stubJSON(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

// hubEnv is the environment of a client that uses s.
func (s *hubStub) env() map[string]string {
	return map[string]string{"MINION_HUB_URL": s.srv.URL, "MINION_API_KEY_FILE": s.keyFile, "MINION_HUB_CA": s.caFile}
}

// depsWithSettings are test deps whose Hub settings come from the real
// resolver (flags of the command, this environment), like the real command.
func depsWithSettings(local *fakeOpsBackend, env map[string]string) opsDeps {
	deps := testDeps(local)
	deps.hubSettings = func(cmd *cobra.Command) (config.ClientHub, error) {
		return resolveHubSettings(cmd.Flags(), envFrom(env), nil)
	}
	return deps
}

func localAnswers(raw string) *fakeOpsBackend {
	return &fakeOpsBackend{call: answerWith(json.RawMessage(raw), nil)}
}

func TestCommandEntrypointChoosesTheHubOverAPresentLocalWorker(t *testing.T) {
	hub := newHubStub(t, func(w http.ResponseWriter, r *http.Request, s *hubStub) {
		stubJSON(w, 200, `{"sessions":[{"name":"remote","created":"2026-11-09T10:00:00.000Z","windows":1,"attached":false}]}`)
	})
	local := localAnswers(`{"sessions":[{"name":"local","created":"2026-11-09T10:00:00.000Z","windows":1,"attached":false}]}`)
	code, out, errs := runWith(t, depsWithSettings(local, hub.env()), "", "sessions_list", "--hub-worker", hubWorkerID)
	if code != 0 || !strings.Contains(out, "remote") || strings.Contains(out, "local") {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, out, errs)
	}
	if local.count() != 0 {
		t.Errorf("the local Worker was called %d times although a Hub is configured", local.count())
	}
	reqs := hub.requests()
	if len(reqs) != 1 || reqs[0].Path != "/api/v1/ops/sessions_list" || reqs[0].Auth != "Bearer "+hub.key || reqs[0].Body["worker_id"] != hubWorkerID {
		t.Fatalf("requests = %+v", reqs)
	}
	if strings.Contains(out+errs, hub.key) {
		t.Error("the key is in the output")
	}
}

func TestCommandFlagsSelectTheHubAndBeatTheEnvironment(t *testing.T) {
	hub := newHubStub(t, func(w http.ResponseWriter, r *http.Request, s *hubStub) { stubJSON(w, 200, `{"sessions":[]}`) })
	local := localAnswers(`{"sessions":[]}`)
	// Only the key file is in the environment; the flags give the rest.
	env := map[string]string{"MINION_API_KEY_FILE": hub.keyFile, "MINION_HUB_URL": "https://wrong.invalid"}
	code, _, errs := runWith(t, depsWithSettings(local, env), "", "sessions_list", "--hub-url", hub.srv.URL, "--hub-ca-file", hub.caFile, "--hub-worker", hubWorkerID)
	if code != 0 || hub.hits.Load() != 1 || local.count() != 0 {
		t.Fatalf("exit %d (%s), hub hits %d, local %d", code, errs, hub.hits.Load(), local.count())
	}
}

func TestCommandHubFailureNeverFallsBackToTheLocalWorker(t *testing.T) {
	local := localAnswers(`{"sessions":[]}`)
	dir := t.TempDir()
	badKey := filepath.Join(dir, "bad")
	mustWrite(t, badKey, []byte("CANARY-NOT-A-KEY"))
	for name, env := range map[string]map[string]string{
		"unreadable key file":  {"MINION_HUB_URL": "https://hub.example", "MINION_API_KEY_FILE": filepath.Join(dir, "none")},
		"malformed key file":   {"MINION_HUB_URL": "https://hub.example", "MINION_API_KEY_FILE": badKey},
		"no key file":          {"MINION_HUB_URL": "https://hub.example"},
		"key file without URL": {"MINION_API_KEY_FILE": badKey},
		"plain key in the env": {"MINION_HUB_URL": "https://hub.example", "MINION_API_KEY": "mn_CANARYCANARYCANARYCANARY"},
		"unreadable CA file":   {"MINION_HUB_URL": "https://hub.example", "MINION_API_KEY_FILE": badKey, "MINION_HUB_CA": filepath.Join(dir, "no.pem")},
		"URL with credentials": {"MINION_HUB_URL": "https://u:pw@hub.example", "MINION_API_KEY_FILE": badKey},
	} {
		code, out, errs := runWith(t, depsWithSettings(local, env), "", "sessions_list")
		if code != ExitError || out != "" || !strings.Contains(errs, "configure the Hub backend") {
			t.Errorf("%s: exit %d, stdout %q, stderr %q", name, code, out, errs)
		}
		if strings.Contains(errs, "CANARY") || strings.Contains(errs, "pw@") {
			t.Errorf("%s: the error repeats a secret: %q", name, errs)
		}
	}
	if local.count() != 0 {
		t.Errorf("the local Worker served %d requests after a Hub failure", local.count())
	}
}

func TestCommandWithoutAHubUsesTheLocalWorkerAndSaysWhatIsMissingWhenThereIsNone(t *testing.T) {
	local := localAnswers(`{"sessions":[]}`)
	if code, _, errs := runWith(t, depsWithSettings(local, map[string]string{}), "", "sessions_list"); code != 0 || local.count() != 1 {
		t.Fatalf("local: exit %d (%s), calls %d", code, errs, local.count())
	}
	down := &fakeOpsBackend{call: answerWith(nil, client.ErrWorkerNotRunning)}
	code, _, errs := runWith(t, depsWithSettings(down, map[string]string{}), "", "sessions_list")
	if code != ExitError || !strings.Contains(errs, "MINION_HUB_URL") || !strings.Contains(errs, "no Minion Worker is running") {
		t.Errorf("no Hub, no Worker: exit %d, stderr %q", code, errs)
	}
	// A Hub operation says how to configure the Hub.
	code, _, errs = runWith(t, depsWithSettings(local, map[string]string{}), "", "workers_list")
	if code != ExitError || !strings.Contains(errs, "no Hub is configured") || !strings.Contains(errs, "MINION_HUB_URL") {
		t.Errorf("Hub op without a Hub: exit %d, stderr %q", code, errs)
	}
}

func TestCommandWriteThroughTheHubWhoseAnswerIsLostIsUnknownAndSentOnce(t *testing.T) {
	hub := newHubStub(t, func(w http.ResponseWriter, r *http.Request, s *hubStub) {
		c, _, _ := w.(http.Hijacker).Hijack()
		_ = c.Close()
	})
	local := localAnswers(`{}`)
	wid := ops.NewUUIDv7()
	code, out, errs := runWith(t, depsWithSettings(local, hub.env()), "", "send_text", "--text", "hello", "--target", wid+":work", "--json")
	if code != ExitPartial || !strings.Contains(out, `"unknown"`) || !strings.Contains(errs, "--request-id") {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, out, errs)
	}
	if hub.hits.Load() != 1 || local.count() != 0 {
		t.Errorf("hub hits %d, local %d: a write must be sent once and never elsewhere", hub.hits.Load(), local.count())
	}
	// The qualified Worker of the target went to the Hub unchanged.
	tgt, _ := hub.requests()[0].Body["target"].(map[string]any)
	if tgt["worker_id"] != wid || tgt["session_name"] != "work" {
		t.Errorf("target = %v", hub.requests()[0].Body["target"])
	}
}

func TestCommandHubRefusalMapsToTheSameCodeAsTheLocalSocket(t *testing.T) {
	hub := newHubStub(t, func(w http.ResponseWriter, r *http.Request, s *hubStub) {
		stubJSON(w, 403, `{"error":{"code":"forbidden","message":"this key may not operate there","details":{}}}`)
	})
	code, _, errs := runWith(t, depsWithSettings(localAnswers(`{}`), hub.env()), "", "sessions_list", "--hub-worker", hubWorkerID)
	if code != ExitError || !strings.Contains(errs, "forbidden: this key may not operate there") {
		t.Errorf("exit %d, stderr %q", code, errs)
	}
}

func TestCommandStreamThroughTheHubPrintsElementsAsTheyArrive(t *testing.T) {
	hub := newHubStub(t, func(w http.ResponseWriter, r *http.Request, s *hubStub) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.WriteHeader(200)
		for _, id := range []string{"t1", "t2"} {
			_, _ = io.WriteString(w, `{"turn_id":"`+id+`","session":"s","direction":"received","text":"hi","at":"2026-11-09T10:00:00.000Z"}`+"\n")
			w.(http.Flusher).Flush()
		}
	})
	wid := ops.NewUUIDv7()
	code, out, errs := runWith(t, depsWithSettings(localAnswers(`{}`), hub.env()), "", "pipe_wait", "--target", wid+":s", "--json")
	if code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errs)
	}
	if lines := strings.Split(strings.TrimSpace(out), "\n"); len(lines) != 2 || !strings.Contains(lines[0], `"t1"`) || !strings.Contains(lines[1], `"t2"`) {
		t.Errorf("stdout = %q", out)
	}
}

func TestCommandInsecureHubWarnsOnEveryRunAndChangesNothingElse(t *testing.T) {
	hub := newHubStub(t, func(w http.ResponseWriter, r *http.Request, s *hubStub) { stubJSON(w, 200, `{"sessions":[]}`) })
	env := hub.env()
	delete(env, "MINION_HUB_CA")
	// Without the switch the self-signed certificate is refused and nothing is sent.
	code, _, errs := runWith(t, depsWithSettings(localAnswers(`{}`), env), "", "sessions_list", "--hub-worker", hubWorkerID)
	if code != ExitError || !strings.Contains(errs, "--hub-ca-file") || hub.hits.Load() != 0 {
		t.Fatalf("default verification: exit %d, stderr %q, hits %d", code, errs, hub.hits.Load())
	}
	for i := 0; i < 2; i++ {
		code, _, errs = runWith(t, depsWithSettings(localAnswers(`{}`), env), "", "sessions_list", "--hub-worker", hubWorkerID, "--hub-insecure")
		if code != 0 || !strings.Contains(errs, "does not verify the Hub's TLS certificate") {
			t.Fatalf("run %d: exit %d, stderr %q", i, code, errs)
		}
	}
	// CA and insecure together are refused.
	code, _, errs = runWith(t, depsWithSettings(localAnswers(`{}`), hub.env()), "", "sessions_list", "--hub-insecure")
	if code != ExitError || !strings.Contains(errs, "cannot be combined") {
		t.Errorf("CA + insecure: exit %d, stderr %q", code, errs)
	}
}

func TestEveryOpCommandHasTheHubFlagsAndNoFlagForTheKey(t *testing.T) {
	root := &cobra.Command{Use: "minion"}
	addOpCommands(root, testDeps(localAnswers(`{}`)), ops.All())
	for _, c := range root.Commands() {
		for _, name := range []string{"hub-url", "hub-ca-file", "hub-insecure", "hub-worker"} {
			if c.Flags().Lookup(name) == nil {
				t.Errorf("%s has no --%s", c.Name(), name)
			}
		}
		c.Flags().VisitAll(func(f *pflag.Flag) {
			if strings.Contains(f.Name, "api-key") || strings.Contains(f.Usage, "mn_") && !strings.Contains(f.Usage, "mn_...") {
				t.Errorf("%s: flag --%s offers the key on the command line", c.Name(), f.Name)
			}
		})
	}
}

func TestHubFlagsOfTheCommandLineAreSourcesOnlyWhenSet(t *testing.T) {
	cmd := &cobra.Command{Use: "x"}
	addHubFlags(cmd.Flags())
	src := hubFlags{fs: cmd.Flags()}
	if _, ok, _ := src.Lookup(config.KeyHubURL); ok {
		t.Error("an unset flag is a source")
	}
	if err := cmd.Flags().Parse([]string{"--hub-insecure", "--hub-url", "https://h"}); err != nil {
		t.Fatal(err)
	}
	if v, ok, _ := src.Lookup(config.KeyHubInsecure); !ok || v != "true" {
		t.Errorf("insecure = %q, %v", v, ok)
	}
	if v, ok, _ := src.Lookup(config.KeyHubURL); !ok || v != "https://h" {
		t.Errorf("url = %q, %v", v, ok)
	}
	if _, ok, _ := src.Lookup(config.KeyAPIKey); ok {
		t.Error("the API key has a flag source")
	}
}

func TestHubSettingsReadTheConfigFileButNeverAKeyFromIt(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "worker.json")
	mustWrite(t, cfgPath, []byte(`{"hub_url":"https://file.example","hub_worker":"`+hubWorkerID+`"}`))
	load := func(stateDir string) (config.FileSource, error) { return config.LoadFile(cfgPath) }
	env := envFrom(map[string]string{"MINION_STATE_DIR": dir})
	got, err := resolveHubSettings(nil, env, load)
	if err != nil || got.URL != "https://file.example" || got.Worker != hubWorkerID {
		t.Fatalf("%+v, %v", got, err)
	}
	// The environment wins over the file.
	env = envFrom(map[string]string{"MINION_STATE_DIR": dir, "MINION_HUB_URL": "https://env.example"})
	if got, err = resolveHubSettings(nil, env, load); err != nil || got.URL != "https://env.example" {
		t.Errorf("%+v, %v", got, err)
	}
	// A key in the file is refused.
	mustWrite(t, cfgPath, []byte(`{"hub_url":"https://file.example","api_key":"mn_CANARYCANARYCANARY0000"}`))
	if _, err = resolveHubSettings(nil, envFrom(map[string]string{"MINION_STATE_DIR": dir}), load); err == nil || strings.Contains(err.Error(), "CANARY") {
		t.Errorf("api_key in the file: %v", err)
	}
}

// ---- minion mcp ----

func hubMCPDeps(env map[string]string, local client.Backend) (mcpDeps, *lockedBuf) {
	deps, stderr := testMCPDeps(env, nil, nil)
	deps.backend = nil
	deps.local = func() client.Backend { return local }
	return deps, stderr
}

func TestMCPEntrypointChoosesTheHubAndKeepsTheSameTools(t *testing.T) {
	hub := newHubStub(t, func(w http.ResponseWriter, r *http.Request, s *hubStub) {
		stubJSON(w, 200, `{"sessions":[{"name":"remote","created":"2026-11-09T10:00:00.000Z","windows":1,"attached":false}]}`)
	})
	local := &stubBackend{out: map[string]string{ops.OpSessionsList: `{"sessions":[]}`}}
	deps, stderr := hubMCPDeps(hub.env(), local)
	got, _ := rpc(t, deps, []string{"--hub-worker", hubWorkerID}, listRequest,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"sessions_list","arguments":{}}}`)
	if !strings.Contains(string(got[3]), "remote") || strings.Contains(string(got[3]), `"isError":true`) {
		t.Fatalf("tools/call = %s", got[3])
	}
	if len(local.ops()) != 0 {
		t.Errorf("the local Worker was used: %v", local.ops())
	}
	reqs := hub.requests()
	if len(reqs) != 1 || reqs[0].Auth != "Bearer "+hub.key || reqs[0].Body["worker_id"] != hubWorkerID {
		t.Fatalf("requests = %+v", reqs)
	}
	if strings.Contains(stderr.String(), hub.key) || strings.Contains(string(got[3]), hub.key) {
		t.Error("the key reached a log or a result")
	}
	// The tool set is the one of Solo mode, and still read-only.
	hubTools := toolNamesOf(t, got[2])
	localDeps, _ := testMCPDeps(map[string]string{}, local, mcp.TrustAll)
	soloGot, _ := rpc(t, localDeps, nil, listRequest)
	soloTools := toolNamesOf(t, soloGot[2])
	if len(hubTools) != len(soloTools) {
		t.Errorf("tools differ: hub %v, solo %v", hubTools, soloTools)
	}
	for name := range soloTools {
		if !hubTools[name] {
			t.Errorf("tool %s is missing in Hub mode", name)
		}
	}
	for _, w := range []string{ops.OpSendText, ops.OpSendKeys, ops.OpSessionCreate, ops.OpAnswer, ops.OpPipeSend, ops.OpSetTask} {
		if hubTools[w] {
			t.Errorf("write tool %s exposed in Hub mode without --allow-writes", w)
		}
	}
}

func TestMCPHubModeDoesNotLowerTheWriteOptIn(t *testing.T) {
	hub := newHubStub(t, func(w http.ResponseWriter, r *http.Request, s *hubStub) { stubJSON(w, 200, `{}`) })
	deps, _ := hubMCPDeps(hub.env(), &stubBackend{})
	got, _ := rpc(t, deps, []string{"--allow-writes"}, listRequest)
	if !toolNamesOf(t, got[2])[ops.OpSendText] {
		t.Error("--allow-writes does not expose the write tools in Hub mode")
	}
}

func TestMCPHubFailureEndsTheCommandInsteadOfUsingTheLocalWorker(t *testing.T) {
	local := &stubBackend{}
	bad := filepath.Join(t.TempDir(), "bad")
	mustWrite(t, bad, []byte("CANARY-NOT-A-KEY"))
	for name, env := range map[string]map[string]string{
		"malformed key": {"MINION_HUB_URL": "https://hub.example", "MINION_API_KEY_FILE": bad},
		"no key":        {"MINION_HUB_URL": "https://hub.example"},
		"plain key":     {"MINION_HUB_URL": "https://hub.example", "MINION_API_KEY": "mn_CANARYCANARYCANARY0000"},
	} {
		deps, _ := hubMCPDeps(env, local)
		root := mcpRoot(deps)
		root.SetIn(strings.NewReader(""))
		root.SetOut(io.Discard)
		var errb bytes.Buffer
		root.SetErr(&errb)
		root.SetArgs([]string{"mcp"})
		err := root.ExecuteContext(context.Background())
		if err == nil || !strings.Contains(err.Error(), "configure the Hub backend") || strings.Contains(err.Error(), "CANARY") {
			t.Errorf("%s: error = %v", name, err)
		}
	}
	if len(local.ops()) != 0 {
		t.Error("the local Worker was used")
	}
}

func TestMCPWithoutAHubStillUsesTheLocalWorkerAndStartsWithoutOne(t *testing.T) {
	local := &stubBackend{out: map[string]string{ops.OpSessionsList: `{"sessions":[]}`}}
	deps, _ := hubMCPDeps(map[string]string{"MINION_STATE_DIR": ""}, local)
	got, _ := rpc(t, deps, nil, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"sessions_list","arguments":{}}}`)
	if strings.Contains(string(got[3]), `"isError":true`) || len(local.ops()) != 1 {
		t.Errorf("tools/call = %s, local ops %v", got[3], local.ops())
	}
}

func TestMCPInsecureHubWarnsInTheLogAndNeverLogsTheKey(t *testing.T) {
	hub := newHubStub(t, func(w http.ResponseWriter, r *http.Request, s *hubStub) { stubJSON(w, 200, `{"sessions":[]}`) })
	env := hub.env()
	delete(env, "MINION_HUB_CA")
	env["MINION_HUB_INSECURE"] = "true"
	deps, stderr := hubMCPDeps(env, &stubBackend{})
	rpc(t, deps, []string{"--hub-worker", hubWorkerID}, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"sessions_list","arguments":{}}}`)
	if !strings.Contains(stderr.String(), "does not verify the Hub's TLS certificate") || strings.Contains(stderr.String(), hub.key) {
		t.Errorf("stderr = %q", stderr.String())
	}
}

func TestMCPOverTheHubCarriesWhatTheHubWithheldToTheModel(t *testing.T) {
	hub := newHubStub(t, func(w http.ResponseWriter, r *http.Request, s *hubStub) {
		stubJSON(w, 200, `{"turns":[],"withheld":2,"reason":"untrusted_peer","notice":"2 pipe turn(s) were withheld because the peer is not trusted."}`)
	})
	deps, _ := hubMCPDeps(hub.env(), &stubBackend{})
	wid := ops.NewUUIDv7()
	got, _ := rpc(t, deps, nil, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"pipe_read","arguments":{"target":{"worker_id":"`+wid+`","session_name":"s"}}}}`)
	res := string(got[3])
	if !strings.Contains(res, `"withheld":2`) || !strings.Contains(res, "untrusted_peer") || strings.Contains(res, `"isError":true`) {
		t.Fatalf("pipe_read over the Hub = %s", res)
	}
}
