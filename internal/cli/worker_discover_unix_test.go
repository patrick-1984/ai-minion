//go:build unix

package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/patrick-1984/ai-minion/internal/worker/agent"
	"github.com/patrick-1984/ai-minion/internal/worker/presence"
)

var discoverNow = time.UnixMilli(1_795_000_000_000)

type discoverEnv struct {
	t       *testing.T
	state   string // default state dir (login environment)
	private string // default private presence dir
	index   string
	public  string
}

func newDiscoverEnv(t *testing.T) *discoverEnv {
	t.Helper()
	root := t.TempDir()
	e := &discoverEnv{t: t, state: filepath.Join(root, "state")}
	e.private = filepath.Join(e.state, "presence")
	e.index = filepath.Join(e.state, "presence-index")
	e.public = filepath.Join(root, "pub")
	// Modes are set explicitly: the fixtures must not depend on the umask.
	for _, d := range []string{e.state, e.private, e.index} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(e.public, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(e.public, 0o755); err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *discoverEnv) rec(id, scope, state string, updatedAgo time.Duration, mod func(map[string]any)) []byte {
	m := map[string]any{
		"schema": presence.Schema, "instance": id, "daemon_version": "v1.2.3",
		"started_at": discoverNow.Add(-time.Hour).UnixMilli(), "updated_at": discoverNow.Add(-updatedAgo).UnixMilli(),
		"stale_after_s": 90, "state": state, "os_scope": scope, "hub_origin": "https://hub.example",
	}
	if mod != nil {
		mod(m)
	}
	b, _ := json.Marshal(m)
	return b
}

func (e *discoverEnv) write(dir, name string, mode os.FileMode, b []byte) {
	e.t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, b, mode); err != nil {
		e.t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil { // not subject to the umask
		e.t.Fatal(err)
	}
}

// forUmasks runs fn under the usual umask and a restrictive one.
func forUmasks(t *testing.T, fn func(t *testing.T)) {
	for _, um := range []int{0o022, 0o077} {
		t.Run(fmt.Sprintf("umask-%04o", um), func(t *testing.T) {
			old := syscall.Umask(um)
			defer syscall.Umask(old)
			fn(t)
		})
	}
}

func (e *discoverEnv) deps(live agent.LiveSource) workerDiscoverDeps {
	return workerDiscoverDeps{
		lookupEnv: mapEnv(map[string]string{}),
		reader: func(stateDir string) presence.Reader {
			r := presence.Reader{
				UID: os.Geteuid(), RootUID: os.Geteuid(), PublicRoot: e.public,
				PrivateDirs:   []string{e.private},
				DiscoveryDirs: []string{e.index},
				Now:           func() time.Time { return discoverNow },
			}
			if stateDir != "" && stateDir != e.state {
				r.PrivateDirs = append(r.PrivateDirs, filepath.Join(stateDir, "presence"))
			}
			return r
		},
		live: func(string) agent.LiveSource { return live },
	}
}

func (e *discoverEnv) run(live agent.LiveSource, args ...string) (discoverOutput, string, int) {
	e.t.Helper()
	root := &cobra.Command{Use: "minion", SilenceUsage: true, SilenceErrors: true}
	workerCommand(root).AddCommand(newWorkerDiscoverCmd(e.deps(live)))
	var out strings.Builder
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(append([]string{"worker", "discover"}, args...))
	err := root.Execute()
	code := 0
	var ec *exitCodeError
	if errors.As(err, &ec) {
		code = ec.code
	} else if err != nil {
		e.t.Fatalf("unexpected error: %v", err)
	}
	var doc discoverOutput
	if len(args) > 0 && args[0] == "--json" || strings.Contains(strings.Join(args, " "), "--json") {
		if err := json.Unmarshal([]byte(out.String()), &doc); err != nil {
			e.t.Fatalf("bad json %q: %v", out.String(), err)
		}
	}
	return doc, out.String(), code
}

func compact(b []byte) string {
	var buf bytes.Buffer
	if err := json.Compact(&buf, b); err != nil {
		return string(b)
	}
	return buf.String()
}

var noWorker = fakeLive{err: agent.ErrNoLiveWorker}

func TestDiscoverFreshLoginFindsRelocatedService(t *testing.T) {
	forUmasks(t, func(t *testing.T) {
		e := newDiscoverEnv(t)
		// The service keeps its state elsewhere; the login shell knows only the
		// default location, where the owner-only pointer lives.
		reloc := filepath.Join(t.TempDir(), "svc-state", "presence")
		if err := os.MkdirAll(reloc, 0o700); err != nil {
			t.Fatal(err)
		}
		e.write(reloc, "svc1.json", 0o600, e.rec("svc1", "user", "running", 5*time.Second, nil))
		ptr, _ := json.Marshal(map[string]string{"schema": presence.IndexSchema, "dir": reloc})
		e.write(e.index, "k.json", 0o600, ptr)
		usage := `{"schema":"minion.usage-meters/1","generated_at":1,"slots":[]}`
		e.write(reloc, "svc1.usage.json", 0o600, []byte(usage))

		doc, _, code := e.run(noWorker, "--json", "--usage")
		if code != 0 || len(doc.Workers) != 1 {
			t.Fatalf("code %d, %+v", code, doc)
		}
		w := doc.Workers[0]
		if w.Instance != "svc1" || w.Status != presence.StatusRunning || w.OSScope != "user" || w.DaemonVersion != "v1.2.3" ||
			w.HubOrigin != "https://hub.example" || w.UpdatedAgeS == nil || *w.UpdatedAgeS != 5 || compact(w.Usage) != usage {
			t.Fatalf("%+v", w)
		}
		// Without --usage the document is left out.
		doc, _, _ = e.run(noWorker, "--json")
		if len(doc.Workers[0].Usage) != 0 {
			t.Fatal("usage printed without --usage")
		}
	})
}

func TestDiscoverStalePrivateAndLiveRoot(t *testing.T) {
	forUmasks(t, func(t *testing.T) {
		e := newDiscoverEnv(t)
		e.write(e.private, "old.json", 0o600, e.rec("old", "user", "running", 10*time.Minute, nil))
		e.write(e.public, "root1.json", 0o644, e.rec("root1", "root", "running", 3*time.Second, nil))
		doc, text, code := e.run(noWorker, "--json")
		if code != 0 || len(doc.Workers) != 2 {
			t.Fatalf("code %d %+v", code, doc)
		}
		if doc.Workers[0].Instance != "root1" || doc.Workers[0].Status != presence.StatusRunning ||
			doc.Workers[1].Instance != "old" || doc.Workers[1].Status != presence.StatusStale {
			t.Fatalf("%s", text)
		}
		_, out, _ := e.run(noWorker)
		if !strings.Contains(out, "running") || !strings.Contains(out, "stale") {
			t.Fatalf("text: %s", out)
		}
	})
}

func TestDiscoverStoppedExitsOne(t *testing.T) {
	e := newDiscoverEnv(t)
	e.write(e.private, "a.json", 0o600, e.rec("a", "user", "stopped", time.Second, nil))
	doc, _, code := e.run(noWorker, "--json")
	if code != DiscoverNone || doc.Workers[0].Status != presence.StatusStopped {
		t.Fatalf("code %d %+v", code, doc)
	}
}

func TestDiscoverOldDaemonWithoutPresence(t *testing.T) {
	e := newDiscoverEnv(t)
	// Nothing published and nothing answers: exit 1 with a hint.
	doc, _, code := e.run(noWorker, "--json")
	if code != DiscoverNone || len(doc.Workers) != 0 || !strings.Contains(doc.Note, "older release") {
		t.Fatalf("code %d %+v", code, doc)
	}
	// An older daemon answers on its socket without daemon info.
	doc, _, code = e.run(fakeLive{}, "--json")
	if code != 0 || len(doc.Workers) != 1 || doc.Workers[0].Source != "socket" || doc.Workers[0].Status != presence.StatusRunning {
		t.Fatalf("code %d %+v", code, doc)
	}
}

func TestDiscoverMalformedOversizedAndFuture(t *testing.T) {
	forUmasks(t, func(t *testing.T) {
		e := newDiscoverEnv(t)
		e.write(e.private, "bad.json", 0o600, []byte(`{"schema": "x"`))
		e.write(e.private, "big.json", 0o600, []byte(`{"schema":"`+strings.Repeat("a", 5000)+`"}`))
		e.write(e.private, "fut.json", 0o600, e.rec("fut", "user", "running", -time.Hour, nil))
		e.write(e.private, "wide.json", 0o644, e.rec("wide", "user", "running", time.Second, nil))
		doc, _, code := e.run(noWorker, "--json")
		if code != DiscoverError || len(doc.Workers) != 4 {
			t.Fatalf("code %d %+v", code, doc)
		}
		for _, w := range doc.Workers {
			if w.Status != presence.StatusUnavailable || w.Error == "" || w.DaemonVersion != "" {
				t.Fatalf("%+v", w)
			}
		}
		// A running Worker next to a broken record still gives exit 0.
		e.write(e.public, "ok.json", 0o644, e.rec("ok", "root", "running", time.Second, nil))
		if _, _, code := e.run(noWorker, "--json"); code != 0 {
			t.Fatalf("code %d", code)
		}
	})
}

func TestDiscoverExplicitStateDirAndBadUsage(t *testing.T) {
	e := newDiscoverEnv(t)
	other := t.TempDir()
	if err := os.Mkdir(filepath.Join(other, "presence"), 0o700); err != nil {
		t.Fatal(err)
	}
	e.write(filepath.Join(other, "presence"), "x.json", 0o600, e.rec("x", "user", "running", time.Second, nil))
	e.write(filepath.Join(other, "presence"), "x.usage.json", 0o600, []byte(`{"schema":"other"}`))
	doc, _, code := e.run(noWorker, "--json", "--usage", "--state-dir", other)
	if code != 0 || len(doc.Workers) != 1 || len(doc.Workers[0].Usage) != 0 {
		t.Fatalf("code %d %+v", code, doc)
	}
}

// The tray keeps using "worker status --json": its fields stay, and usage is
// added only when a stored document exists.
func TestWorkerStatusJSONKeepsTrayFieldsAndAddsUsage(t *testing.T) {
	dir := t.TempDir()
	run := func() map[string]any {
		out, _, err := runWorkerCmds(t, testStatusDeps(map[string]string{"MINION_STATE_DIR": dir}), workerConfigDeps{}, "worker", "status", "--json")
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(out), &m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	m := run()
	for _, k := range []string{"version", "name", "mode", "hub_url", "connection", "paired", "paused", "sessions", "problems"} {
		if _, ok := m[k]; !ok && k != "problems" {
			t.Fatalf("field %q missing", k)
		}
	}
	if _, ok := m["usage"]; ok {
		t.Fatal("usage without a document")
	}
	pdir := filepath.Join(dir, "presence")
	if err := os.MkdirAll(pdir, 0o700); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(map[string]any{"schema": presence.Schema, "instance": "i1", "daemon_version": "v1",
		"started_at": time.Now().UnixMilli(), "updated_at": time.Now().UnixMilli(), "stale_after_s": 90,
		"state": "running", "os_scope": "user"})
	if err := os.WriteFile(filepath.Join(pdir, "i1.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pdir, "i1.usage.json"), []byte(`{"schema":"minion.usage-meters/1","generated_at":1,"slots":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	m = run()
	u, ok := m["usage"].(map[string]any)
	if !ok || u["generated_at"] != float64(1) {
		t.Fatalf("usage = %v", m["usage"])
	}
	if m["connection"] == nil || m["version"] != "v9.9.9" {
		t.Fatal("tray fields changed")
	}
}

// An unrelated invalid setting of the selected profile must not hide a readable
// record of a running Worker.
func TestDiscoverIgnoresUnrelatedConfigErrors(t *testing.T) {
	forUmasks(t, func(t *testing.T) {
		e := newDiscoverEnv(t)
		e.write(e.public, "root1.json", 0o644, e.rec("root1", "root", "running", 2*time.Second, nil))
		deps := e.deps(noWorker)
		deps.lookupEnv = mapEnv(map[string]string{"MINION_SHARE_LOCK": "definitely-not-valid", "MINION_NAME": "bad name \x00"})
		root := &cobra.Command{Use: "minion", SilenceUsage: true, SilenceErrors: true}
		workerCommand(root).AddCommand(newWorkerDiscoverCmd(deps))
		var out strings.Builder
		root.SetOut(&out)
		root.SetErr(&out)
		root.SetArgs([]string{"worker", "discover", "--json"})
		err := root.Execute()
		if err != nil {
			t.Fatalf("exit: %v\n%s", err, out.String())
		}
		var doc discoverOutput
		if err := json.Unmarshal([]byte(out.String()), &doc); err != nil || len(doc.Workers) != 1 || doc.Workers[0].Instance != "root1" {
			t.Fatalf("%v %s", err, out.String())
		}
	})
}

// A selector that cannot be resolved is one more entry, never an empty answer.
func TestDiscoverReportsAnUnresolvableSelectorNextToTheRecords(t *testing.T) {
	e := newDiscoverEnv(t)
	e.write(e.public, "root1.json", 0o644, e.rec("root1", "root", "running", 2*time.Second, nil))
	deps := e.deps(noWorker)
	doc, _, code := runDiscoverWith(t, deps, "--json", "--state-dir", "")
	if code != 0 || len(doc.Workers) < 1 || doc.Workers[0].Instance != "root1" {
		t.Fatalf("code %d %+v", code, doc)
	}
	var cfgEntries int
	for _, w := range doc.Workers {
		if w.Source == presence.SourceConfig {
			cfgEntries++
			if w.Status != presence.StatusUnavailable || !strings.Contains(w.Error, "state_dir") {
				t.Fatalf("%+v", w)
			}
		}
	}
	if cfgEntries != 1 {
		t.Fatalf("%d config entries: %+v", cfgEntries, doc.Workers)
	}
}

func runDiscoverWith(t *testing.T, deps workerDiscoverDeps, args ...string) (discoverOutput, string, int) {
	t.Helper()
	root := &cobra.Command{Use: "minion", SilenceUsage: true, SilenceErrors: true}
	workerCommand(root).AddCommand(newWorkerDiscoverCmd(deps))
	var out strings.Builder
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(append([]string{"worker", "discover"}, args...))
	err := root.Execute()
	code := 0
	var ec *exitCodeError
	if errors.As(err, &ec) {
		code = ec.code
	} else if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var doc discoverOutput
	_ = json.Unmarshal([]byte(out.String()), &doc)
	return doc, out.String(), code
}

// The socket of the selected profile is queried even when another Worker runs,
// and its answer is not listed twice when a record already accounts for it.
func TestDiscoverSocketFallbackDeduplicatesByInstance(t *testing.T) {
	e := newDiscoverEnv(t)
	e.write(e.public, "root1.json", 0o644, e.rec("root1", "root", "running", 2*time.Second, nil))
	// An older user Worker (no daemon info) next to a modern root Worker.
	doc, _, code := e.run(fakeLive{}, "--json")
	if code != 0 || len(doc.Workers) != 2 || doc.Workers[0].Source != presence.SourceSocket {
		t.Fatalf("legacy hidden: code %d %+v", code, doc)
	}
	// The same instance as a record: listed once.
	same := fakeLive{ls: agent.LiveStatus{Daemon: &agent.LiveDaemon{Instance: "root1"}}}
	doc, _, _ = e.run(same, "--json")
	if len(doc.Workers) != 1 || doc.Workers[0].Source != presence.SourcePublic {
		t.Fatalf("duplicate: %+v", doc)
	}
	// An instance no record accounts for is added.
	other := fakeLive{ls: agent.LiveStatus{Daemon: &agent.LiveDaemon{Instance: "svc9", Version: "v1"}}}
	doc, _, _ = e.run(other, "--json")
	if len(doc.Workers) != 2 || doc.Workers[0].Instance != "svc9" {
		t.Fatalf("unrepresented: %+v", doc)
	}
}

// The authenticated socket answering for the instance of a stale record means
// it runs (a reader may be blocking the heartbeat replacement); the record's
// metadata stays and the Worker is listed once.
func TestDiscoverLiveSocketOverridesAStaleHeartbeat(t *testing.T) {
	e := newDiscoverEnv(t)
	e.write(e.private, "svc.json", 0o600, e.rec("svc", "user", "running", 10*time.Minute, nil))
	live := fakeLive{ls: agent.LiveStatus{Daemon: &agent.LiveDaemon{Instance: "svc", Version: "v1"}}}
	doc, _, code := e.run(live, "--json")
	if code != 0 || len(doc.Workers) != 1 {
		t.Fatalf("code %d %+v", code, doc)
	}
	w := doc.Workers[0]
	if w.Status != presence.StatusRunning || w.Source != presence.SourcePrivate || w.UpdatedAgeS == nil || *w.UpdatedAgeS < 600 {
		t.Fatalf("want running with the disk metadata kept: %+v", w)
	}
}

func TestDiscoverRejectsATrailingBracketAndMissingStartedAt(t *testing.T) {
	forUmasks(t, func(t *testing.T) {
		e := newDiscoverEnv(t)
		e.write(e.private, "a.json", 0o600, append(e.rec("a", "user", "running", time.Second, nil), ']'))
		e.write(e.private, "b.json", 0o600, e.rec("b", "user", "running", time.Second, func(m map[string]any) { delete(m, "started_at") }))
		e.write(e.private, "c.json", 0o600, e.rec("c", "user", "running", time.Second, func(m map[string]any) { m["started_at"] = nil }))
		doc, _, code := e.run(noWorker, "--json")
		if code != DiscoverError || len(doc.Workers) != 3 {
			t.Fatalf("code %d %+v", code, doc)
		}
		for _, w := range doc.Workers {
			if w.Status != presence.StatusUnavailable {
				t.Fatalf("%+v", w)
			}
		}
	})
}

func TestDiscoverShowsHubLink(t *testing.T) {
	e := newDiscoverEnv(t)
	e.write(e.public, "root1.json", 0o644, e.rec("root1", "root", "running", 2*time.Second, func(m map[string]any) { m["hub_link"] = "registration_pending" }))
	doc, _, _ := e.run(noWorker, "--json")
	if len(doc.Workers) != 1 || doc.Workers[0].HubLink != "registration_pending" {
		t.Fatalf("%+v", doc)
	}
}
