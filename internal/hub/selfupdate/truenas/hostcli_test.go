package truenas

import (
	"context"
	"errors"
	"strings"
	"testing"
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
	r := &seqRunner{outs: []string{"abc\n", "abc|sha256:" + strings.Repeat("1", 64) + "|minion-hub|true\n"}, n: &calls}
	cs, err := DockerCLI{Runner: r}.Containers(context.Background(), "ix-app")
	if err != nil || len(cs) != 1 || !cs[0].Running || cs[0].Service != "minion-hub" {
		t.Fatalf("%+v %v", cs, err)
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
