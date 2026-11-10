package agentdetect

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/ops"
)

// screen loads testdata/<name>.txt as a sanitised screen.
func screen(t *testing.T, name string) ops.Screen {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name+".txt"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	return ops.Screen{Width: 100, Height: len(lines), Lines: lines}
}

// fakeProcs is an in-memory process table.
type fakeProcs struct {
	procs []Proc
	err   error
	calls int
}

func (f *fakeProcs) Processes(context.Context) ([]Proc, error) {
	f.calls++
	return f.procs, f.err
}

// fgGroup is the pane terminal's foreground process group of the fake tables
// and paneTTY the pane's terminal.
const (
	fgGroup = 500
	paneTTY = "pts/1"
)

// proc is a running process on the pane's terminal, in its foreground group.
func proc(pid, ppid int, args ...string) Proc {
	return Proc{PID: pid, PPID: ppid, PGRP: fgGroup, TTY: paneTTY, TPGID: fgGroup, State: "S", Args: args}
}

// nestedProc is a process on another terminal (a pty created by "script"),
// in the foreground group of that terminal.
func nestedProc(pid, ppid int, args ...string) Proc {
	p := proc(pid, ppid, args...)
	p.PGRP, p.TPGID, p.TTY = 700, 700, "pts/9"
	return p
}

// bgProc is a running process of a background job (its own group).
func bgProc(pid, ppid int, args ...string) Proc {
	p := proc(pid, ppid, args...)
	p.PGRP = 900 + pid
	return p
}

// stoppedProc is a job suspended with Ctrl-Z.
func stoppedProc(pid, ppid int, args ...string) Proc {
	p := bgProc(pid, ppid, args...)
	p.State = "T"
	return p
}

func classify(t *testing.T, in Input, procs ProcessSource) Result {
	t.Helper()
	r := New(WithProcessSource(procs)).Classify(context.Background(), in)
	if !r.Valid() {
		t.Fatalf("result outside the closed sets: %+v", r)
	}
	return r
}

// TestGoldenScreens covers idle, busy and dialog for every known agent: the
// command is the agent itself.
func TestGoldenScreens(t *testing.T) {
	for _, agent := range []string{"claude", "codex", "gemini", "qwen", "aider", "opencode"} {
		for _, tc := range []struct{ name, state string }{
			{"idle", ops.PaneStateStable},
			{"busy", ops.PaneStateBusy},
			{"dialog", ops.PaneStateDialog},
		} {
			t.Run(agent+"_"+tc.name, func(t *testing.T) {
				r := classify(t, Input{Command: agent, Screen: screen(t, agent+"_"+tc.name)}, nil)
				if r.Kind != "agent:"+agent || r.State != tc.state {
					t.Fatalf("got %s/%s, want agent:%s/%s", r.Kind, r.State, agent, tc.state)
				}
				if r.State != ops.PaneStateDialog && len(r.Choices) != 0 {
					t.Fatalf("choices outside a dialog: %+v", r.Choices)
				}
			})
		}
	}
}

func TestDialogChoices(t *testing.T) {
	for _, tc := range []struct {
		screen string
		want   []Choice
	}{
		{"claude_dialog", []Choice{
			{"Yes", []string{"1"}},
			{"Yes, and don't ask again for rm commands in /home/user/proj", []string{"2"}},
			{"No, and tell Claude what to do differently (esc)", []string{"3"}},
		}},
		{"codex_dialog", []Choice{
			{"Yes, proceed (y)", []string{"1"}},
			{"Yes, and don't ask again for this command (a)", []string{"2"}},
			{"No, and tell Codex what to do differently (esc)", []string{"3"}},
		}},
		{"gemini_dialog", []Choice{
			{"Allow once", []string{"1"}}, {"Allow always ls", []string{"2"}}, {"No, suggest changes (esc)", []string{"3"}},
		}},
		{"qwen_dialog", []Choice{
			{"Yes, allow once", []string{"1"}}, {"Yes, allow always", []string{"2"}},
			{"Modify with external editor", []string{"3"}}, {"No, suggest changes (esc)", []string{"4"}},
		}},
		{"aider_dialog", []Choice{
			{"Yes", []string{"y", "Enter"}}, {"No", []string{"n", "Enter"}}, {"All", []string{"a", "Enter"}},
			{"Skip all", []string{"s", "Enter"}}, {"Don't ask again", []string{"d", "Enter"}},
		}},
	} {
		t.Run(tc.screen, func(t *testing.T) {
			agent := strings.SplitN(tc.screen, "_", 2)[0]
			r := classify(t, Input{Command: agent, Screen: screen(t, tc.screen)}, nil)
			if r.State != ops.PaneStateDialog {
				t.Fatalf("state %s, want dialog", r.State)
			}
			if !reflect.DeepEqual(r.Choices, tc.want) {
				t.Fatalf("choices\n got %+v\nwant %+v", r.Choices, tc.want)
			}
		})
	}
}

// A dialog marker without a parsable option row is still a dialog, with no
// choices.
func TestDialogWithoutOptionRow(t *testing.T) {
	scr := ops.Screen{Lines: []string{"Permission required: bash", "  make build", "", "  enter confirm"}}
	r := classify(t, Input{Command: "opencode", Screen: scr}, nil)
	if r.State != ops.PaneStateDialog || len(r.Choices) != 0 {
		t.Fatalf("got %+v, want dialog without choices", r)
	}
}

func TestYesNoPrompt(t *testing.T) {
	scr := ops.Screen{Lines: []string{"Some output", "Overwrite config.toml? [Y/n]"}}
	r := classify(t, Input{Command: "aider", Screen: scr}, nil)
	want := []Choice{{"Yes", []string{"y", "Enter"}}, {"No", []string{"n", "Enter"}}}
	if r.State != ops.PaneStateDialog || !reflect.DeepEqual(r.Choices, want) {
		t.Fatalf("got %+v", r)
	}
}

func TestYesNoVariantsGiveKeyAndEnter(t *testing.T) {
	for _, line := range []string{"Continue? (y/n)", "Run it? [Y/n]", "Sure? (yes/no)"} {
		r := classify(t, Input{Command: "aider", Screen: ops.Screen{Lines: []string{line}}}, nil)
		if r.State != ops.PaneStateDialog || len(r.Choices) != 2 {
			t.Fatalf("%q: got %+v", line, r)
		}
		for _, c := range r.Choices {
			if len(c.Keys) != 2 || c.Keys[1] != "Enter" {
				t.Fatalf("%q: choice %+v must be the letter then Enter", line, c)
			}
		}
	}
}

// A numbered list in the agent's prose is not a dialog.
func TestProseListIsNotDialog(t *testing.T) {
	r := classify(t, Input{Command: "claude", Screen: screen(t, "prose_list")}, nil)
	if r.State != ops.PaneStateStable || len(r.Choices) != 0 {
		t.Fatalf("got %+v, want stable without choices", r)
	}
}

// A spinner line that scrolled far above the prompt does not make it busy.
func TestStaleBusyMarkerIgnored(t *testing.T) {
	r := classify(t, Input{Command: "claude", Screen: screen(t, "stale_busy")}, nil)
	if r.State != ops.PaneStateStable {
		t.Fatalf("state %s, want stable", r.State)
	}
}

// Numbered prose above a running turn must not turn the spinner's "esc to
// cancel" into a dialog footer.
func TestBusyStatusIsNotDialogFooter(t *testing.T) {
	prose := []string{"Options:", "1. Keep the layout", "2. Move the parser", "3. Drop the parser", ""}
	for _, agent := range []string{"claude", "codex", "gemini", "qwen", "aider", "opencode"} {
		t.Run(agent, func(t *testing.T) {
			scr := screen(t, agent+"_busy")
			scr.Lines = append(append([]string{}, prose...), scr.Lines...)
			r := classify(t, Input{Command: agent, Screen: scr}, nil)
			if r.State != ops.PaneStateBusy || len(r.Choices) != 0 {
				t.Fatalf("got %+v, want busy without choices", r)
			}
		})
	}
}

// A long label wraps in a narrow pane; every option survives.
func TestWrappedLabelsKeepAllChoices(t *testing.T) {
	for name, lines := range map[string][]string{
		"second wraps": {
			"│ Do you want to proceed?                    │",
			"│ ❯ 1. Yes                                    │",
			"│   2. Yes, and don't ask again for rm        │",
			"│      commands in /home/user/a/long/dir      │",
			"│   3. No, and tell Claude what to do (esc)   │",
		},
		"first wraps": {
			"│ Do you want to proceed?                    │",
			"│ ❯ 1. Yes, and also remember this choice     │",
			"│      for the rest of the session            │",
			"│   2. Yes                                    │",
			"│   3. No, and tell Claude what to do (esc)   │",
		},
	} {
		t.Run(name, func(t *testing.T) {
			r := classify(t, Input{Command: "claude", Screen: ops.Screen{Lines: lines}}, nil)
			if r.State != ops.PaneStateDialog || len(r.Choices) != 3 {
				t.Fatalf("got %+v, want dialog with 3 choices", r)
			}
			for i, c := range r.Choices {
				if !reflect.DeepEqual(c.Keys, []string{strconv.Itoa(i + 1)}) {
					t.Fatalf("choice %d keys %v", i, c.Keys)
				}
			}
			if !strings.Contains(r.Choices[0].Label+r.Choices[1].Label, "commands in") && !strings.Contains(r.Choices[0].Label, "for the rest") {
				t.Fatalf("wrapped text lost: %+v", r.Choices)
			}
			if !strings.HasPrefix(r.Choices[2].Label, "No,") {
				t.Fatalf("third choice is %q, want the rejection", r.Choices[2].Label)
			}
		})
	}
}

func TestEmptyOrUnmarkedScreenIsUnknown(t *testing.T) {
	for name, scr := range map[string]ops.Screen{
		"empty":    {},
		"blank":    {Lines: []string{"", "   ", ""}},
		"unmarked": {Lines: []string{"just some text", "nothing here"}},
	} {
		t.Run(name, func(t *testing.T) {
			r := classify(t, Input{Command: "claude", Screen: scr}, nil)
			if r.Kind != "agent:claude" || r.State != ops.PaneStateUnknown {
				t.Fatalf("got %+v", r)
			}
		})
	}
}

// TestIdleAgentUnderBashIsReady covers the known defect "idle agent under
// bash not ready": the pane's command is the wrapper, the agent is a child,
// and the idle screen must classify as agent:claude + stable.
func TestIdleAgentUnderBashIsReady(t *testing.T) {
	procs := &fakeProcs{procs: []Proc{
		proc(1, 0, "/sbin/init"),
		proc(100, 1, "tmux: server"),
		proc(200, 100, "-bash"),
		proc(300, 200, "bash", "-c", "claude"),
		proc(301, 300, "node", "/usr/lib/node_modules/@anthropic-ai/claude-code/cli.js"),
	}}
	for _, cmd := range []string{"bash", "-bash", "/bin/bash", "node", "sh"} {
		t.Run(cmd, func(t *testing.T) {
			r := classify(t, Input{Command: cmd, PID: 200, Screen: screen(t, "claude_idle")}, procs)
			if r.Kind != "agent:claude" || r.State != ops.PaneStateStable {
				t.Fatalf("got %+v, want agent:claude stable", r)
			}
		})
	}
	// The wrapper itself is the pane process.
	r := classify(t, Input{Command: "bash", PID: 300, Screen: screen(t, "claude_idle")}, procs)
	if r.Kind != "agent:claude" || r.State != ops.PaneStateStable {
		t.Fatalf("pane pid is the wrapper: got %+v", r)
	}
}

func TestProcessTreeFixtures(t *testing.T) {
	for _, tc := range []struct {
		name string
		args [][]string // process args chain below the pane shell (pid 10)
		want string
	}{
		{"bash -c with prefix", [][]string{{"bash", "-c", "FOO=1 exec claude --resume"}}, "claude"},
		{"bash -lc", [][]string{{"bash", "-lc", "codex"}}, "codex"},
		{"bash -c compound without a live agent", [][]string{{"bash", "-c", "claude;", "true"}}, ""},
		{"bash -c compound script string", [][]string{{"bash", "-c", "claude; bash -i; true"}}, ""},
		{"compound under sudo", [][]string{{"sudo", "bash", "-c", "claude && true"}}, ""},
		{"compound with a live agent", [][]string{{"bash", "-c", "claude; true"}, {"claude"}}, "claude"},
		{"package flag then another program", [][]string{{"npx", "--package", "@anthropic-ai/claude-code", "bash"}}, ""},
		{"package flag short then another program", [][]string{{"npx", "-p", "@anthropic-ai/claude-code", "node", "server.js"}}, ""},
		{"package flag with equals then another program", [][]string{{"npx", "--package=@anthropic-ai/claude-code", "bash"}}, ""},
		{"package flag without a program", [][]string{{"npx", "--package", "@anthropic-ai/claude-code"}}, ""},
		{"uvx from then another program", [][]string{{"uvx", "--from", "aider-chat", "python"}}, ""},
		{"uvx from equals then another program", [][]string{{"uvx", "--from=aider-chat", "python"}}, ""},
		{"uv tool run from then another program", [][]string{{"uv", "tool", "run", "--from", "aider-chat", "python"}}, ""},
		{"pipx spec then another program", [][]string{{"pipx", "run", "--spec", "aider-chat", "python"}}, ""},
		{"npx allow-scripts then package flag and another program", [][]string{{"npx", "--allow-scripts", "@anthropic-ai/claude-code", "-p", "@anthropic-ai/claude-code", "bash"}}, ""},
		{"npm exec workspace then another program", [][]string{{"npm", "exec", "--workspace", "claude", "--", "bash"}}, ""},
		{"npx workspace short then another program", [][]string{{"npx", "-w", "claude", "bash"}}, ""},
		{"uvx with-editable then another program", [][]string{{"uvx", "--with-editable", "/src/aider", "python"}}, ""},
		{"uvx upgrade-package short then from", [][]string{{"uvx", "-P", "aider-chat", "--from", "aider-chat", "python"}}, ""},
		{"uvx upgrade-package long then from", [][]string{{"uvx", "--upgrade-package", "aider-chat", "--from", "aider-chat", "python"}}, ""},
		{"uv tool run with-editable then another program", [][]string{{"uv", "tool", "run", "--with-editable", "/src/aider", "python"}}, ""},
		{"uv run package flag then another program", [][]string{{"uv", "run", "--package", "aider", "python"}}, ""},
		{"uv run with-editable then another program", [][]string{{"uv", "run", "--with-editable", "/src/aider", "python"}}, ""},
		{"uv run extra flag then another program", [][]string{{"uv", "run", "--extra", "aider", "python"}}, ""},
		{"uv run group flag then another program", [][]string{{"uv", "run", "--group", "claude", "python"}}, ""},
		{"uv run no-group flag takes a value", [][]string{{"uv", "run", "--no-group", "claude", "python"}}, ""},
		{"uv run no-extra flag takes a value", [][]string{{"uv", "run", "--no-extra", "aider", "python"}}, ""},
		{"uv run unknown attached flag fails closed", [][]string{{"uv", "run", "--future=x", "aider"}}, ""},
		{"uv run unknown flag fails closed", [][]string{{"uv", "run", "--future-flag", "aider", "python"}}, ""},
		{"uv run known flags then the agent", [][]string{{"uv", "run", "--package", "x", "--no-sync", "aider"}}, "aider"},
		{"bun run cwd flag", [][]string{{"bun", "run", "--cwd", "claude", "server.ts"}}, ""},
		{"bun cwd flag", [][]string{{"bun", "--cwd", "claude", "server.ts"}}, ""},
		{"bun bare config is a bool", [][]string{{"bun", "--config", "server.ts", "claude"}}, ""},
		{"bun bare short config is a bool", [][]string{{"bun", "-c", "server.ts", "claude"}}, ""},
		{"bun attached config then the agent", [][]string{{"bun", "--config=bunfig.toml", "/opt/gemini"}}, "gemini"},
		{"bun eval attached fails closed", [][]string{{"bun", "--eval=x", "claude"}}, ""},
		{"bun unknown flag fails closed", [][]string{{"bun", "--future-flag", "claude", "server.ts"}}, ""},
		{"bun known flags then the agent", [][]string{{"bun", "--cwd", "/x", "run", "/opt/gemini"}}, "gemini"},
		{"deno run config flag", [][]string{{"deno", "run", "--config", "claude", "main.ts"}}, ""},
		{"deno run bare env-file is a bool", [][]string{{"deno", "run", "--env-file", "main.ts", "claude"}}, ""},
		{"deno run bare lock fails closed, a safe false negative", [][]string{{"deno", "run", "--lock", "main.ts", "claude"}}, ""},
		{"deno run bare lock may bind the agent as its file, fails closed", [][]string{{"deno", "run", "--lock", "claude", "main.ts"}}, ""},
		{"deno bare lock before the subcommand fails closed", [][]string{{"deno", "--lock", "claude", "main.ts"}}, ""},
		{"deno run bare lock then a flag fails closed", [][]string{{"deno", "run", "--lock", "--config", "claude", "main.ts"}}, ""},
		{"deno run attached lock fails closed", [][]string{{"deno", "run", "--lock=deno.lock", "claude"}}, ""},
		{"deno run bare v8-flags is a bool", [][]string{{"deno", "run", "--v8-flags", "main.ts", "claude"}}, ""},
		{"deno run bare node-modules-dir is a bool", [][]string{{"deno", "run", "--node-modules-dir", "main.ts", "claude"}}, ""},
		{"deno run attached env-file then the agent", [][]string{{"deno", "run", "--env-file=.env", "/opt/gemini"}}, "gemini"},
		{"deno run unknown attached flag fails closed", [][]string{{"deno", "run", "--future=x", "claude"}}, ""},
		{"deno run unknown flag fails closed", [][]string{{"deno", "run", "--future-flag", "claude", "main.ts"}}, ""},
		{"deno run permission flags then the agent", [][]string{{"deno", "run", "-A", "--allow-read=/x", "/opt/gemini"}}, "gemini"},
		{"node title flag", [][]string{{"node", "--title", "claude", "server.js"}}, ""},
		{"node redirect-warnings flag", [][]string{{"node", "--redirect-warnings", "claude", "server.js"}}, ""},
		{"node eval attached fails closed", [][]string{{"node", "--eval=console.log(1)", "claude"}}, ""},
		{"node print attached fails closed", [][]string{{"node", "--print=1", "claude"}}, ""},
		{"node inspect attached then the agent", [][]string{{"node", "--inspect=127.0.0.1:9229", "/usr/lib/node_modules/@anthropic-ai/claude-code/cli.js"}}, "claude"},
		{"node unknown flag fails closed", [][]string{{"node", "--future-flag", "claude", "server.js"}}, ""},
		{"node eval flag", [][]string{{"node", "-e", "claude"}}, ""},
		{"node attached flag value then the agent", [][]string{{"node", "--max-old-space-size=4096", "/usr/lib/node_modules/@anthropic-ai/claude-code/cli.js"}}, "claude"},
		{"node value flag then the agent", [][]string{{"node", "--title", "x", "/opt/lib/node_modules/.bin/gemini"}}, "gemini"},
		{"unknown runner flag before the agent", [][]string{{"npx", "--some-new-flag", "claude"}}, ""},
		{"unknown runner flag with a live agent child", [][]string{{"npx", "--some-new-flag", "x", "bash"}, {"claude"}}, "claude"},
		{"known boolean runner flag then the agent", [][]string{{"npx", "--yes", "--no-install", "claude"}}, "claude"},
		{"package flag then the agent", [][]string{{"npx", "--package", "@anthropic-ai/claude-code", "claude"}}, "claude"},
		{"package flag short then the agent", [][]string{{"npx", "-p", "@anthropic-ai/claude-code", "claude", "--resume"}}, "claude"},
		{"pipx spec then the agent", [][]string{{"pipx", "run", "--spec", "aider-chat", "aider"}}, "aider"},
		{"uvx without from", [][]string{{"uvx", "aider-chat@latest"}}, "aider"},
		{"direct program", [][]string{{"/home/u/.local/bin/aider", "--model", "x"}}, "aider"},
		{"node script", [][]string{{"node", "/opt/lib/node_modules/.bin/gemini"}}, "gemini"},
		{"node package path", [][]string{{"node", "--no-warnings", "/usr/lib/node_modules/@qwen-code/qwen-code/cli.js"}}, "qwen"},
		{"npx package", [][]string{{"npx", "-y", "@google/gemini-cli"}}, "gemini"},
		{"python module", [][]string{{"python3.12", "-m", "aider"}}, "aider"},
		{"deep child", [][]string{{"sudo", "-u", "x", "env"}, {"bash", "-c", "opencode"}}, "opencode"},
		{"windows exe", [][]string{{`C:\tools\Claude.exe`}}, "claude"},
		{"nothing", [][]string{{"sleep", "100"}}, ""},
		{"editing a file named like an agent", [][]string{{"vim", "claude"}}, ""},
		{"shell echo", [][]string{{"bash", "-c", "echo claude"}}, ""},
		{"script path only", [][]string{{"bash", "notes-claude.sh"}}, ""},
		{"zombie-looking name", [][]string{{"claude-notes"}}, ""},
		{"npm install of the claude package", [][]string{{"npm", "install", "-g", "@anthropic-ai/claude-code"}}, ""},
		{"pip install of aider", [][]string{{"python3", "-m", "pip", "install", "aider-chat"}}, ""},
		{"apt install aider", [][]string{{"sudo", "apt", "install", "aider"}}, ""},
		{"pipx install aider", [][]string{{"pipx", "install", "aider-chat"}}, ""},
		{"script argument names an agent", [][]string{{"node", "server.js", "claude"}}, ""},
		{"python script argument", [][]string{{"python3", "tool.py", "--agent", "aider"}}, ""},
		{"python -c code", [][]string{{"python3", "-c", "import aider"}}, ""},
		{"uv pip install", [][]string{{"uv", "pip", "install", "aider-chat"}}, ""},
		{"npm run script", [][]string{{"npm", "run", "claude"}}, ""},
		{"bash script argument", [][]string{{"bash", "run.sh", "claude"}}, ""},
		{"env without program", [][]string{{"env", "FOO=claude"}}, ""},
		{"sudo claude", [][]string{{"sudo", "-u", "me", "claude"}}, "claude"},
		{"env prefix", [][]string{{"env", "FOO=1", "-u", "BAR", "codex"}}, "codex"},
		{"pipx run", [][]string{{"pipx", "run", "aider-chat", "--help"}}, "aider"},
		{"uvx from", [][]string{{"uvx", "--from", "aider-chat", "aider"}}, "aider"},
		{"npx with version", [][]string{{"npx", "@google/gemini-cli@latest"}}, "gemini"},
		{"npm exec", [][]string{{"npm", "exec", "--", "claude"}}, "claude"},
		{"python script path", [][]string{{"python3", "/home/u/.local/bin/aider"}}, "aider"},
		{"shell script file", [][]string{{"bash", "/usr/local/bin/claude"}}, "claude"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ps := []Proc{proc(10, 1, "-zsh")}
			parent := 10
			for i, a := range tc.args {
				ps = append(ps, proc(20+i, parent, a...))
				parent = 20 + i
			}
			r := classify(t, Input{Command: "zsh", PID: 10, Screen: screen(t, "claude_idle")}, &fakeProcs{procs: ps})
			if tc.want == "" {
				if r.IsAgent() {
					t.Fatalf("got %+v, want no agent", r)
				}
				return
			}
			if r.Agent() != tc.want {
				t.Fatalf("got %+v, want agent %s", r, tc.want)
			}
		})
	}
}

// AD-12: a stopped or background agent leaves the input with the shell, and a
// message-only caller must not be told the pane is an agent.
func TestJobControlAgentsDoNotCount(t *testing.T) {
	for _, tc := range []struct {
		name  string
		procs []Proc
		cmd   string
		want  string // kind/state
	}{
		{"stopped claude below bash", []Proc{proc(10, 1, "-bash"), stoppedProc(11, 10, "claude")}, "bash", "shell/stable"},
		{"stopped wrapper chain", []Proc{proc(10, 1, "-bash"), stoppedProc(11, 10, "bash", "-c", "claude"), stoppedProc(12, 11, "node", "/x/@anthropic-ai/claude-code/cli.js")}, "bash", "shell/stable"},
		{"background claude -p", []Proc{proc(10, 1, "-bash"), bgProc(11, 10, "claude", "-p", "x")}, "bash", "shell/stable"},
		{"zombie claude", []Proc{proc(10, 1, "-bash"), {PID: 11, PPID: 10, PGRP: fgGroup, TPGID: fgGroup, State: "Z"}}, "bash", "shell/stable"},
		{"zombie with an agent argv", []Proc{proc(10, 1, "-bash"), {PID: 11, PPID: 10, PGRP: fgGroup, TPGID: fgGroup, State: "Z", Args: []string{"claude"}}}, "bash", "shell/stable"},
		{"zsh with a background helper", []Proc{proc(10, 1, "-zsh"), bgProc(11, 10, "gitstatusd-linux-x86_64", "-G", "v1")}, "zsh", "shell/stable"},
		{"foreground job", []Proc{proc(10, 1, "-bash"), proc(11, 10, "make", "build")}, "bash", "shell/busy"},
		{"foreground job beside a stopped one", []Proc{proc(10, 1, "-bash"), stoppedProc(11, 10, "vim"), proc(12, 10, "make")}, "bash", "shell/busy"},
		{"stopped job only", []Proc{proc(10, 1, "-bash"), stoppedProc(11, 10, "vim")}, "bash", "shell/stable"},
		{"foreground claude", []Proc{proc(10, 1, "-bash"), proc(11, 10, "claude")}, "bash", "agent:claude/stable"},
		{"unknown group data is not foreground", []Proc{proc(10, 1, "-bash"), {PID: 11, PPID: 10, Args: []string{"claude"}}}, "bash", "shell/stable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := classify(t, Input{Command: tc.cmd, PID: 10, Screen: screen(t, "claude_idle")}, &fakeProcs{procs: tc.procs})
			if got := r.Kind + "/" + r.State; got != tc.want {
				t.Fatalf("got %s, want %s", got, tc.want)
			}
		})
	}
}

func TestOtherProcessTreesAreIgnored(t *testing.T) {
	// Another pane's agent (different parent) must not leak in.
	procs := &fakeProcs{procs: []Proc{
		proc(10, 1, "-bash"),
		proc(50, 1, "-bash"),
		proc(51, 50, "claude"),
	}}
	r := classify(t, Input{Command: "bash", PID: 10, Screen: screen(t, "shell_prompt")}, procs)
	if r.Kind != "shell" {
		t.Fatalf("got %+v, want shell", r)
	}
}

func TestShellState(t *testing.T) {
	idle := &fakeProcs{procs: []Proc{proc(10, 1, "-bash")}}
	busy := &fakeProcs{procs: []Proc{proc(10, 1, "-bash"), proc(11, 10, "make", "build")}}
	broken := &fakeProcs{err: errors.New("no ps")}
	for _, tc := range []struct {
		name  string
		procs ProcessSource
		pid   int
		want  string
	}{
		{"no child", idle, 10, ops.PaneStateStable},
		{"child running", busy, 10, ops.PaneStateBusy},
		{"table unavailable", broken, 10, ops.PaneStateUnknown},
		{"no source", nil, 10, ops.PaneStateUnknown},
		{"no pid", idle, 0, ops.PaneStateUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := classify(t, Input{Command: "bash", PID: tc.pid, Screen: screen(t, "shell_prompt")}, tc.procs)
			if r.Kind != ops.PaneKindShell || r.State != tc.want || len(r.Choices) != 0 {
				t.Fatalf("got %+v, want shell/%s", r, tc.want)
			}
		})
	}
}

func TestUnrecognisedProgramIsOtherUnknown(t *testing.T) {
	procs := &fakeProcs{procs: []Proc{proc(10, 1, "vim", "x.txt"), proc(11, 10, "claude")}}
	for _, cmd := range []string{"vim", "htop", "less", "ssh", "my-tool"} {
		r := classify(t, Input{Command: cmd, PID: 10, Screen: screen(t, "claude_idle")}, procs)
		if r.Kind != ops.PaneKindOther || r.State != ops.PaneStateUnknown || len(r.Choices) != 0 {
			t.Fatalf("%s: got %+v, want other/unknown", cmd, r)
		}
	}
	if procs.calls != 0 {
		t.Fatalf("the process table was read for a non-wrapper command (%d calls)", procs.calls)
	}
}

func TestNoEvidenceIsUnknown(t *testing.T) {
	r := classify(t, Input{}, nil)
	if r.Kind != ops.PaneKindUnknown || r.State != ops.PaneStateUnknown {
		t.Fatalf("got %+v", r)
	}
	// An empty command with an agent in the tree is still found.
	procs := &fakeProcs{procs: []Proc{proc(10, 1, "claude")}}
	r = classify(t, Input{PID: 10, Screen: screen(t, "claude_idle")}, procs)
	if r.Kind != "agent:claude" || r.State != ops.PaneStateStable {
		t.Fatalf("got %+v", r)
	}
}

// Classification never errors and never leaves the closed sets, whatever the
// input.
func TestClosedSetsAndNoPanic(t *testing.T) {
	procs := &fakeProcs{procs: []Proc{
		proc(1, 1, "weird"),    // self-parent loop
		proc(2, 3), proc(3, 2), // empty args and a cycle
		proc(10, 1, "bash", "-c"),
	}}
	cmds := []string{"", "-", "/", ".", "claude", "bash", "node", "???", "claude\x00", strings.Repeat("x", 5000)}
	screens := []ops.Screen{{}, screen(t, "claude_dialog"), screen(t, "aider_busy"), {Lines: []string{strings.Repeat("1. ", 1000)}}}
	for _, c := range cmds {
		for _, s := range screens {
			for _, pid := range []int{0, 1, 2, 10, -5} {
				r := New(WithProcessSource(procs)).Classify(context.Background(), Input{Command: c, PID: pid, Screen: s})
				if !r.Valid() {
					t.Fatalf("command %q pid %d: outside the closed sets: %+v", c, pid, r)
				}
			}
		}
	}
}

func TestResultValid(t *testing.T) {
	for _, r := range []Result{
		{Kind: "agent:", State: ops.PaneStateStable},
		{Kind: "robot", State: ops.PaneStateStable},
		{Kind: "shell", State: "idle"},
		{Kind: "", State: ""},
	} {
		if r.Valid() {
			t.Errorf("%+v must not be valid", r)
		}
	}
}

func TestExtensibleTable(t *testing.T) {
	custom := append(DefaultAgents(), Agent{
		Name:     "mycli",
		Commands: []string{"mycli"},
		Idle:     []*regexp.Regexp{rx(`^mycli ready$`)},
	})
	d := New(WithAgents(custom), WithProcessSource(nil))
	r := d.Classify(context.Background(), Input{Command: "mycli", Screen: ops.Screen{Lines: []string{"mycli ready"}}})
	if r.Kind != "agent:mycli" || r.State != ops.PaneStateStable {
		t.Fatalf("got %+v", r)
	}
	// The default table is not shared between calls.
	a, b := DefaultAgents(), DefaultAgents()
	a[0].Name = "changed"
	if b[0].Name != "claude" {
		t.Fatal("DefaultAgents returned shared state")
	}
}

func TestDefaultAgentNames(t *testing.T) {
	var names []string
	for _, a := range DefaultAgents() {
		names = append(names, a.Name)
	}
	want := []string{"claude", "codex", "gemini", "qwen", "aider", "opencode"}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("agents %v, want %v", names, want)
	}
}

func TestPackageClassify(t *testing.T) {
	r := Classify(context.Background(), Input{Command: "claude", Screen: screen(t, "claude_busy")})
	if r.Kind != "agent:claude" || r.State != ops.PaneStateBusy {
		t.Fatalf("got %+v", r)
	}
}

func TestParsePS(t *testing.T) {
	out := "    1     0     1     -1 ?? Ss /sbin/init splash\n  200   100   200    300 ttys001 Ss+ -bash\n  300   200   300    300 ttys001 S+ bash -c claude\n  301   200   301    300 ttys001 T claude\nbad line\n x y z\n  400\n 5 6 7 8 ttys001\n"
	got := parsePS(out)
	want := []Proc{
		{PID: 1, PPID: 0, PGRP: 1, TPGID: -1, State: "Ss", Args: []string{"/sbin/init", "splash"}},
		{PID: 200, PPID: 100, PGRP: 200, TTY: "ttys001", TPGID: 300, State: "Ss+", Args: []string{"-bash"}},
		{PID: 300, PPID: 200, PGRP: 300, TTY: "ttys001", TPGID: 300, State: "S+", Args: []string{"bash", "-c", "claude"}},
		{PID: 301, PPID: 200, PGRP: 301, TTY: "ttys001", TPGID: 300, State: "T", Args: []string{"claude"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v", got)
	}
	if !got[2].Foreground() || got[3].Foreground() || got[3].Running() || got[0].Foreground() {
		t.Fatal("foreground or run state misread")
	}
}

// fakeProc writes one /proc/<pid> entry of a fake procfs tree.
func fakeProc(t *testing.T, root string, pid int, stat, cmdline string) {
	t.Helper()
	dir := filepath.Join(root, strconv.Itoa(pid))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if stat != "" {
		if err := os.WriteFile(filepath.Join(dir, "stat"), []byte(stat), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if cmdline != "" {
		if err := os.WriteFile(filepath.Join(dir, "cmdline"), []byte(cmdline), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// TestProcSourceParsesFakeProcfs reads a fake /proc tree: exact argv from
// cmdline, ppid/pgrp/tpgid/state from stat (also for a comm with spaces and
// parentheses), unreadable and non-numeric entries skipped.
func TestProcSourceParsesFakeProcfs(t *testing.T) {
	root := t.TempDir()
	fakeProc(t, root, 200, "200 (bash) S 100 200 200 34816 300 4194560 1 0 0 0", "-bash\x00")
	fakeProc(t, root, 300, "300 (sh) S 200 300 200 34816 300 4194304 1 0 0 0", "bash\x00-c\x00cd . && claude\x00")
	fakeProc(t, root, 301, "301 (we ird) (x) T 200 301 200 34816 300 4194304 1 0 0 0", "sh\x00/tmp/dir/claude\x00")
	fakeProc(t, root, 302, "302 (claude) Z 200 300 200 34816 300 4194304 1 0 0 0", "\x00")       // zombie: empty cmdline
	fakeProc(t, root, 400, "", "orphan\x00")                                                     // no stat: vanished
	fakeProc(t, root, 401, "garbage", "x\x00")                                                   // malformed stat
	fakeProc(t, root, 402, "402 (x) S 1 402 402 0 -1 0 0 0 0 0", "")                             // no cmdline file: skipped
	fakeProc(t, root, 303, "303 (claude) S 300 303 200 34817 303 4194304 1 0 0 0", "claude\x00") // nested pty: other tty
	fakeProc(t, root, 304, "304 (daemon) S 1 304 304 0 -1 4194304 1 0 0 0", "daemon\x00")        // no terminal
	if err := os.MkdirAll(filepath.Join(root, "self"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "cpuinfo"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := ProcSource{Root: root}.Processes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	byPID := map[int]Proc{}
	for _, p := range got {
		byPID[p.PID] = p
	}
	if len(byPID) != 6 {
		t.Fatalf("got pids %v, want 200, 300, 301, 302, 303, 304", byPID)
	}
	if p := byPID[304]; p.TTY != "" || p.Foreground() {
		t.Fatalf("pid 304 has no terminal: %+v", p)
	}
	if p := byPID[303]; p.TTY != "34817" || !p.Foreground() || inPaneForeground(byPID[200], p) {
		t.Fatalf("pid 303 is foreground on another terminal: %+v", p)
	}
	if p := byPID[302]; p.State != "Z" || p.Running() || len(p.Args) != 0 {
		t.Fatalf("pid 302: %+v", p)
	}
	if p := byPID[300]; p.TTY != "34816" || p.PPID != 200 || p.PGRP != 300 || p.TPGID != 300 || p.State != "S" ||
		!reflect.DeepEqual(p.Args, []string{"bash", "-c", "cd . && claude"}) || !p.Foreground() {
		t.Fatalf("pid 300: %+v", p)
	}
	if p := byPID[301]; p.PPID != 200 || p.PGRP != 301 || p.TPGID != 300 || p.State != "T" || p.Running() || p.Foreground() {
		t.Fatalf("pid 301: %+v", p)
	}
	if p := byPID[200]; !reflect.DeepEqual(p.Args, []string{"-bash"}) || p.Foreground() {
		t.Fatalf("pid 200: %+v", p)
	}

	// The fake tree classifies like the real one: the suspended agent does
	// not count, the foreground wrapper's "cd . && claude" is not an agent.
	r := classify(t, Input{Command: "bash", PID: 200, Screen: screen(t, "claude_idle")}, ProcSource{Root: root})
	if r.Kind != "shell" || r.State != ops.PaneStateBusy {
		t.Fatalf("got %+v, want shell/busy (a foreground wrapper, no agent)", r)
	}
}

func TestProcSourceMissingRootIsAnError(t *testing.T) {
	if _, err := (ProcSource{Root: filepath.Join(t.TempDir(), "nope")}).Processes(context.Background()); err == nil {
		t.Fatal("want an error for a missing procfs root")
	}
	r := classify(t, Input{Command: "bash", PID: 10, Screen: screen(t, "shell_prompt")}, ProcSource{Root: filepath.Join(t.TempDir(), "nope")})
	if r.Kind != ops.PaneKindShell || r.State != ops.PaneStateUnknown {
		t.Fatalf("got %+v, want shell/unknown", r)
	}
}

func TestParseProcStat(t *testing.T) {
	p, ok := parseProcStat([]byte("12 (a b) c) R 3 4 5 6 7 8"))
	if !ok || p.State != "R" || p.PPID != 3 || p.PGRP != 4 || p.TPGID != 7 {
		t.Fatalf("got %+v %v", p, ok)
	}
	for _, bad := range []string{"", "12 (x", "12 (x) S 1 2", "12 (x) S a b c d e"} {
		if _, ok := parseProcStat([]byte(bad)); ok {
			t.Errorf("%q must not parse", bad)
		}
	}
}

func TestDescendantsBounds(t *testing.T) {
	var all []Proc
	for i := 2; i < 1000; i++ { // a wide fan-out below the root
		all = append(all, proc(i, 1, "x"))
	}
	if got := descendants(all, 1); len(got) != maxTreeProcs {
		t.Fatalf("got %d procs, want %d", len(got), maxTreeProcs)
	}
	var chain []Proc // a deep chain
	for i := 2; i < 50; i++ {
		chain = append(chain, proc(i, i-1, "x"))
	}
	if got := descendants(chain, 1); len(got) != maxTreeDepth+1 {
		t.Fatalf("got %d procs, want %d", len(got), maxTreeDepth+1)
	}
}

// opencodeRowScreen returns the golden OpenCode permission dialog with the
// style spans of the option row: every option is a button with the menu
// background, the selected one with the highlight background. selected < 0
// draws no spans at all (a screen without style information).
func opencodeRowScreen(t *testing.T, selected int) ops.Screen {
	t.Helper()
	scr := screen(t, "opencode_dialog")
	if selected < 0 {
		return scr
	}
	row := -1
	for i, l := range scr.Lines {
		if strings.Contains(l, "Allow once") {
			row = i
		}
	}
	if row < 0 {
		t.Fatal("no option row in the golden screen")
	}
	at := func(label string) int {
		i := strings.Index(scr.Lines[row], label)
		return len([]rune(scr.Lines[row][:i]))
	}
	for i, label := range []string{"Allow once", "Allow always", "Reject"} {
		sp := ops.StyleSpan{Line: row, Start: at(label) - 1, End: at(label) + len([]rune(label)) + 1, FG: "#808080", BG: "#202020"}
		if i == selected {
			sp.FG, sp.BG, sp.Bold = "#000000", "#ffcc00", true
		}
		scr.Spans = append(scr.Spans, sp)
	}
	return scr
}

// menuModel is the OpenCode permission row driven by the arrow keys: it moves
// on Left and Right (clamped at the ends, or wrapping around) and confirms on
// Enter.
func menuModel(n, selected int, wrap bool, keys []string) (confirmed int) {
	for _, k := range keys {
		switch k {
		case "Right":
			selected++
		case "Left":
			selected--
		case "Enter":
			return selected
		default:
			return -1
		}
		if wrap {
			selected = (selected + n) % n
		} else if selected < 0 {
			selected = 0
		} else if selected >= n {
			selected = n - 1
		}
	}
	return -1
}

// Every OpenCode choice confirms the option it names, whichever option the
// human left selected. A first-option assumption would confirm "Allow
// always" for a human who moved the selection there and then asked to allow
// once.
func TestOpenCodeChoicesFromEverySelection(t *testing.T) {
	labels := []string{"Allow once", "Allow always", "Reject"}
	for sel := range labels {
		r := classify(t, Input{Command: "opencode", Screen: opencodeRowScreen(t, sel)}, nil)
		if r.State != ops.PaneStateDialog || len(r.Choices) != len(labels) {
			t.Fatalf("selected %d: got %+v, want a dialog with %d choices", sel, r, len(labels))
		}
		for want, c := range r.Choices {
			if c.Label != labels[want] {
				t.Fatalf("selected %d: choice %d is %q, want %q", sel, want, c.Label, labels[want])
			}
			for _, wrap := range []bool{false, true} {
				if got := menuModel(len(labels), sel, wrap, c.Keys); got != want {
					t.Errorf("selected %d, wrap %v: %q sends %v and confirms option %d, want %d", sel, wrap, c.Label, c.Keys, got, want)
				}
			}
		}
	}
	// The moves from the first option are the documented minimum.
	r := classify(t, Input{Command: "opencode", Screen: opencodeRowScreen(t, 0)}, nil)
	want := []Choice{
		{"Allow once", []string{"Enter"}},
		{"Allow always", []string{"Right", "Enter"}},
		{"Reject", []string{"Right", "Right", "Enter"}},
	}
	if !reflect.DeepEqual(r.Choices, want) {
		t.Fatalf("choices\n got %+v\nwant %+v", r.Choices, want)
	}
}

// Without a readable selection no key is safe: the dialog stays a dialog and
// offers no choices.
func TestOpenCodeUnreadableSelectionOffersNoChoices(t *testing.T) {
	row := func(styles ...ops.StyleSpan) ops.Screen {
		scr := opencodeRowScreen(t, -1)
		for i, l := range scr.Lines {
			if strings.Contains(l, "Allow once") {
				for _, sp := range styles {
					sp.Line = i
					scr.Spans = append(scr.Spans, sp)
				}
			}
		}
		return scr
	}
	// Option starts in "   ┃  Allow once    Allow always    Reject".
	const once, always, reject = 6, 20, 36
	same := func(start int) ops.StyleSpan { return ops.StyleSpan{Start: start, End: start + 4, BG: "#202020"} }
	for name, scr := range map[string]ops.Screen{
		"no spans":              opencodeRowScreen(t, -1),
		"all options alike":     row(same(once), same(always), same(reject)),
		"two odd ones":          row(ops.StyleSpan{Start: once, End: once + 4, BG: "#ffcc00"}, ops.StyleSpan{Start: always, End: always + 4, BG: "#ff0000"}, same(reject)),
		"spans on another line": {Lines: opencodeRowScreen(t, -1).Lines, Spans: []ops.StyleSpan{{Line: 0, Start: 0, End: 5, BG: "#ffcc00"}}},
	} {
		t.Run(name, func(t *testing.T) {
			r := classify(t, Input{Command: "opencode", Screen: scr}, nil)
			if r.State != ops.PaneStateDialog || len(r.Choices) != 0 {
				t.Fatalf("got %+v, want dialog without choices", r)
			}
		})
	}
}

// A menu of two options reads its selection from the highlighted option.
func TestOpenCodeTwoOptionMenu(t *testing.T) {
	lines := []string{"Permission required: edit", "  a.go", "", "  Allow once    Reject", "", "  enter confirm"}
	for sel := 0; sel < 2; sel++ {
		scr := ops.Screen{Lines: lines}
		scr.Spans = []ops.StyleSpan{{Line: 3, Start: 2, End: 12, BG: "#202020"}, {Line: 3, Start: 16, End: 22, BG: "#202020"}}
		scr.Spans[sel].BG = "#ffcc00"
		scr.Spans[sel].Inverse = true
		r := classify(t, Input{Command: "opencode", Screen: scr}, nil)
		if len(r.Choices) != 2 {
			t.Fatalf("selected %d: got %+v", sel, r)
		}
		for want, c := range r.Choices {
			if got := menuModel(2, sel, false, c.Keys); got != want {
				t.Errorf("selected %d: %q confirms %d, want %d", sel, c.Label, got, want)
			}
		}
	}
}

// AD-12: the pane's input goes to the processes on the pane's terminal in its
// foreground group; an agent on another terminal does not receive it.
func TestPaneTerminalOwnsTheInput(t *testing.T) {
	for _, tc := range []struct {
		name  string
		procs []Proc
		want  string
	}{
		{"agent on a nested pty under a background script", []Proc{
			proc(10, 1, "-bash"),
			bgProc(11, 10, "script", "-q", "-c", "claude", "/dev/null"),
			nestedProc(12, 11, "claude"),
		}, "shell/stable"},
		{"nested pty agent below a background wrapper chain", []Proc{
			proc(10, 1, "-bash"),
			bgProc(11, 10, "bash", "-c", "script -q -c claude /dev/null"),
			bgProc(12, 11, "script", "-q", "-c", "claude", "/dev/null"),
			nestedProc(13, 12, "node", "/x/@anthropic-ai/claude-code/cli.js"),
		}, "shell/stable"},
		{"a foreground job on another pty does not make the shell busy", []Proc{
			proc(10, 1, "-bash"),
			bgProc(11, 10, "script", "-q", "-c", "make", "/dev/null"),
			nestedProc(12, 11, "make"),
		}, "shell/stable"},
		{"foreground script relays the pane input", []Proc{
			proc(10, 1, "-bash"),
			proc(11, 10, "script", "-q", "-c", "claude", "/dev/null"),
			nestedProc(12, 11, "claude"),
		}, "agent:claude/stable"},
		{"unknown terminal of the root", []Proc{
			{PID: 10, PPID: 1, PGRP: fgGroup, TPGID: fgGroup, State: "S", Args: []string{"-bash"}},
			{PID: 11, PPID: 10, PGRP: fgGroup, TPGID: fgGroup, State: "S", Args: []string{"claude"}},
		}, "shell/stable"},
		{"descendant without a terminal", []Proc{
			proc(10, 1, "-bash"),
			{PID: 11, PPID: 10, PGRP: fgGroup, TPGID: fgGroup, State: "S", Args: []string{"claude"}},
		}, "shell/stable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := classify(t, Input{Command: "bash", PID: 10, Screen: screen(t, "claude_idle")}, &fakeProcs{procs: tc.procs})
			if got := r.Kind + "/" + r.State; got != tc.want {
				t.Fatalf("got %s, want %s", got, tc.want)
			}
		})
	}
}

// AD-12: a wrapper that kept its argv after the agent exited is not the
// agent: the root needs foreground evidence like any other process, and the
// first word of a compound script is not evidence.
func TestExitedAgentUnderCompoundWrapper(t *testing.T) {
	wrapper := func(fg bool) Proc {
		p := proc(10, 1, "bash", "-c", "claude; bash -i; true")
		if !fg {
			p.PGRP = 10 // the interactive child took the terminal
		}
		return p
	}
	for _, tc := range []struct {
		name  string
		procs []Proc
	}{
		{"interactive shell took the terminal", []Proc{wrapper(false), proc(11, 10, "bash", "-i")}},
		{"wrapper still in the foreground", []Proc{wrapper(true), proc(11, 10, "bash", "-i")}},
		{"wrapper alone", []Proc{wrapper(true)}},
		{"root outside the foreground group", []Proc{func() Proc { p := proc(10, 1, "claude"); p.PGRP = 10; return p }()}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := classify(t, Input{Command: "bash", PID: 10, Screen: screen(t, "claude_idle")}, &fakeProcs{procs: tc.procs})
			if r.IsAgent() {
				t.Fatalf("got %+v, want no agent", r)
			}
		})
	}
	// The same wrapper while the agent runs: the live child is the evidence.
	r := classify(t, Input{Command: "bash", PID: 10, Screen: screen(t, "claude_idle")},
		&fakeProcs{procs: []Proc{wrapper(true), proc(11, 10, "claude")}})
	if r.Agent() != "claude" {
		t.Fatalf("got %+v, want agent:claude", r)
	}
	// A root that is the agent itself still counts while it is foreground.
	r = classify(t, Input{Command: "node", PID: 10, Screen: screen(t, "claude_idle")},
		&fakeProcs{procs: []Proc{proc(10, 1, "node", "/x/@anthropic-ai/claude-code/cli.js")}})
	if r.Agent() != "claude" {
		t.Fatalf("got %+v, want agent:claude", r)
	}
}
