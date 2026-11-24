package cli

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/patrick-1984/ai-minion/internal/client"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/worker/agent"
	"github.com/patrick-1984/ai-minion/internal/worker/agentdetect"
)

// harnessPaneProcess binds a hook to the live Worker's terminal. The inherited
// tmux variables nominate a pane. The local Worker, tmux and the host's process
// table each confirm a different part of the binding.
func harnessPaneProcess(ctx context.Context, lookup func(string) (string, bool), stateDir, harness string) (string, string, int64, int, string) {
	return bindHarnessPane(ctx, lookup, stateDir, harness, client.Local(client.WithDialTimeout(150*time.Millisecond)).Call)
}

func bindHarnessPane(ctx context.Context, lookup func(string) (string, bool), stateDir, harness string, call func(context.Context, string, any) (json.RawMessage, error)) (string, string, int64, int, string) {
	if (runtime.GOOS != "linux" && runtime.GOOS != "android" && runtime.GOOS != "darwin") ||
		lookup == nil || !filepath.IsAbs(stateDir) || (harness != "claude" && harness != "codex") {
		return "", "", 0, 0, ""
	}
	pane, _ := lookup("TMUX_PANE")
	tmuxEnv, _ := lookup("TMUX")
	socket, serverPID := hookTmuxTarget(tmuxEnv)
	if socket == "" || !claudeUsagePane.MatchString(pane) {
		return "", "", 0, 0, ""
	}

	ctx, cancel := context.WithTimeout(ctx, 350*time.Millisecond)
	defer cancel()
	raw, err := call(ctx, agent.OpLocalStatus, nil)
	if err != nil {
		return "", "", 0, 0, ""
	}
	var worker agent.LiveStatusReply
	if json.Unmarshal(raw, &worker) != nil || !filepath.IsAbs(worker.StateDir) ||
		!sameDir(worker.StateDir, stateDir) || !filepath.IsAbs(worker.TmuxSocket) {
		return "", "", 0, 0, ""
	}
	workerSocket, err := os.Stat(worker.TmuxSocket)
	if err != nil || workerSocket.Mode()&os.ModeSocket == 0 || !isSameSocket(socket, workerSocket) {
		return "", "", 0, 0, ""
	}
	target := hookTmuxPane(ctx, worker.TmuxBinary, worker.TmuxSocket, pane, serverPID)
	if target.pid == 0 || !isSameSocket(worker.TmuxSocket, workerSocket) {
		return "", "", 0, 0, ""
	}
	var processes []agentdetect.Proc
	if runtime.GOOS == "darwin" {
		processes, err = (agentdetect.PSSource{}).Processes(ctx)
	} else {
		processes, err = (agentdetect.ProcSource{}).Ancestors(ctx, os.Getpid(), target.pid)
	}
	if err != nil {
		return "", "", 0, 0, ""
	}
	pid := hookHarnessAncestor(processes, os.Getpid(), target.pid, serverPID, harness)
	if pid == 0 {
		return "", "", 0, 0, ""
	}
	birth := agentdetect.ProcessBirth(pid)
	if birth == "" || ctx.Err() != nil {
		return "", "", 0, 0, ""
	}
	return target.session, pane, target.created, pid, birth
}

// TMUX has the form socket,server pid,session number. Parse from the right
// since a valid socket pathname can itself contain commas.
func hookTmuxTarget(value string) (string, int) {
	server, sessionNumber, ok := cutLastComma(value)
	if !ok {
		return "", 0
	}
	socket, serverPID, ok := cutLastComma(server)
	pid, pidErr := strconv.Atoi(serverPID)
	number, numberErr := strconv.Atoi(sessionNumber)
	if !ok || pidErr != nil || numberErr != nil || pid <= 0 || number < 0 || !filepath.IsAbs(socket) {
		return "", 0
	}
	return socket, pid
}

func cutLastComma(value string) (string, string, bool) {
	index := strings.LastIndexByte(value, ',')
	if index < 0 {
		return "", "", false
	}
	return value[:index], value[index+1:], true
}

func isSameSocket(path string, want os.FileInfo) bool {
	got, err := os.Stat(path)
	return err == nil && os.SameFile(got, want)
}

type hookPaneTarget struct {
	session string
	pid     int
	created int64
}

const hookPaneFormat = "#{pane_id}\t#{pid}\t#{pane_pid}\t#{pane_dead}\t#{window_linked}\t#{session_created}\t#{session_name}"

func hookTmuxPane(ctx context.Context, binary, socket, pane string, serverPID int) hookPaneTarget {
	if binary == "" {
		var err error
		binary, err = exec.LookPath("tmux")
		if err != nil {
			return hookPaneTarget{}
		}
	}
	if !filepath.IsAbs(binary) {
		return hookPaneTarget{}
	}
	cmd := exec.CommandContext(ctx, binary, "-S", socket, "-u", "display-message", "-p", "-t", pane, hookPaneFormat)
	cmd.WaitDelay = 100 * time.Millisecond
	var out hookPaneOutput
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return hookPaneTarget{}
	}
	return parseHookPane(string(out.data[:out.size]), pane, serverPID)
}

type hookPaneOutput struct {
	data [1024]byte
	size int
}

func (o *hookPaneOutput) Write(data []byte) (int, error) {
	n := copy(o.data[o.size:], data)
	o.size += n
	if n < len(data) {
		return n, io.ErrShortWrite
	}
	return n, nil
}

func parseHookPane(line, pane string, serverPID int) hookPaneTarget {
	fields := strings.Split(strings.TrimSuffix(line, "\n"), "\t")
	// A linked window has multiple session presentations. A pane ID alone
	// cannot identify which one should receive the observation.
	if len(fields) != 7 || fields[0] != pane || fields[1] != strconv.Itoa(serverPID) ||
		fields[3] != "0" || fields[4] != "0" || !ops.ValidSessionName(fields[6]) {
		return hookPaneTarget{}
	}
	pid, pidErr := strconv.Atoi(fields[2])
	created, createdErr := strconv.ParseInt(fields[5], 10, 64)
	if pidErr != nil || createdErr != nil || pid <= 0 || created <= 0 {
		return hookPaneTarget{}
	}
	return hookPaneTarget{session: fields[6], pid: pid, created: created}
}

func hookHarnessAncestor(processes []agentdetect.Proc, hookPID, panePID, serverPID int, harness string) int {
	byPID := make(map[int]agentdetect.Proc, len(processes))
	for _, p := range processes {
		byPID[p.PID] = p
	}
	root, ok := byPID[panePID]
	if !ok || root.PPID != serverPID || root.TTY == "" || root.TPGID <= 0 || !root.Running() {
		return 0
	}
	match := 0
	pid := byPID[hookPID].PPID
	for depth := 0; depth < 64 && pid > 1; depth++ {
		p, ok := byPID[pid]
		if !ok {
			break
		}
		if name := agentdetect.ProcessHarness(p.Args); name != "" && match == 0 {
			if name != harness || !p.Running() || p.TTY != root.TTY || p.TPGID != root.TPGID || !p.Foreground() {
				return 0
			}
			match = p.PID
		}
		if pid == panePID {
			return match
		}
		pid = p.PPID
	}
	return 0
}
