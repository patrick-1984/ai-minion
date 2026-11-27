package keep

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// fakeProc writes a process table under a temporary root.
type fakeProc struct {
	t    *testing.T
	root string
}

func newFakeProc(t *testing.T) *fakeProc {
	return &fakeProc{t: t, root: t.TempDir()}
}

// add writes pid with its parent, start time and argv.
func (f *fakeProc) add(pid, ppid int, start string, argv ...string) {
	f.t.Helper()
	dir := filepath.Join(f.root, strconv.Itoa(pid))
	if err := os.MkdirAll(filepath.Join(dir, "fd"), 0o700); err != nil {
		f.t.Fatal(err)
	}
	// 20 fields after the name: state, ppid, ... field 19 is the start time.
	fields := make([]string, 20)
	for i := range fields {
		fields[i] = "0"
	}
	fields[0], fields[1], fields[19] = "S", strconv.Itoa(ppid), start
	stat := fmt.Sprintf("%d (%s) %s\n", pid, filepath.Base(argv[0]), strings.Join(fields, " "))
	if err := os.WriteFile(filepath.Join(dir, "stat"), []byte(stat), 0o600); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cmdline"), []byte(strings.Join(argv, "\x00")+"\x00"), 0o600); err != nil {
		f.t.Fatal(err)
	}
}

// openFile makes pid hold target open.
func (f *fakeProc) openFile(pid int, fd string, target string) {
	f.t.Helper()
	if err := os.Symlink(target, filepath.Join(f.root, strconv.Itoa(pid), "fd", fd)); err != nil {
		f.t.Fatal(err)
	}
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

const (
	convA = "11111111-2222-4333-8444-555555555555"
	convB = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
)

func TestDetectClaudeFromItsRegistry(t *testing.T) {
	p := newFakeProc(t)
	home := t.TempDir()
	p.add(100, 1, "500", "/bin/bash")
	p.add(101, 100, "600", "node", "/usr/lib/node_modules/.bin/claude")
	// The registry entry of this claude, one of a dead process whose pid was
	// reused (start time differs), and one of a pid outside the pane.
	writeFile(t, filepath.Join(home, ".claude/sessions/101.json"), `{"pid":101,"sessionId":"`+convA+`","procStart":"600"}`)
	writeFile(t, filepath.Join(home, ".claude/sessions/x.json"), `{"pid":101,"sessionId":"`+convB+`","procStart":"1"}`)
	writeFile(t, filepath.Join(home, ".claude/sessions/y.json"), `{"pid":999,"sessionId":"`+convB+`"}`)
	d := newDetector(p.root, home)
	tool, conv := d.detect(100, "node", "")
	if tool != ToolClaude || conv != convA {
		t.Fatalf("detect = %q %q", tool, conv)
	}
}

func TestDetectClaudeFromResumeArgumentAndMarker(t *testing.T) {
	p := newFakeProc(t)
	home := t.TempDir()
	p.add(200, 1, "5", "/bin/sh")
	p.add(201, 200, "6", "claude", "--resume", convB)
	d := newDetector(p.root, home)
	if tool, conv := d.detect(200, "claude", ""); tool != ToolClaude || conv != convB {
		t.Fatalf("resume argument: %q %q", tool, conv)
	}
	// A restored pane whose claude has not registered yet: the marker names it.
	p.add(300, 1, "5", "/bin/sh")
	p.add(301, 300, "6", "claude", "--continue")
	d = newDetector(p.root, home)
	start := `/bin/sh -lc ": minion-keep claude ` + convA + `; claude --resume '` + convA + `'; exec '/bin/sh' -l"`
	if tool, conv := d.detect(300, "claude", start); tool != ToolClaude || conv != convA {
		t.Fatalf("marker: %q %q", tool, conv)
	}
	// The marker alone does not make a shell a claude pane.
	p.add(400, 1, "5", "/bin/bash")
	d = newDetector(p.root, home)
	if tool, conv := d.detect(400, "bash", ": minion-keep claude "+convA+"; x"); tool != "" || conv != "" {
		t.Fatalf("idle shell with a marker: %q %q", tool, conv)
	}
}

func TestDetectCodexFromItsRolloutFile(t *testing.T) {
	p := newFakeProc(t)
	home := t.TempDir()
	rollout := filepath.Join(home, ".codex/sessions/2026/11/26/rollout-2026-11-26T10-00-00-"+convB+".jsonl")
	writeFile(t, rollout, "{}\n")
	p.add(500, 1, "5", "/bin/zsh")
	p.add(501, 500, "6", "/usr/local/bin/codex")
	p.openFile(501, "3", "/dev/null")
	p.openFile(501, "4", rollout)
	d := newDetector(p.root, home)
	if tool, conv := d.detect(500, "zsh", ""); tool != ToolCodex || conv != convB {
		t.Fatalf("detect = %q %q", tool, conv)
	}
}

func TestDetectNothingInAShellOrWithoutProc(t *testing.T) {
	p := newFakeProc(t)
	p.add(600, 1, "5", "/bin/bash")
	p.add(601, 600, "6", "vim", "notes")
	if tool, conv := newDetector(p.root, t.TempDir()).detect(600, "vim", ""); tool != "" || conv != "" {
		t.Fatalf("detect = %q %q", tool, conv)
	}
	if tool, conv := newDetector(filepath.Join(t.TempDir(), "none"), "").detect(600, "bash", ""); tool != "" || conv != "" {
		t.Fatalf("without a process table: %q %q", tool, conv)
	}
}

func TestLaunchArgv(t *testing.T) {
	tests := []struct {
		tool, conv, shell string
		want              []string
	}{
		{"claude", convA, "/bin/bash", []string{"/bin/bash", "-lc", ": minion-keep claude " + convA + "; claude --resume '" + convA + "'; exec '/bin/bash' -l"}},
		{"claude", "", "/usr/bin/zsh", []string{"/usr/bin/zsh", "-lc", ": minion-keep claude -; claude --continue; exec '/usr/bin/zsh' -l"}},
		{"codex", convB, "/bin/sh", []string{"/bin/sh", "-lc", ": minion-keep codex " + convB + "; codex resume '" + convB + "'; exec '/bin/sh' -l"}},
		{"codex", "", "/bin/bash", []string{"/bin/bash", "-lc", ": minion-keep codex -; codex resume --last; exec '/bin/bash' -l"}},
		// An id that could be shell syntax or an option is not used.
		{"claude", "'; rm -rf ~ #", "/bin/bash", []string{"/bin/bash", "-lc", ": minion-keep claude -; claude --continue; exec '/bin/bash' -l"}},
		{"claude", "--dangerous", "/bin/bash", []string{"/bin/bash", "-lc", ": minion-keep claude -; claude --continue; exec '/bin/bash' -l"}},
		{"", convA, "/bin/bash", nil},
		{"hermes", "", "/bin/bash", nil},
	}
	for _, tc := range tests {
		if got := LaunchArgv(tc.tool, tc.conv, tc.shell); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("LaunchArgv(%q, %q, %q) = %q, want %q", tc.tool, tc.conv, tc.shell, got, tc.want)
		}
	}
	// A shell that is not POSIX (fish) runs the script with sh.
	got := LaunchArgv("claude", "", "/usr/bin/fish")
	if len(got) != 3 || filepath.Base(got[0]) != "sh" || got[1] != "-lc" {
		t.Fatalf("fish: %q", got)
	}
	// The marker the script starts with is the one detect reads.
	if m := markerRE.FindStringSubmatch(LaunchArgv("codex", convB, "/bin/sh")[2]); m == nil || m[1] != "codex" || m[2] != convB {
		t.Fatalf("marker = %q", m)
	}
}
