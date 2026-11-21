//go:build a77_native && linux

package fixtures

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/testutil/tmuxtest"
	"golang.org/x/sys/unix"
)

const (
	dummyKey   = "sk-ant-a77-loopback-fixture-only"
	masterTool = "mcp__minion_fixture__master_list"
	baseTool   = "mcp__baseline_fixture__baseline"
)

type nativeFixture struct {
	broker         *fixtureBroker
	tmux           *tmuxtest.Server
	model          *fixtureModel
	launch         func(string, bool) *terminal
	verifySettings func()
}

func newNativeFixture(t *testing.T, interactive bool) nativeFixture {
	t.Helper()
	claude, err := filepath.EvalSymlinks(findClaude(t))
	must(t, err)
	self, err := os.Executable()
	must(t, err)
	root, err := os.MkdirTemp("", "a77-")
	must(t, err)
	t.Cleanup(func() { must(t, os.RemoveAll(root)) })
	profile, project := filepath.Join(root, "profile"), filepath.Join(root, "project")
	must(t, os.Mkdir(profile, 0o700))
	must(t, os.Mkdir(project, 0o700))
	settings := filepath.Join(profile, "settings.json")
	profileContents := `{"spinnerTipsEnabled":false}`
	must(t, os.WriteFile(settings, []byte(profileContents), 0o600))

	model := startModel(t)
	tmux := tmuxtest.New(t)
	_, err = tmux.Tmux("set-option", "-wg", "remain-on-exit", "on")
	must(t, err)
	b := newBroker(t, tmux, self, filepath.Join(root, "broker.sock"))
	b.open(t)
	launch := func(name string, prepared bool) *terminal {
		config := map[string]any{
			"baseline_fixture": map[string]any{
				"type": "stdio", "command": self,
				"args": []string{"-test.run=^TestFixtureChild$", "--", "baseline"},
			},
		}
		if prepared {
			config["minion_fixture"] = map[string]any{
				"type": "stdio", "command": self,
				"args": []string{"-test.run=^TestFixtureChild$", "--", "master", b.path},
			}
		}
		rawConfig, err := json.Marshal(map[string]any{"mcpServers": config})
		must(t, err)
		return launchTerminal(t, tmux, self, childLaunch{
			Name: name, Root: root, Claude: claude, Profile: profile, Project: project,
			ModelPort: model.port, MCPConfig: string(rawConfig), Interactive: interactive,
		})
	}
	verifySettings := func() {
		got, err := os.ReadFile(settings)
		must(t, err)
		if string(got) != profileContents {
			t.Fatal("shared fixture settings changed")
		}
	}
	return nativeFixture{b, tmux, model, launch, verifySettings}
}

func TestClaudeNative(t *testing.T) {
	f := newNativeFixture(t, false)
	b, tmux, model, launch := f.broker, f.tmux, f.model, f.launch
	a, other := launch("master", true), launch("other", false)
	b.pin(t, a.target)
	initialA := a.ask(t, model, "idle")
	initialB := other.ask(t, model, "idle")
	requireTools(t, initialA, false)
	requireTools(t, initialB, false)
	b.wait(t, "initial native tools/list", func(s brokerEvidence) bool {
		return s.Lists > 0 && s.PeerPID > 0 && s.Rejected == 0
	})
	t.Log("both CLI processes expose only the baseline MCP tool before nomination")

	b.set(true, true, false)
	b.waitState(t, "activation")
	requireTools(t, a.awaitTools(t, model, "activation", true), true)

	b.set(false, false, false)
	b.waitState(t, "healthy withdrawal before first use")
	a.awaitWithdrawal(t, model, "healthy-unused")

	b.set(true, true, false)
	b.waitState(t, "reactivate before call")
	requireTools(t, a.awaitTools(t, model, "reactivate-before-call", true), true)
	requireTools(t, a.ask(t, model, "active-call"), true)
	requireSameCatalog(t, initialB, other.ask(t, model, "active"))
	b.wait(t, "successful native tools/call", func(s brokerEvidence) bool { return s.Delivered == 1 })
	t.Log("nominated CLI adds the master MCP tool on a later turn and calls it once")

	b.set(false, true, true)
	b.waitState(t, "failed refresh")
	requireTools(t, a.ask(t, model, "stale-call"), true)
	b.wait(t, "stale native tool rejected at broker", func(s brokerEvidence) bool {
		return s.Denied == 1 && s.ListErrors > 0
	})
	t.Log("native CLI retains a cached tool after failed rediscovery, but the fixture authority denies its call")

	b.set(false, false, false)
	b.waitState(t, "withdrawal")
	if b.evidenceNow().LastToolCount != 0 {
		t.Fatal("the broker did not observe an empty tools/list response")
	}
	a.awaitWithdrawal(t, model, "recovered-withdrawal")
	a.ask(t, model, "withdrawn-cache-call")
	if !a.lastToolError || b.evidenceNow().Delivered != 1 {
		t.Fatal("replaying a cached tool after healthy withdrawal must return an error")
	}
	requireSameCatalog(t, initialB, other.ask(t, model, "revoked"))
	t.Log("healthy native refresh removes master tools and preserves baseline tools")

	// This command runs only a fixture process in the other private terminal.
	// It presents the master pane's copied metadata and its real kernel identity.
	_, err := tmux.Tmux("new-window", "-d", "-t", other.target, "--", b.self,
		"-test.run=^TestFixtureChild$", "--", "wrong-peer", b.path, b.pane().pane)
	must(t, err)
	b.wait(t, "sibling with copied metadata rejected", func(s brokerEvidence) bool { return s.Rejected == 1 })

	b.set(true, true, false)
	b.waitState(t, "reactivation")
	requireTools(t, a.awaitTools(t, model, "reactivated", true), true)
	b.close()
	// The same connected Claude's own MCP child notices local broker loss.
	// Probe additional turns only if native rediscovery is still in progress.
	a.awaitWithdrawal(t, model, "broker-down")
	b.open(t)
	b.wait(t, "same helper reconnected", func(s brokerEvidence) bool {
		return s.Applied == b.revision() && s.ListsAfterReopen > 0
	})
	requireTools(t, a.awaitTools(t, model, "broker-up", true), true)
	t.Log("same authenticated helper removed tools offline and recovered on broker reconnect")

	beforeClear := a.lastSessionID
	a.send(t, "/clear")
	a.awaitResult(t)
	afterClear := a.ask(t, model, "after-clear")
	requireTools(t, afterClear, true)
	if beforeClear == a.lastSessionID {
		t.Log("print stream accepted the /clear probe, but no native session ID change was observed")
	} else {
		t.Log("native clear changed the chat ID while the pinned terminal and helper retained the binding")
	}

	_, err = tmux.Tmux("link-window", "-s", a.target+"0", "-t", other.target+"9")
	must(t, err)
	b.wait(t, "linked pane retired", func(s brokerEvidence) bool { return s.Retired == "pane belongs to multiple sessions" })
	a.awaitWithdrawal(t, model, "linked")
	_, err = tmux.Tmux("unlink-window", "-t", other.target+"9")
	must(t, err)
	a.awaitWithdrawal(t, model, "unlinked")
	t.Log("linked pane loses authority and unlinking does not restore it")

	// Deliberately nominate the original terminal again to test a distinct move.
	b.pin(t, a.target)
	b.set(true, true, false)
	b.waitState(t, "new fixture nomination")
	requireTools(t, a.awaitTools(t, model, "before-move", true), true)
	_, err = tmux.Tmux("new-window", "-d", "-t", a.target, "--", "/usr/bin/sleep", "30")
	must(t, err)
	_, err = tmux.Tmux("move-window", "-s", a.target+"0", "-t", other.target+"9")
	must(t, err)
	b.wait(t, "moved pane retired", func(s brokerEvidence) bool { return s.Retired == "terminal mapping changed" })
	a.awaitWithdrawal(t, model, "moved")
	_, err = tmux.Tmux("kill-session", "-t", a.target)
	must(t, err)
	replacement := launch("master", true)
	requireTools(t, replacement.ask(t, model, "replacement"), false)
	requireSameCatalog(t, initialB, other.ask(t, model, "after-move"))
	t.Log("moved pane and new terminal with the former name never inherit the grant")

	b.pin(t, replacement.target)
	b.set(true, true, false)
	b.waitState(t, "explicit nomination of replacement terminal")
	requireTools(t, replacement.awaitTools(t, model, "replacement-active", true), true)
	old := b.pane()
	_, err = tmux.Tmux("respawn-pane", "-k", "-t", old.pane, "--", "/usr/bin/sleep", "30")
	must(t, err)
	_ = replacement.input.Close()
	eventually(t, "same pane replacing the harness", func() bool {
		matches, err := b.rows(old.pane)
		return err == nil && len(matches) == 1 && matches[0].harness != old.harness.pid
	}, func() string { return old.pane })
	if b.observeLifecycle() {
		t.Fatal("pinned role survived replacement of the vendor process in the same pane")
	}
	f.verifySettings()
	t.Log("replacing the harness inside the same pane also retires the binding")
}

func TestClaudeInteractive(t *testing.T) {
	f := newNativeFixture(t, true)
	b, tmux, model := f.broker, f.tmux, f.model
	a, other := f.launch("master", true), f.launch("other", false)
	b.pin(t, a.target)
	for _, p := range []*terminal{a, other} {
		eventually(t, p.name+" TUI ready", func() bool {
			raw := tmux.Capture(p.target)
			if strings.Contains(raw, "trust this folder") {
				tmux.SendKeys(p.target, "Enter")
			}
			return strings.Contains(raw, "Claude Code v2.1.330") && strings.Contains(raw, "❯") &&
				!strings.Contains(raw, "trust this folder")
		}, func() string { return logTail([]byte(tmux.Capture(p.target))) })
	}
	b.wait(t, "TUI dormant MCP registration", func(s brokerEvidence) bool { return s.Lists > 0 })
	requireTools(t, a.awaitTools(t, model, "tui-idle", false), false)
	initialOther := other.awaitTools(t, model, "tui-idle", false)
	requireTools(t, initialOther, false)

	b.set(true, true, false)
	b.waitState(t, "TUI activation")
	requireTools(t, a.awaitTools(t, model, "tui-active", true), true)
	requireSameCatalog(t, initialOther, other.ask(t, model, "tui-active"))

	b.set(false, false, false)
	b.waitState(t, "TUI withdrawal")
	if b.evidenceNow().LastToolCount != 0 {
		t.Fatal("the TUI did not fetch an empty MCP tool catalog")
	}
	// The API request may retain declarations for the historical prefix after
	// the live MCP list changes.
	tools := a.ask(t, model, "tui-revoked")
	t.Logf("TUI requested zero live MCP tools on revoke, model retained declaration=%t", slices.Contains(tools, masterTool))
	tmux.SendKeys(a.target, "/mcp", "Enter")
	panel := tmux.WaitFor(a.target, "minion_fixture", 6*time.Second)
	withdrawn := false
	for _, line := range strings.Split(panel, "\n") {
		if strings.Contains(line, "minion_fixture") || strings.Contains(line, "baseline_fixture") {
			t.Logf("TUI MCP entry after revoke: %s", strings.TrimSpace(line))
		}
		if strings.Contains(line, "minion_fixture") && strings.Contains(line, "no tools") && strings.Contains(line, "connected") {
			withdrawn = true
		}
	}
	if !withdrawn {
		t.Fatal("TUI should retain a connected server entry with no master tools")
	}
	tmux.SendKeys(a.target, "Escape")
	requireSameCatalog(t, initialOther, other.ask(t, model, "tui-revoked"))
	f.verifySettings()
}

type terminal struct {
	name, target, output, stderr string
	input                        *os.File
	results                      int
	lastSessionID                string
	lastInitTools                []string
	lastToolError                bool
	lastToolResult               string
	interactive                  *tmuxtest.Server
}

func launchTerminal(t *testing.T, tmux *tmuxtest.Server, self string, cfg childLaunch) *terminal {
	t.Helper()
	dir, err := os.MkdirTemp(cfg.Root, cfg.Name+"-")
	must(t, err)
	cfg.Input, cfg.Output, cfg.Stderr = filepath.Join(dir, "input"), filepath.Join(dir, "output"), filepath.Join(dir, "stderr")
	t.Cleanup(func() {
		if t.Failed() {
			stderr, _ := os.ReadFile(cfg.Stderr)
			pane, _ := tmux.Tmux("capture-pane", "-p", "-t", "="+cfg.Name+":0.0")
			t.Logf("%s fixture stderr: %s; pane: %s", cfg.Name, logTail(stderr), strings.TrimSpace(pane))
		}
	})
	var in *os.File
	if !cfg.Interactive {
		must(t, unix.Mkfifo(cfg.Input, 0o600))
		in, err = os.OpenFile(cfg.Input, os.O_RDWR, 0)
		must(t, err)
		t.Cleanup(func() { _ = in.Close() })
	}
	raw, err := json.Marshal(cfg)
	must(t, err)
	input := filepath.Join(dir, "launch.json")
	must(t, os.WriteFile(input, raw, 0o600))
	target := tmux.NewSession(cfg.Name, self, "-test.run=^TestFixtureChild$", "--", "launch", input)
	rawPID, err := tmux.Tmux("display-message", "-p", "-t", target, "#{pane_pid}")
	must(t, err)
	pid, err := strconv.Atoi(strings.TrimSpace(rawPID))
	must(t, err)
	child, err := pinProcess(pid)
	must(t, err)
	t.Cleanup(func() {
		defer child.close()
		if in != nil {
			_ = in.Close()
		}
		stopFixture(child)
	})
	p := &terminal{name: cfg.Name, target: target, input: in, output: cfg.Output, stderr: cfg.Stderr}
	if cfg.Interactive {
		p.interactive = tmux
	}
	return p
}

func (p *terminal) send(t *testing.T, prompt string) {
	t.Helper()
	must(t, json.NewEncoder(p.input).Encode(map[string]any{
		"type": "user", "parent_tool_use_id": nil,
		"message": map[string]string{"role": "user", "content": prompt},
	}))
}

func (p *terminal) awaitResult(t *testing.T) {
	t.Helper()
	eventually(t, p.name+" native result", func() bool {
		f, err := os.Open(p.output)
		if err != nil {
			return false
		}
		defer f.Close()
		scanner := bufio.NewScanner(f)
		scanner.Buffer(make([]byte, 4096), 4<<20)
		results := 0
		var toolError bool
		var toolResult string
		for scanner.Scan() {
			var event struct {
				Type      string          `json:"type"`
				SessionID string          `json:"session_id"`
				Error     bool            `json:"is_error"`
				Subtype   string          `json:"subtype"`
				Version   string          `json:"claude_code_version"`
				Tools     []string        `json:"tools"`
				Message   json.RawMessage `json:"message"`
			}
			if json.Unmarshal(scanner.Bytes(), &event) != nil {
				continue
			}
			if event.Type == "system" && event.Subtype == "init" {
				if event.Version != "2.1.330" {
					t.Fatalf("expected Claude CLI 2.1.330, native init reported %q", event.Version)
				}
				p.lastInitTools = event.Tools
			}
			if event.Type != "result" {
				if event.Type == "user" {
					var message struct {
						Content []struct {
							Type    string
							Content json.RawMessage
							IsError bool `json:"is_error"`
						}
					}
					if json.Unmarshal(event.Message, &message) == nil {
						for _, block := range message.Content {
							if block.Type == "tool_result" {
								toolError, toolResult = block.IsError, printableFixtureResult(block.Content)
							}
						}
					}
				}
				continue
			}
			results++
			if results == p.results+1 {
				if event.Error {
					t.Fatalf("%s model interaction failed (%s): %s", p.name, event.Subtype, scanner.Text())
				}
				p.lastSessionID = event.SessionID
				p.results = results
				p.lastToolError = toolError
				p.lastToolResult = toolResult
				return true
			}
			toolError = false
			toolResult = ""
		}
		return false
	}, func() string {
		out, _ := os.ReadFile(p.output)
		err, _ := os.ReadFile(p.stderr)
		return fmt.Sprintf("stdout %s stderr %s", logTail(out), logTail(err))
	})
}

func printableFixtureResult(body json.RawMessage) string {
	var text string
	if json.Unmarshal(body, &text) == nil {
		return text
	}
	return string(body)
}

func (p *terminal) ask(t *testing.T, model *fixtureModel, step string) []string {
	t.Helper()
	marker := "A77:" + p.name + ":" + step
	if p.interactive == nil {
		p.send(t, marker)
		p.awaitResult(t)
	} else {
		p.interactive.SendKeys(p.target, marker, "Enter")
		p.interactive.WaitFor(p.target, "fixture-ok "+marker, 6*time.Second)
	}
	model.mu.Lock()
	defer model.mu.Unlock()
	tools, ok := model.catalogs[marker]
	if !ok {
		t.Fatalf("%s had no matching request at the loopback model", marker)
	}
	return tools
}

func (p *terminal) awaitTools(t *testing.T, model *fixtureModel, step string, want bool) []string {
	t.Helper()
	var tools []string
	for i, deadline := 0, time.Now().Add(4*time.Second); time.Now().Before(deadline); i++ {
		tools = p.ask(t, model, step+"-"+strconv.Itoa(i))
		if slices.Contains(tools, baseTool) && slices.Contains(tools, masterTool) == want {
			t.Logf("%s catalog converged at %s probe %d", p.name, step, i+1)
			return tools
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("native master presence did not reach %t at %s; native init=%v, model=%v",
		want, step, p.lastInitTools, tools)
	return nil
}

func (p *terminal) awaitWithdrawal(t *testing.T, model *fixtureModel, step string) {
	t.Helper()
	for i, deadline := 0, time.Now().Add(4*time.Second); time.Now().Before(deadline); i++ {
		tools := p.ask(t, model, step+"-"+strconv.Itoa(i))
		if slices.Contains(p.lastInitTools, baseTool) && !slices.Contains(p.lastInitTools, masterTool) {
			t.Logf("%s current native catalog withdrawn at %s probe %d, model retained declaration=%t",
				p.name, step, i+1, slices.Contains(tools, masterTool))
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("native current catalog did not withdraw at %s: %v", step, p.lastInitTools)
}

func requireTools(t *testing.T, tools []string, master bool) {
	t.Helper()
	if !slices.Contains(tools, baseTool) || slices.Contains(tools, masterTool) != master {
		t.Fatalf("model-visible catalog %v, want baseline and master=%t", tools, master)
	}
}

func requireSameCatalog(t *testing.T, want, got []string) {
	t.Helper()
	if !slices.Equal(want, got) {
		t.Fatalf("unrelated session catalog changed from %v to %v", want, got)
	}
}

func logTail(data []byte) string {
	const limit = 2000
	if len(data) > limit {
		return "..." + string(data[len(data)-limit:])
	}
	return string(data)
}

func eventually(t *testing.T, label string, condition func() bool, diagnostic func() string) {
	t.Helper()
	for deadline := time.Now().Add(12 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if condition() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s: %s", label, diagnostic())
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

type fixtureModel struct {
	port     int
	mu       sync.Mutex
	catalogs map[string][]string
	planned  map[string]fixtureTool
}

type fixtureTool struct {
	name      string
	arguments map[string]any
	entered   chan struct{}
	proceed   <-chan struct{}
}

func (m *fixtureModel) plan(marker string, tool fixtureTool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.planned[marker] = tool
}

func startModel(t *testing.T) *fixtureModel {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	must(t, err)
	model := &fixtureModel{port: listener.Addr().(*net.TCPAddr).Port,
		catalogs: map[string][]string{}, planned: map[string]fixtureTool{}}
	server := &http.Server{Handler: http.HandlerFunc(model.serve), ReadHeaderTimeout: 3 * time.Second}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
	})
	return model
}

func (m *fixtureModel) serve(w http.ResponseWriter, r *http.Request) {
	if (r.Header.Get("x-api-key") != dummyKey && r.Header.Get("Authorization") != "Bearer "+dummyKey) ||
		r.Method != http.MethodPost {
		http.Error(w, "synthetic fixture requests only", http.StatusForbidden)
		return
	}
	if r.URL.Path == "/v1/messages/count_tokens" {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"input_tokens":5}`)
		return
	}
	if r.URL.Path != "/v1/messages" {
		http.NotFound(w, r)
		return
	}
	var req struct {
		Model    string
		Stream   bool
		Tools    []struct{ Name string }
		Messages []struct{ Content json.RawMessage }
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2<<20)).Decode(&req); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	var marker string
	for i := len(req.Messages) - 1; i >= 0 && marker == ""; i-- {
		marker = fixtureMarker(req.Messages[i].Content)
	}
	var chosen fixtureTool
	var first bool
	if strings.HasPrefix(marker, "A77:") {
		tools := make([]string, 0, len(req.Tools))
		for _, tool := range req.Tools {
			tools = append(tools, tool.Name)
		}
		m.mu.Lock()
		if _, seen := m.catalogs[marker]; !seen {
			m.catalogs[marker] = tools
			chosen = m.planned[marker]
			first = true
		}
		m.mu.Unlock()
	}
	answer := "fixture-ok " + marker
	block := map[string]any{"type": "text", "text": answer}
	stop := "end_turn"
	if first && chosen.name == "" && strings.HasSuffix(marker, "-call") {
		chosen = fixtureTool{name: masterTool, arguments: map[string]any{}}
	}
	if chosen.name != "" {
		if chosen.entered != nil {
			close(chosen.entered)
			select {
			case <-chosen.proceed:
			case <-r.Context().Done():
				return
			}
		}
		id := "toolu_" + strings.NewReplacer(":", "_", "-", "_").Replace(marker)
		block = map[string]any{"type": "tool_use", "id": id, "name": chosen.name, "input": chosen.arguments}
		stop = "tool_use"
	}
	message := map[string]any{
		"id": "msg_a77_probe", "type": "message", "role": "assistant", "model": req.Model,
		"content": []any{block}, "stop_reason": stop, "stop_sequence": nil,
		"usage": map[string]int{"input_tokens": 5, "output_tokens": 2},
	}
	if !req.Stream {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(message)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	message["content"], message["stop_reason"] = []any{}, nil
	send := func(kind string, payload map[string]any) {
		payload["type"] = kind
		raw, _ := json.Marshal(payload)
		_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", kind, raw)
	}
	send("message_start", map[string]any{"message": message})
	if stop == "tool_use" {
		input, _ := json.Marshal(block["input"])
		block["input"] = map[string]any{}
		send("content_block_start", map[string]any{"index": 0, "content_block": block})
		send("content_block_delta", map[string]any{"index": 0, "delta": map[string]any{
			"type": "input_json_delta", "partial_json": string(input),
		}})
	} else {
		send("content_block_start", map[string]any{"index": 0, "content_block": map[string]any{"type": "text", "text": ""}})
		send("content_block_delta", map[string]any{"index": 0, "delta": map[string]any{"type": "text_delta", "text": answer}})
	}
	send("content_block_stop", map[string]any{"index": 0})
	send("message_delta", map[string]any{"delta": map[string]any{"stop_reason": stop, "stop_sequence": nil}, "usage": map[string]int{"output_tokens": 2}})
	send("message_stop", map[string]any{})
}

func fixtureMarker(content json.RawMessage) string {
	var text string
	if json.Unmarshal(content, &text) == nil && strings.HasPrefix(text, "A77:") {
		return text
	}
	var blocks []struct{ Type, Text string }
	if json.Unmarshal(content, &blocks) == nil {
		for i := len(blocks) - 1; i >= 0; i-- {
			block := blocks[i]
			if block.Type == "text" && strings.HasPrefix(block.Text, "A77:") {
				return block.Text
			}
		}
	}
	return ""
}
