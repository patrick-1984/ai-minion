// Package keep is the keep list of the Worker (session_keep_set, v0.24.0):
// it saves the layout of every kept session and recreates a kept session that
// is missing when the Worker (or its managed terminal server) starts again.
//
// What is saved per pane: the window and split layout, the pane's folder and
// the agent tool running in it (claude or codex) with its conversation id.
// Never screen content, input, keys or environment values. The conversation
// id is found the way the workspace's wb-tmux-state does it: Claude Code's own
// registry (~/.claude/sessions/<pid>.json, matched by pid and process start
// time), a --resume argument, the Codex rollout file the codex process has
// open (~/.codex/sessions/.../rollout-...-<uuid>.jsonl), and the marker of a
// pane this package restored itself. Detection reads /proc and is bounded and
// best effort: on a system without /proc the tool is unknown and the pane is
// restored as a shell in its folder.
package keep

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// The agent tools the keeper knows how to resume.
const (
	ToolClaude = "claude"
	ToolCodex  = "codex"
)

// Bounds of one detection pass.
const (
	maxProcs        = 32768
	maxPanePIDs     = 256
	maxFDs          = 1024
	maxRegistry     = 512
	maxRegistryFile = 64 << 10
)

var (
	uuidRE = regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)
	// idRE is a conversation id that is safe as one CLI argument: never an
	// option, never shell syntax.
	idRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	// markerRE reads the marker a restored pane was started with (see
	// LaunchArgv). tmux shows the start command of a pane as the quoted
	// argv, so the marker is found inside it.
	markerRE = regexp.MustCompile(`: ` + marker + ` (claude|codex) ([A-Za-z0-9._-]+);`)
)

// marker names a pane this package started: the first words of its start
// command, which tmux keeps for the life of the pane.
const marker = "minion-keep"

// ValidConversation reports whether id can be passed to a tool.
func ValidConversation(id string) bool { return idRE.MatchString(id) }

// procTable is one read of the process table.
type procTable struct {
	root     string
	children map[int][]int
}

// readProcs reads the parent of every process under root (normally /proc).
// An unreadable table is empty: nothing is detected.
func readProcs(root string) procTable {
	t := procTable{root: root, children: map[int][]int{}}
	ents, err := os.ReadDir(root)
	if err != nil {
		return t
	}
	n := 0
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 0 {
			continue
		}
		if n++; n > maxProcs {
			break
		}
		f := statFields(root, pid)
		if len(f) < 2 {
			continue
		}
		if ppid, err := strconv.Atoi(f[1]); err == nil {
			t.children[ppid] = append(t.children[ppid], pid)
		}
	}
	return t
}

// statFields returns the fields of /proc/<pid>/stat after the command name:
// [0] is the state, [1] the parent pid, [19] the start time.
func statFields(root string, pid int) []string {
	b, err := os.ReadFile(filepath.Join(root, strconv.Itoa(pid), "stat"))
	if err != nil {
		return nil
	}
	i := bytes.LastIndexByte(b, ')')
	if i < 0 {
		return nil
	}
	return strings.Fields(string(b[i+1:]))
}

// procStart is the start time of pid in clock ticks since boot (what Claude's
// registry calls procStart), "" when unknown.
func (t procTable) procStart(pid int) string {
	f := statFields(t.root, pid)
	if len(f) < 20 {
		return ""
	}
	return f[19]
}

// tree returns pid and its descendants, at most maxPanePIDs.
func (t procTable) tree(pid int) []int {
	if pid <= 0 {
		return nil
	}
	out, todo := []int{pid}, []int{pid}
	for len(todo) > 0 && len(out) < maxPanePIDs {
		p := todo[0]
		todo = todo[1:]
		for _, c := range t.children[p] {
			if len(out) >= maxPanePIDs {
				break
			}
			out = append(out, c)
			todo = append(todo, c)
		}
	}
	return out
}

func (t procTable) cmdline(pid int) []string {
	b, err := os.ReadFile(filepath.Join(t.root, strconv.Itoa(pid), "cmdline"))
	if err != nil {
		return nil
	}
	var out []string
	for _, a := range bytes.Split(b, []byte{0}) {
		if len(a) > 0 {
			out = append(out, string(a))
		}
	}
	return out
}

// toolOf names the tool a process runs: its program, or the script a wrapper
// such as node runs, or the binary of a native Claude Code install.
func (t procTable) toolOf(pid int, home string) string {
	a := t.cmdline(pid)
	for i, w := range a {
		if i > 1 {
			break
		}
		switch filepath.Base(w) {
		case ToolClaude:
			return ToolClaude
		case ToolCodex:
			return ToolCodex
		}
	}
	if home != "" {
		if exe, err := os.Readlink(filepath.Join(t.root, strconv.Itoa(pid), "exe")); err == nil &&
			strings.HasPrefix(exe, filepath.Join(home, ".local/share/claude/versions")+"/") {
			return ToolClaude
		}
	}
	return ""
}

// detector finds the tool and conversation of a pane.
type detector struct {
	procs procTable
	home  string
	// registry is Claude Code's session registry, read once per pass.
	registry []claudeEntry
}

type claudeEntry struct {
	PID       int    `json:"pid"`
	SessionID string `json:"sessionId"`
	// ProcStart is a number or a string in the files.
	ProcStart json.RawMessage `json:"procStart"`
}

func (e claudeEntry) start() string {
	s := strings.Trim(string(e.ProcStart), `"`)
	if s == "null" {
		return ""
	}
	return s
}

func newDetector(procRoot, home string) *detector {
	d := &detector{procs: readProcs(procRoot), home: home}
	if home == "" {
		return d
	}
	files, _ := filepath.Glob(filepath.Join(home, ".claude", "sessions", "*.json"))
	sort.Strings(files)
	if len(files) > maxRegistry {
		files = files[:maxRegistry]
	}
	for _, f := range files {
		fi, err := os.Stat(f)
		if err != nil || !fi.Mode().IsRegular() || fi.Size() > maxRegistryFile {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		var e claudeEntry
		if json.Unmarshal(b, &e) == nil && e.PID > 0 && e.SessionID != "" {
			d.registry = append(d.registry, e)
		}
	}
	return d
}

// detect returns the tool and conversation of the pane whose process is pid,
// whose current command is command and that was started with start.
func (d *detector) detect(pid int, command, start string) (tool, conv string) {
	pids := d.procs.tree(pid)
	if command == ToolClaude || command == ToolCodex {
		tool = command
	}
	for _, p := range pids {
		if tool != "" {
			break
		}
		tool = d.procs.toolOf(p, d.home)
	}
	switch tool {
	case ToolClaude:
		conv = d.claudeID(pids)
	case ToolCodex:
		conv = d.codexID(pids)
	}
	if m := markerRE.FindStringSubmatch(start); m != nil && tool == m[1] && conv == "" {
		// A pane this package restored, whose tool runs but has not
		// registered its conversation yet: it is still the restored one.
		if m[2] != "-" && ValidConversation(m[2]) {
			conv = m[2]
		}
	}
	if conv != "" && !ValidConversation(conv) {
		conv = ""
	}
	return tool, conv
}

// claudeID finds the conversation of a claude process among pids.
func (d *detector) claudeID(pids []int) string {
	in := map[int]bool{}
	for _, p := range pids {
		in[p] = true
	}
	best, bestStart := "", int64(-1)
	for _, e := range d.registry {
		if !in[e.PID] || d.procs.toolOf(e.PID, d.home) != ToolClaude {
			continue
		}
		st := d.procs.procStart(e.PID)
		if s := e.start(); s != "" && s != st {
			continue // a pid reused by another process
		}
		n, _ := strconv.ParseInt(st, 10, 64)
		if best == "" || n < bestStart {
			best, bestStart = e.SessionID, n
		}
	}
	if best != "" {
		return best
	}
	for _, p := range pids { // claude --resume <id> in this pane
		a := d.procs.cmdline(p)
		if len(a) == 0 || d.procs.toolOf(p, d.home) != ToolClaude {
			continue
		}
		for i := 0; i+1 < len(a); i++ {
			if a[i] == "--resume" || a[i] == "-r" {
				if uuidRE.MatchString(a[i+1]) && len(a[i+1]) == 36 {
					return a[i+1]
				}
			}
		}
	}
	return ""
}

// codexID finds the rollout file a codex process among pids has open, the
// newest one when there are several.
func (d *detector) codexID(pids []int) string {
	best, bestMod := "", int64(0)
	for _, p := range pids {
		dir := filepath.Join(d.procs.root, strconv.Itoa(p), "fd")
		ents, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for i, e := range ents {
			if i >= maxFDs {
				break
			}
			target, err := os.Readlink(filepath.Join(dir, e.Name()))
			if err != nil || !strings.Contains(target, "/.codex/sessions/") {
				continue
			}
			base := filepath.Base(target)
			if !strings.HasPrefix(base, "rollout-") {
				continue
			}
			ids := uuidRE.FindAllString(base, -1)
			if len(ids) == 0 {
				continue
			}
			var mod int64
			if fi, err := os.Stat(target); err == nil {
				mod = fi.ModTime().UnixNano()
			}
			if best == "" || mod > bestMod {
				best, bestMod = ids[len(ids)-1], mod
			}
		}
	}
	if best != "" {
		return best
	}
	for _, p := range pids { // codex resume <id> in this pane
		a := d.procs.cmdline(p)
		for i := 0; i+1 < len(a); i++ {
			if a[i] == "resume" && uuidRE.MatchString(a[i+1]) && len(a[i+1]) == 36 && d.procs.toolOf(p, d.home) == ToolCodex {
				return a[i+1]
			}
		}
	}
	return ""
}

// posixShells are the shells whose -l and -c the launch script relies on.
var posixShells = map[string]bool{"bash": true, "sh": true, "zsh": true, "dash": true, "ksh": true, "mksh": true, "ash": true}

// LaunchArgv is the program a restored pane starts with: the tool on its
// conversation through a login shell (so the tool is found on the PATH the
// user's profile sets), and then the login shell, so the pane stays when the
// tool ends. claude --resume <id> or claude --continue (the newest
// conversation of the folder) when the id is unknown; codex resume <id> or
// codex resume --last (newest of the folder). Nil for a pane without a tool:
// the user's shell starts. The script starts with a no-op that names the tool
// and conversation (the marker the next save reads). No key or environment
// value is in it.
func LaunchArgv(tool, conv, shell string) []string {
	if conv != "" && !ValidConversation(conv) {
		conv = ""
	}
	var cmd string
	switch tool {
	case ToolClaude:
		cmd = "claude --continue"
		if conv != "" {
			cmd = "claude --resume '" + conv + "'"
		}
	case ToolCodex:
		cmd = "codex resume --last"
		if conv != "" {
			cmd = "codex resume '" + conv + "'"
		}
	default:
		return nil
	}
	if !filepath.IsAbs(shell) || strings.ContainsAny(shell, "'\x00\n") || !posixShells[filepath.Base(shell)] {
		shell = "/bin/sh"
		if _, err := os.Stat(shell); err != nil {
			shell = "/system/bin/sh" // Android
		}
	}
	mark := conv
	if mark == "" {
		mark = "-"
	}
	return []string{shell, "-lc", ": " + marker + " " + tool + " " + mark + "; " + cmd + "; exec '" + shell + "' -l"}
}
