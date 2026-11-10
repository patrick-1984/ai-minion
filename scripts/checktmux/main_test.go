package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func run(t *testing.T, files map[string]string) []string {
	t.Helper()
	root := t.TempDir()
	for name, src := range files {
		p := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(src), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	got, err := check([]string{root})
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestCheck(t *testing.T) {
	const head = "package x\n\nimport (\n\t\"context\"\n\t\"os/exec\"\n)\n\nvar _ = context.Background\nvar _ = exec.Command\n\n"
	cases := []struct {
		name  string
		src   string
		wantN int
	}{
		{"plain command", `func f() { exec.Command("tmux", "new-session").Run() }`, 1},
		{"nested context expression", `func f() { exec.CommandContext(context.Background(), "tmux", "new-session", "-d").Run() }`, 1},
		{"multiline call", "func f() {\n\texec.Command(\n\t\t\"tmux\",\n\t\t\"new-session\",\n\t).Run()\n}", 1},
		{"tmux inside a shell string", `func f() { exec.Command("sh", "-c", "tmux kill-session -t a").Run() }`, 1},
		{"even with -S it must go through tmuxtest", `func f() { exec.Command("tmux", "-S", "/tmp/s", "ls").Run() }`, 1},
		{"kill-server literal", `var s = "kill-server"`, 1},
		{"unrelated command", `func f() { exec.Command("git", "status").Run() }`, 0},
		{"tmux as plain text", `var name = "tmux adapter"; var _ = name`, 0},
		{"tmuxtest identifier-like word", `func f() { exec.Command("go", "test", "./internal/testutil/tmuxtest").Run() }`, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := run(t, map[string]string{"a_test.go": head + c.src + "\n"})
			if len(got) != c.wantN {
				t.Fatalf("want %d problems, got %d: %s", c.wantN, len(got), strings.Join(got, "; "))
			}
		})
	}
}

func TestCheckExemptsTmuxtestAndNonTestFiles(t *testing.T) {
	src := "package x\n\nimport \"os/exec\"\n\nfunc f() { exec.Command(\"tmux\", \"kill-server\").Run() }\n"
	got := run(t, map[string]string{
		"tmuxtest/a_test.go": src,
		"b.go":               src,
	})
	if len(got) != 0 {
		t.Fatalf("want no problems, got %v", got)
	}
}
