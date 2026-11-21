//go:build a77_native && linux

package fixtures

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/auth/session"
	"github.com/patrick-1984/ai-minion/internal/hub/hubid/hubidtest"
	"github.com/patrick-1984/ai-minion/internal/hub/server"
	hstore "github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/hub/workers"
	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/testutil/tmuxtest"
	"github.com/patrick-1984/ai-minion/internal/worker/agent"
	"github.com/patrick-1984/ai-minion/internal/worker/localapi"
	wstore "github.com/patrick-1984/ai-minion/internal/worker/store"
)

const productionToolPrefix = "mcp__minion_master__"

type productionHub struct {
	store   *hstore.Store
	service *workers.Service
	session *session.Manager
	http    *http.Client
	url     string
	admin   session.Issued
	owner   string
}

func startProductionHub(t *testing.T, root string) *productionHub {
	t.Helper()
	t.Setenv("MINION_ADMIN_USERNAME", "root")
	t.Setenv("MINION_ADMIN_PASSWORD", "a77-local-admin-fixture")
	dir := filepath.Join(root, "hub")
	must(t, os.Mkdir(dir, 0o700))
	hubidtest.Provision(t, dir)
	out := &productionHub{}
	reg := server.DefaultRegistry.Clone()
	must(t, reg.Register("test-capture", 101, func(_ context.Context, e *server.Env) (err error) {
		if out.store, err = server.Require[*hstore.Store](e); err != nil {
			return err
		}
		if out.service, err = server.Require[*workers.Service](e); err != nil {
			return err
		}
		out.session, err = server.Require[*session.Manager](e)
		return err
	}))
	log := slog.New(slog.DiscardHandler)
	h := server.New(&server.Config{DataDir: dir, Listen: "127.0.0.1:0", Logger: log}, reg)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.Run(ctx) }()
	select {
	case <-h.Started():
	case err := <-done:
		cancel()
		t.Fatalf("fixture Hub startup: %v", err)
	case <-time.After(15 * time.Second):
		cancel()
		t.Fatal("fixture Hub startup timed out")
	}
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("fixture Hub shutdown: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Error("fixture Hub did not stop")
		}
	})
	out.url = "https://" + h.Addr()
	out.http = &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
		DisableKeepAlives: true, TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // this test starts the private local Hub
	}}
	user, err := out.store.UserByIdentifier(ctx, "root")
	must(t, err)
	issued, err := out.session.Create(ctx, user, time.Hour)
	must(t, err)
	out.owner, out.admin = user.ID, issued
	return out
}

func (h *productionHub) request(t *testing.T, op string, args, result any) (int, string) {
	t.Helper()
	body, err := json.Marshal(args)
	must(t, err)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, h.url+"/api/v1/ops/"+op, bytes.NewReader(body))
	must(t, err)
	req.AddCookie(&http.Cookie{Name: session.CookieName, Value: h.admin.Token})
	req.Header.Set(session.CSRFHeader, h.admin.CSRFToken)
	req.Header.Set("Content-Type", "application/json")
	res, err := h.http.Do(req)
	must(t, err)
	defer res.Body.Close()
	body, err = io.ReadAll(io.LimitReader(res.Body, 1<<20))
	must(t, err)
	if res.StatusCode == http.StatusOK && result != nil {
		must(t, json.Unmarshal(body, result))
	}
	return res.StatusCode, string(body)
}

func (h *productionHub) op(t *testing.T, op string, args, result any) {
	t.Helper()
	code, body := h.request(t, op, args, result)
	if code != http.StatusOK {
		t.Fatalf("%s returned %d: %s", op, code, logTail([]byte(body)))
	}
}

func (h *productionHub) control(t *testing.T, id string) ops.ControlWorker {
	t.Helper()
	var state ops.ControlListResult
	h.op(t, ops.OpControlList, ops.ControlListArgs{}, &state)
	for _, w := range state.Workers {
		if w.WorkerID == id {
			return w
		}
	}
	t.Fatalf("Worker %s missing from Minion control", id)
	return ops.ControlWorker{}
}

func (h *productionHub) setMaster(t *testing.T, id string, c ops.ControlMasterCandidate, master bool) {
	t.Helper()
	h.op(t, ops.OpControlSetBoundMaster, ops.ControlSetBoundMasterArgs{
		WriteArgs: ops.WriteArgs{RequestID: ops.NewUUIDv7()}, WorkerID: id, SessionName: c.SessionName,
		IncarnationID: c.IncarnationID, Master: master, IfGeneration: &c.Generation,
	}, nil)
}

type productionWorker struct {
	id, state, runtime, name string
	tmux                     *tmuxtest.Server
}

func seedProductionWorker(t *testing.T, h *productionHub, root, name string) productionWorker {
	t.Helper()
	w := productionWorker{id: hstore.NewID(), state: filepath.Join(root, name+"-state"),
		runtime: filepath.Join(root, name+"-runtime"), name: name, tmux: tmuxtest.New(t)}
	must(t, os.Mkdir(w.state, 0o700))
	must(t, os.Mkdir(w.runtime, 0o700))
	st, err := wstore.Open(w.state)
	must(t, err)
	enrollment := agent.NewEnrollments(st, w.state)
	key, err := enrollment.LoadOrCreateKey()
	must(t, err)
	must(t, enrollment.Commit(t.Context(), agent.Enrollment{Pin: identity.Pin{Pub: h.service.Identity().Public()}}))
	must(t, st.Close())
	must(t, h.store.InsertWorker(t.Context(), nil, hstore.WorkerRow{ID: w.id, Pub: key.Public(),
		Name: name, OwnerID: h.owner, Kind: hstore.WorkerKindPrivate, CreatedAt: time.Now().UnixMilli()}))
	return w
}

func (w productionWorker) start(t *testing.T, h *productionHub, bin, share string) func() {
	t.Helper()
	cmd := exec.Command(bin, "worker", "up", "--foreground", "--server", h.url,
		"--accept-usage", "--share-existing", share, "--state-dir", w.state, "--tmux-socket", w.tmux.Socket(),
		"--name", w.name)
	cmd.Dir = w.state
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "XDG_RUNTIME_DIR=" + w.runtime,
		"XDG_CONFIG_HOME=" + filepath.Join(w.state, "config"), "XDG_STATE_HOME=" + w.state,
		"XDG_CACHE_HOME=" + filepath.Join(w.state, "cache")}
	logfile, err := os.CreateTemp(w.state, "worker-*.log")
	must(t, err)
	cmd.Stdout, cmd.Stderr = logfile, logfile
	must(t, cmd.Start())
	must(t, logfile.Close())
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	stop := func() {
		if cmd.Process == nil {
			return
		}
		_ = cmd.Process.Signal(os.Interrupt)
		select {
		case <-done:
		case <-time.After(8 * time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
		cmd.Process = nil
	}
	t.Cleanup(stop)
	eventually(t, w.name+" authenticated production Worker", func() bool {
		_, err := os.Stat(filepath.Join(w.runtime, "minion", "worker.sock"))
		return err == nil && h.service.Registry().IsOnline(w.id)
	}, func() string {
		data, _ := os.ReadFile(logfile.Name())
		return logTail(data)
	})
	return stop
}

func productionMinion(t *testing.T, root, claude string) string {
	t.Helper()
	dir := filepath.Join(root, "bin")
	must(t, os.Mkdir(dir, 0o700))
	must(t, os.Symlink(claude, filepath.Join(dir, "claude")))
	bin := filepath.Join(dir, "minion")
	cmd := exec.CommandContext(t.Context(), "go", "build", "-buildvcs=false", "-o", bin, "./cmd/minion")
	cmd.Dir = "../../.."
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("build fixture Minion: %v\n%s", err, out)
	}
	return bin
}

func TestClaudeProductionMaster(t *testing.T) {
	root, err := os.MkdirTemp("", "a77-prod-")
	must(t, err)
	t.Cleanup(func() { must(t, os.RemoveAll(root)) })
	claude, err := filepath.EvalSymlinks(findClaude(t))
	must(t, err)
	self, err := os.Executable()
	must(t, err)
	bin := productionMinion(t, root, claude)
	hub := startProductionHub(t, root)
	source := seedProductionWorker(t, hub, root, "source")
	dest := seedProductionWorker(t, hub, root, "destination")
	_, err = source.tmux.Tmux("set-option", "-wg", "remain-on-exit", "on")
	must(t, err)
	destScript := filepath.Join(root, "claude")
	taskFile := filepath.Join(root, "received-prompts")
	must(t, os.WriteFile(destScript, []byte("#!/bin/sh\nstty -echo\nprintf 'Fixture target ready.\\n❯ '\n"+
		"while IFS= read -r text; do printf '%s\\n' \"$text\" >> \"$1\"; printf 'Fixture prompt received.\\n❯ '; done\n"), 0o700))
	dest.tmux.NewSession("target", destScript, taskFile)
	dest.tmux.NewSession("private", "/usr/bin/sleep", "120")
	dest.tmux.WaitFor("=target:", "Fixture target ready.", 5*time.Second)
	stopSource := source.start(t, hub, bin, "all")
	dest.start(t, hub, bin, "all")

	model := startModel(t)
	profile, project := filepath.Join(root, "profile"), filepath.Join(root, "project")
	must(t, os.Mkdir(profile, 0o700))
	must(t, os.Mkdir(project, 0o700))
	settings := `{"spinnerTipsEnabled":false}`
	must(t, os.WriteFile(filepath.Join(profile, "settings.json"), []byte(settings), 0o600))
	baseline, err := json.Marshal(map[string]any{
		"mcpServers": map[string]any{"baseline_fixture": map[string]any{
			"type": "stdio", "command": self, "args": []string{"-test.run=^TestFixtureChild$", "--", "baseline"},
		}},
	})
	must(t, err)
	must(t, os.WriteFile(filepath.Join(profile, ".claude.json"), baseline, 0o600))
	launch := func(name string, prepared bool) *terminal {
		cfg := childLaunch{Name: name, Root: root, Claude: claude, Profile: profile,
			Project: project, ModelPort: model.port, WorkerRuntime: source.runtime}
		if prepared {
			cfg.Minion = bin
		}
		return launchTerminal(t, source.tmux, self, cfg)
	}
	master, standby, sibling := launch("master", true), launch("standby", true), launch("sibling", false)
	initial := make(map[string][]string)
	for _, terminal := range []*terminal{master, standby, sibling} {
		initial[terminal.name] = terminal.ask(t, model, "production-idle")
		if !slices.Contains(initial[terminal.name], baseTool) || hasProductionTool(initial[terminal.name]) {
			t.Fatalf("%s idle catalog = %v", terminal.name, initial[terminal.name])
		}
	}
	var candidates ops.ControlWorker
	eventually(t, "two production candidates", func() bool {
		candidates = hub.control(t, source.id)
		return len(candidates.MasterCandidates) == 2
	}, func() string { return fmt.Sprintf("%+v", candidates) })
	for _, c := range candidates.MasterCandidates {
		if c.SessionName == sibling.name || c.Master {
			t.Fatalf("unexpected idle candidate %+v", c)
		}
	}
	candidate := productionCandidate(t, candidates, master.name)
	hub.setMaster(t, source.id, candidate, true)
	active := productionCandidate(t, hub.control(t, source.id), master.name)
	workersTool := productionTool(ops.OpWorkersList, active.Generation)
	awaitProductionTool(t, master, model, "production-nominated", workersTool, true)
	checkSiblings := func(step string) {
		for _, p := range []*terminal{standby, sibling} {
			requireSameCatalog(t, initial[p.name], p.ask(t, model, step))
		}
	}
	checkSiblings("production-nominated")
	t.Log("real Hub nomination exposed tools only to the selected production Claude invocation")

	workers := invokeProduction(t, master, model, "production-no-targets", workersTool, map[string]any{}, false)
	if !strings.Contains(workers, `"workers":[]`) {
		t.Fatalf("expected no workers from zero consent: %s", workers)
	}
	sessionsTool := productionTool(ops.OpSessionsList, active.Generation)
	denial := invokeProduction(t, master, model, "production-local-shortcut", sessionsTool, map[string]any{"worker_id": source.id}, true)
	if !strings.Contains(denial, "forbidden") && !strings.Contains(denial, "not_found") {
		t.Fatalf("source shortcut failed outside authorization: %s", denial)
	}
	// The socket accepts no caller pane fields, and the account's known
	// generation alone gives a same-user sibling no role.
	_, err = localapi.CallMaster(t.Context(), filepath.Join(source.runtime, "minion", "master.sock"),
		localapi.MasterRequest{Kind: localapi.MasterCall, Generation: active.Generation, RequestID: ops.NewUUIDv7(),
			Operation: ops.OpWorkersList, Arguments: json.RawMessage(`{}`)})
	if proto.CodeOf(err) != proto.CodeForbidden {
		t.Fatalf("copied generation claimed master authority: %v", err)
	}
	legacy := ops.ControlSetMasterArgs{WriteArgs: ops.WriteArgs{RequestID: ops.NewUUIDv7()}, WorkerID: source.id,
		SessionName: master.name, Master: true}
	if code, _ := hub.request(t, ops.OpControlSetMaster, legacy, nil); code != http.StatusConflict {
		t.Fatalf("name-only mutation of bound nomination returned %d, want 409", code)
	}
	hub.op(t, ops.OpControlSetAccept, ops.ControlSetAcceptArgs{
		WriteArgs: ops.WriteArgs{RequestID: ops.NewUUIDv7()}, WorkerID: dest.id, SourceWorkerID: source.id, Accept: true,
	}, nil)
	hub.op(t, ops.OpShareSet, ops.ShareSetArgs{
		WorkerID: dest.id, Mode: ops.ShareModeSelect, Names: []string{"target"}}, nil)
	workers = invokeProduction(t, master, model, "production-list-workers", workersTool, map[string]any{}, false)
	if !strings.Contains(workers, dest.id) || strings.Contains(workers, source.id) {
		t.Fatalf("scoped Worker discovery escaped consent: %s", workers)
	}
	workerInput := map[string]any{"worker_id": dest.id}
	list := invokeProduction(t, master, model, "production-list-sessions", sessionsTool, workerInput, false)
	if !strings.Contains(list, `"target"`) || strings.Contains(list, `"private"`) {
		t.Fatalf("scoped session discovery did not respect sharing: %s", list)
	}
	target := map[string]string{"worker_id": dest.id, "session_name": "target"}
	snapshotTool := productionTool(ops.OpSessionSnapshot, active.Generation)
	invokeProduction(t, master, model, "production-hidden-target", snapshotTool,
		map[string]any{"target": map[string]string{"worker_id": dest.id, "session_name": "private"}}, true)
	snapshot := invokeProduction(t, master, model, "production-inspect", snapshotTool,
		map[string]any{"target": target}, false)
	if !strings.Contains(snapshot, "Fixture target ready.") {
		t.Fatalf("real snapshot omitted the fixture console: %s", snapshot)
	}
	const prompt = "Please report the fixture status."
	promptTool := productionTool(ops.OpSendText, active.Generation)
	promptArgs := map[string]any{"target": target, "text": prompt, "request_id": ops.NewUUIDv7()}
	for _, step := range []string{"production-delivery", "production-duplicate"} {
		result := invokeProduction(t, master, model, step, promptTool, promptArgs, false)
		if !strings.Contains(result, `"delivered"`) {
			t.Fatalf("guarded prompt did not deliver: %s", result)
		}
	}
	assertDeliveredOnce(t, taskFile, prompt)
	t.Log("actual Workers listed, inspected and delivered one prompt through native MCP, including duplicate request ID")

	// Hold a loopback model turn that already received the old native schema.
	// Complete the Hub revoke before that turn attempts to invoke the tool.
	started, release := make(chan struct{}), make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	step := "production-inflight-stale"
	staleArgs := map[string]any{"target": target, "text": "This revoked prompt must never arrive.", "request_id": ops.NewUUIDv7()}
	model.plan(productionMarker(master, step), fixtureTool{name: promptTool, arguments: staleArgs, entered: started, proceed: release})
	master.send(t, productionMarker(master, step))
	select {
	case <-started:
	case <-time.After(6 * time.Second):
		t.Fatal("could not hold a stale model turn")
	}
	hub.setMaster(t, source.id, active, false)
	close(release)
	master.awaitResult(t)
	if !master.lastToolError {
		t.Fatalf("in-flight native call after real revoke had no tool error: %s", master.lastToolResult)
	}
	awaitProductionTool(t, master, model, "production-revoked", workersTool, false)
	checkSiblings("production-revoked")
	assertDeliveredOnce(t, taskFile, prompt)
	revoked := productionCandidate(t, hub.control(t, source.id), master.name)
	hub.setMaster(t, source.id, revoked, true)
	renominated := productionCandidate(t, hub.control(t, source.id), master.name)
	newWorkersTool := productionTool(ops.OpWorkersList, renominated.Generation)
	awaitProductionTool(t, master, model, "production-renominated", newWorkersTool, true)
	invokeProduction(t, master, model, "production-old-generation", promptTool, staleArgs, true)
	assertDeliveredOnce(t, taskFile, prompt)
	t.Log("real revoke and re-nomination reject cached native calls from the older generation")

	beforeLink, ok := hub.service.Registry().Get(source.id)
	if !ok || !hub.service.Registry().Disconnect(source.id) {
		t.Fatal("fixture source had no active Hub link")
	}
	eventually(t, "source Worker authenticates a replacement link", func() bool {
		link, ok := hub.service.Registry().Get(source.id)
		return ok && !bytes.Equal(link.TranscriptHash, beforeLink.TranscriptHash)
	}, func() string { return "the original authenticated link is gone" })
	eventually(t, "same native incarnation regains its durable nomination", func() bool {
		w := hub.control(t, source.id)
		return slices.ContainsFunc(w.MasterCandidates, func(c ops.ControlMasterCandidate) bool {
			// Ordinary sharing and the master lane are independent streams.
			// The reconnect can require the next report to issue its lease.
			return c.IncarnationID == renominated.IncarnationID && c.Generation == renominated.Generation &&
				c.Master && c.Attachment == "active"
		})
	}, func() string { return fmt.Sprintf("%+v", hub.control(t, source.id)) })
	awaitProductionTool(t, master, model, "production-reconnected", newWorkersTool, true)
	eventually(t, "new Hub lease is usable through the same native session", func() bool {
		step := "production-reconnect-read-" + ops.NewUUIDv7()
		model.plan(productionMarker(master, step), fixtureTool{name: newWorkersTool, arguments: map[string]any{}})
		master.ask(t, model, step)
		return !master.lastToolError && strings.Contains(master.lastToolResult, dest.id)
	}, func() string { return master.lastToolResult })

	hub.op(t, ops.OpShareSet, ops.ShareSetArgs{WorkerID: source.id, Mode: ops.ShareModeNone}, nil)
	awaitProductionTool(t, master, model, "production-unshared", newWorkersTool, false)
	eventually(t, "source withholding removes both prepared candidates", func() bool {
		return len(hub.control(t, source.id).MasterCandidates) == 0
	}, func() string { return fmt.Sprintf("%+v", hub.control(t, source.id)) })
	hub.op(t, ops.OpShareSet, ops.ShareSetArgs{WorkerID: source.id, Mode: ops.ShareModeAll}, nil)
	awaitProductionTool(t, master, model, "production-reshared", newWorkersTool, false)
	invokeProduction(t, master, model, "production-reshared-replay", promptTool, staleArgs, true)
	afterShare := launch("after-share", true)
	afterShare.ask(t, model, "production-restart-idle")
	eventually(t, "fresh candidate after share restoration", func() bool {
		return len(hub.control(t, source.id).MasterCandidates) == 1
	}, func() string { return fmt.Sprintf("%+v", hub.control(t, source.id)) })
	fresh := productionCandidate(t, hub.control(t, source.id), afterShare.name)
	hub.setMaster(t, source.id, fresh, true)
	fresh = productionCandidate(t, hub.control(t, source.id), afterShare.name)
	restartTool := productionTool(ops.OpWorkersList, fresh.Generation)
	awaitProductionTool(t, afterShare, model, "production-before-worker-restart", restartTool, true)
	stopSource()
	awaitProductionTool(t, afterShare, model, "production-worker-down", restartTool, false)
	source.start(t, hub, bin, "all")
	awaitProductionTool(t, afterShare, model, "production-worker-back", restartTool, false)
	if candidates := hub.control(t, source.id).MasterCandidates; len(candidates) != 0 {
		t.Fatalf("old preparations survived a new Worker process: %+v", candidates)
	}
	invokeProduction(t, afterShare, model, "production-worker-stale", restartTool, map[string]any{}, true)
	t.Log("real link reconnect re-proves the surviving session; share loss and Worker replacement retire preparations")
	var ordinary ops.WorkersListResult
	hub.op(t, ops.OpWorkersList, ops.WorkersListArgs{}, &ordinary)
	if len(ordinary.Workers) != 2 {
		t.Fatalf("ordinary admin discovery lost Workers: %v", ordinary.Workers)
	}
	got, err := os.ReadFile(filepath.Join(profile, "settings.json"))
	must(t, err)
	if string(got) != settings {
		t.Fatal("production launcher changed shared fixture settings")
	}
	assertDeliveredOnce(t, taskFile, prompt)
}

func productionCandidate(t *testing.T, w ops.ControlWorker, name string) ops.ControlMasterCandidate {
	t.Helper()
	for _, c := range w.MasterCandidates {
		if c.SessionName == name {
			return c
		}
	}
	t.Fatalf("missing candidate %s: %+v", name, w.MasterCandidates)
	return ops.ControlMasterCandidate{}
}

func productionTool(op string, generation uint64) string {
	return productionToolPrefix + op + "_g" + strconv.FormatUint(generation, 10)
}

func hasProductionTool(tools []string) bool {
	return slices.ContainsFunc(tools, func(tool string) bool { return strings.HasPrefix(tool, productionToolPrefix) })
}

func productionMarker(terminal *terminal, step string) string {
	return "A77:" + terminal.name + ":" + step
}

func invokeProduction(t *testing.T, p *terminal, model *fixtureModel, step, tool string, args map[string]any, wantError bool) string {
	t.Helper()
	model.plan(productionMarker(p, step), fixtureTool{name: tool, arguments: args})
	p.ask(t, model, step)
	if p.lastToolResult == "" || p.lastToolError != wantError {
		t.Fatalf("%s tool error=%t, want %t, result %q", step, p.lastToolError, wantError, p.lastToolResult)
	}
	return p.lastToolResult
}

func awaitProductionTool(t *testing.T, p *terminal, model *fixtureModel, step, tool string, want bool) {
	t.Helper()
	for i, deadline := 0, time.Now().Add(6*time.Second); time.Now().Before(deadline); i++ {
		tools := p.ask(t, model, step+"-"+strconv.Itoa(i))
		if slices.Contains(p.lastInitTools, tool) == want && (want && slices.Contains(tools, tool) || !want && !hasProductionTool(p.lastInitTools)) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("%s native catalog %v, expecting %q present=%t", step, p.lastInitTools, tool, want)
}

func assertDeliveredOnce(t *testing.T, path, prompt string) {
	t.Helper()
	eventually(t, "exactly one fixture prompt", func() bool {
		body, _ := os.ReadFile(path)
		return string(body) == prompt+"\n"
	}, func() string {
		body, _ := os.ReadFile(path)
		return string(body)
	})
}
