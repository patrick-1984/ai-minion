package truenas

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type recRunner struct {
	calls [][]string
	stdin [][]byte
	out   string
	err   error
}

func (r *recRunner) Run(_ context.Context, stdin []byte, name string, args ...string) ([]byte, error) {
	r.calls = append(r.calls, append([]string{name}, args...))
	r.stdin = append(r.stdin, stdin)
	return []byte(r.out), r.err
}

func TestMiddlewarePayloadGoesToStdinNotArgv(t *testing.T) {
	r := &recRunner{out: `{"result": 42}`}
	m := &MidcltClient{Runner: r}
	c := Compose{"services": map[string]any{"hub": map[string]any{"image": "x", "environment": map[string]any{"TOKEN": "s3cret-value"}}}}
	id, err := m.Update(context.Background(), "ai-minion", c)
	if err != nil || id != 42 {
		t.Fatalf("%d %v", id, err)
	}
	if strings.Contains(strings.Join(r.calls[0], " "), "s3cret") {
		t.Fatalf("secret in argv: %v", r.calls[0])
	}
	if !strings.Contains(string(r.stdin[0]), "s3cret-value") || !strings.Contains(string(r.stdin[0]), "app.update") {
		t.Fatalf("stdin: %s", r.stdin[0])
	}
}

func TestMiddlewareErrorsAreSanitized(t *testing.T) {
	r := &recRunner{out: `{"error": "ValidationErrors"}`}
	m := &MidcltClient{Runner: r}
	_, err := m.Stop(context.Background(), "ai-minion")
	var me *MiddlewareError
	if !errors.As(err, &me) || me.Class != "ValidationErrors" || strings.Contains(err.Error(), "s3cret") {
		t.Fatalf("%v", err)
	}
	r = &recRunner{err: errors.New("exit 1: password=hunter2")}
	_, err = (&MidcltClient{Runner: r}).Stop(context.Background(), "ai-minion")
	if err == nil || strings.Contains(err.Error(), "hunter2") {
		t.Fatalf("raw client error leaked: %v", err)
	}
}

func TestAppConfigReadsTheDirectDocument(t *testing.T) {
	// TrueNAS 24.10 to 25.10: app.config returns the compose document itself.
	r := &recRunner{out: `{"result": {"services": {"hub": {"image": "a:b"}}}}`}
	c, err := (&MidcltClient{Runner: r}).AppConfig(context.Background(), "ai-minion")
	if err != nil {
		t.Fatal(err)
	}
	if img, _ := c.Image("hub"); img != "a:b" {
		t.Fatal(img)
	}
	r = &recRunner{out: `{"result": {"image": "x"}}`}
	if _, err := (&MidcltClient{Runner: r}).AppConfig(context.Background(), "ai-minion"); err == nil {
		t.Fatal("a non-compose answer was accepted")
	}
}

func TestAppConfigExtractsCustomCompose(t *testing.T) {
	r := &recRunner{out: `{"result": {"custom_compose_config": {"services": {"hub": {"image": "a:b"}}}}}`}
	c, err := (&MidcltClient{Runner: r}).AppConfig(context.Background(), "ai-minion")
	if err != nil {
		t.Fatal(err)
	}
	if img, _ := c.Image("hub"); img != "a:b" {
		t.Fatal(img)
	}
}

func TestZFSCommandsAreNotRecursive(t *testing.T) {
	r := &recRunner{}
	z := ZFSCLI{Runner: r}
	ctx := context.Background()
	_ = z.Snapshot(ctx, "tank/hub", "minion-pre-v1")
	_ = z.Rollback(ctx, "tank/hub", "minion-pre-v1")
	for _, c := range r.calls {
		for _, a := range c {
			if a == "-r" || a == "-R" || a == "-f" {
				t.Fatalf("recursive or forced zfs call: %v", c)
			}
		}
	}
	if err := z.Snapshot(ctx, "tank/hub", "bad name;rm"); err == nil {
		t.Fatal("bad snapshot name accepted")
	}
}

func TestDockerContainersParsing(t *testing.T) {
	calls := 0
	r := &seqRunner{outs: []string{"abc\n", "abc|sha256:" + strings.Repeat("1", 64) + "|minion-hub|true|2026-01-01T00:00:00Z|bridge|10.0.0.2 |healthy|readonly\n"}, n: &calls}
	cs, err := DockerCLI{Runner: r}.Containers(context.Background(), "ix-app")
	if err != nil || len(cs) != 1 || !cs[0].Running || cs[0].Service != "minion-hub" {
		t.Fatalf("%+v %v", cs, err)
	}
	if !cs[0].Maintenance || cs[0].IP != "10.0.0.2" {
		t.Fatalf("lost live trial state: %+v", cs[0])
	}
	calls = 0
	r.outs[1] = strings.Replace(r.outs[1], "|bridge|", "|host|", 1)
	cs, err = (DockerCLI{Runner: r}).Containers(context.Background(), "ix-app")
	if err != nil || cs[0].IP != "" {
		t.Fatalf("host networking supplied an unbound readiness address: %+v %v", cs, err)
	}
}

func TestDatasetAtUsesTheMountedFilesystem(t *testing.T) {
	path := t.TempDir()
	for _, tc := range []struct {
		name, source, fs, target string
		valid                    bool
	}{
		{"dedicated", "tank/apps/hub", "zfs", path, true},
		{"unmounted_property", "tank/apps", "zfs", filepath.Dir(path), false},
		{"other_filesystem", "tmpfs", "tmpfs", path, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, _ := json.Marshal(map[string]any{"filesystems": []any{map[string]string{"source": tc.source, "fstype": tc.fs, "target": tc.target}}})
			n := 0
			r := &seqRunner{outs: []string{string(b), "4242\n"}, n: &n}
			got, err := (ZFSCLI{Runner: r}).DatasetAt(context.Background(), path)
			if tc.valid {
				if err != nil || got.Name != tc.source || got.GUID != "4242" {
					t.Fatalf("%+v %v", got, err)
				}
			} else if err == nil {
				t.Fatalf("accepted ineffective mount: %+v", got)
			}
		})
	}
}

type seqRunner struct {
	outs []string
	n    *int
}

func (s *seqRunner) Run(context.Context, []byte, string, ...string) ([]byte, error) {
	o := s.outs[*s.n]
	*s.n++
	return []byte(o), nil
}

// stubRunner runs the real Python bridge script against a stub of the
// TrueNAS client that returns what the real one returns: job records with
// datetime values.
type stubRunner struct{ t *testing.T }

func (s stubRunner) Run(ctx context.Context, stdin []byte, name string, args ...string) ([]byte, error) {
	stub := `import sys, types, datetime
m = types.ModuleType("truenas_api_client")
class Client:
    def __enter__(self): return self
    def __exit__(self, *a): return False
    def call(self, method, *params):
        if method == "core.get_jobs":
            return [{"id": 7, "state": "SUCCESS", "method": "app.stop", "arguments": ["ai-minion"],
                     "time_started": datetime.datetime(2026, 11, 22, 3, 30, tzinfo=datetime.timezone.utc),
                     "time_finished": datetime.datetime(2026, 11, 22, 3, 31)}]
        return {"when": datetime.datetime(2026, 1, 1)}
m.Client = Client
sys.modules["truenas_api_client"] = m
`
	code := stub + args[len(args)-1]
	cmd := exec.CommandContext(ctx, name, append(args[:len(args)-1], code)...)
	cmd.Stdin = bytes.NewReader(stdin)
	return cmd.Output()
}

func TestPythonBridgeSerializesDatetimes(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("no python3")
	}
	m := &MidcltClient{Runner: stubRunner{t}}
	s, err := m.JobState(context.Background(), 7)
	if err != nil || s != JobSuccess {
		t.Fatalf("%v %v", s, err)
	}
	infos, err := m.FindJobs(context.Background(), "app.stop", "ai-minion", time.Date(2026, 11, 22, 3, 0, 0, 0, time.UTC))
	if err != nil || len(infos) != 1 || infos[0].ID != 7 {
		t.Fatalf("%+v %v", infos, err)
	}
	if infos, _ := m.FindJobs(context.Background(), "app.stop", "other", time.Time{}); len(infos) != 0 {
		t.Fatal("a job of another app matched")
	}
	if _, err := m.call(context.Background(), "app.config", "ai-minion"); err != nil {
		t.Fatalf("a datetime in a plain answer: %v", err)
	}
}
