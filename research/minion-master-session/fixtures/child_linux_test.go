//go:build a77_native && linux

package fixtures

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strconv"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/sys/unix"
)

type childLaunch struct {
	Name, Root, Claude, Profile, Project string
	Input, Output, Stderr, MCPConfig     string
	ModelPort                            int
	Interactive                          bool
}

func TestFixtureChild(t *testing.T) {
	index := -1
	for i, arg := range os.Args {
		if arg == "--" {
			index = i + 1
			break
		}
	}
	if index < 0 || index == len(os.Args) {
		return
	}
	args := os.Args[index:]
	var err error
	switch args[0] {
	case "launch":
		err = execClaude(args[1])
	case "baseline":
		err = baselineMCP()
	case "master":
		err = masterMCP(args[1])
	case "wrong-peer":
		got, issue := requestBroker(t.Context(), args[1], peerRequest{Kind: "call", ClaimedPane: args[2]})
		err = issue
		if got.Allowed {
			err = fmt.Errorf("the unrelated fixture process unexpectedly acquired access")
		}
	default:
		err = fmt.Errorf("unknown fixture role %q", args[0])
	}
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	// The MCP subprocess must not write Go's test runner banner to stdout.
	os.Exit(0)
}

func findClaude(t *testing.T) string {
	t.Helper()
	bin, err := exec.LookPath("claude")
	must(t, err)
	return bin
}

func execClaude(input string) error {
	raw, err := os.ReadFile(input)
	if err != nil {
		return err
	}
	var cfg childLaunch
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return err
	}
	if !cfg.Interactive {
		// Redirecting all three streams away from this private pane can cause
		// a hangup. Structured stdin EOF still terminates the print session.
		if _, err := unix.Dup(0); err != nil {
			return err
		}
		signal.Ignore(syscall.SIGHUP)
		if err := redirect(cfg.Input, 0, os.O_RDONLY); err != nil {
			return err
		}
		if err := redirect(cfg.Output, 1, os.O_CREATE|os.O_WRONLY|os.O_TRUNC); err != nil {
			return err
		}
		if err := redirect(cfg.Stderr, 2, os.O_CREATE|os.O_WRONLY|os.O_TRUNC); err != nil {
			return err
		}
	}
	if err := os.Chdir(cfg.Project); err != nil {
		return err
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	runtime.LockOSThread() // carry the Linux restrictions into the vendor exec
	if err := isolateFixture(cfg.Root, cfg.Claude, self, cfg.ModelPort); err != nil {
		return fmt.Errorf("refuse to start the vendor CLI without fixture confinement: %w", err)
	}
	args := []string{
		cfg.Claude, "--model", "claude-sonnet-4-6", "--tools", "",
		"--permission-mode", "default",
		"--setting-sources", "user", "--mcp-config", cfg.MCPConfig, "--allowedTools", masterTool,
	}
	env := []string{
		"PATH=/usr/bin:/bin", "NO_COLOR=1", "TMPDIR=" + cfg.Root,
		"XDG_CONFIG_HOME=" + cfg.Root, "XDG_CACHE_HOME=" + cfg.Root, "XDG_STATE_HOME=" + cfg.Root,
		"CLAUDE_CONFIG_DIR=" + cfg.Profile,
		"ANTHROPIC_BASE_URL=http://127.0.0.1:" + strconv.Itoa(cfg.ModelPort),
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1", "DISABLE_UPDATES=1",
		"ENABLE_CLAUDEAI_MCP_SERVERS=false", "ENABLE_TOOL_SEARCH=false",
	}
	if cfg.Interactive {
		env = append(env, "TERM=xterm-256color", "IS_DEMO=1", "ANTHROPIC_AUTH_TOKEN="+dummyKey)
	} else {
		args = append(args, "-p", "--input-format", "stream-json", "--output-format", "stream-json",
			"--verbose", "--max-turns", "4", "--no-session-persistence")
		env = append(env, "TERM=dumb", "ANTHROPIC_API_KEY="+dummyKey)
	}
	return unix.Exec(cfg.Claude, args, env)
}

func redirect(path string, to int, flags int) error {
	f, err := os.OpenFile(path, flags, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	return unix.Dup2(int(f.Fd()), to)
}

func isolateFixture(root, claude, helper string, port int) error {
	version, _, errno := unix.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET, 0, 0, unix.LANDLOCK_CREATE_RULESET_VERSION)
	if errno != 0 {
		return errno
	}
	if version < 4 {
		return fmt.Errorf("Linux Landlock ABI 4 required for fixture TCP confinement")
	}
	const allFile = (unix.LANDLOCK_ACCESS_FS_TRUNCATE << 1) - 1
	const readFile = unix.LANDLOCK_ACCESS_FS_READ_FILE
	const readTree = readFile | unix.LANDLOCK_ACCESS_FS_READ_DIR | unix.LANDLOCK_ACCESS_FS_EXECUTE
	const tcpConnect = unix.LANDLOCK_ACCESS_NET_CONNECT_TCP
	attr := [2]uint64{allFile, tcpConnect | unix.LANDLOCK_ACCESS_NET_BIND_TCP}
	fd, _, errno := unix.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET, uintptr(unsafe.Pointer(&attr[0])), unsafe.Sizeof(attr), 0)
	if errno != 0 {
		return errno
	}
	defer unix.Close(int(fd))
	pathRule := func(path string, access uint64) error {
		f, err := unix.Open(path, unix.O_PATH|unix.O_CLOEXEC, 0)
		if err != nil {
			return err
		}
		defer unix.Close(f)
		var rule [12]byte // struct landlock_path_beneath_attr is packed
		binary.NativeEndian.PutUint64(rule[:8], access)
		binary.NativeEndian.PutUint32(rule[8:], uint32(f))
		_, _, errno := unix.Syscall6(unix.SYS_LANDLOCK_ADD_RULE, fd, 1, uintptr(unsafe.Pointer(&rule[0])), 0, 0, 0)
		if errno != 0 {
			return fmt.Errorf("%s: %w", path, errno)
		}
		return nil
	}
	for _, item := range []struct {
		path   string
		access uint64
	}{
		{root, allFile}, {"/usr", readTree}, {"/proc/self", readTree},
		{claude, readFile | unix.LANDLOCK_ACCESS_FS_EXECUTE},
		{helper, readFile | unix.LANDLOCK_ACCESS_FS_EXECUTE},
		{"/dev/null", readFile | unix.LANDLOCK_ACCESS_FS_WRITE_FILE}, {"/dev/urandom", readFile},
		{"/etc/passwd", readFile}, {"/etc/ld.so.cache", readFile},
	} {
		if err := pathRule(item.path, item.access); err != nil {
			return err
		}
	}
	network := [2]uint64{tcpConnect, uint64(port)}
	if _, _, errno := unix.Syscall6(unix.SYS_LANDLOCK_ADD_RULE, fd, 2, uintptr(unsafe.Pointer(&network[0])), 0, 0, 0); errno != 0 {
		return errno
	}
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return err
	}
	if _, _, errno := unix.Syscall(unix.SYS_LANDLOCK_RESTRICT_SELF, fd, 0, 0); errno != 0 {
		return errno
	}
	return nil
}

func baselineMCP() error {
	server := mcp.NewServer(&mcp.Implementation{Name: "baseline-fixture", Version: "1"}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: "baseline", Description: "An unrelated fixture tool."},
		func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, any, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "baseline"}}}, nil, nil
		})
	return server.Run(context.Background(), &mcp.StdioTransport{})
}

func masterMCP(path string) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var current atomic.Pointer[catalogState]
	current.Store(&catalogState{})
	server := mcp.NewServer(&mcp.Implementation{Name: "minion-fixture", Version: "1"}, &mcp.ServerOptions{HasTools: true})
	server.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			if method != "tools/list" {
				return next(ctx, method, req)
			}
			state := current.Load()
			if state.FailList {
				_, _ = requestBroker(ctx, path, peerRequest{Kind: "list", Revision: state.Revision, ToolCount: -1})
				return nil, errors.New("deliberate fixture refresh failure")
			}
			result, err := next(ctx, method, req)
			if listing, ok := result.(*mcp.ListToolsResult); ok {
				_, _ = requestBroker(ctx, path, peerRequest{Kind: "list", Revision: state.Revision, ToolCount: len(listing.Tools)})
			}
			return result, err
		}
	})
	publish := func(state catalogState) {
		prev := current.Swap(&state)
		if state.Publish {
			mcp.AddTool(server, &mcp.Tool{Name: "master_list", Description: "List permitted fixture Workers."},
				func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
					reply, err := requestBroker(ctx, path, peerRequest{Kind: "call"})
					result := &mcp.CallToolResult{IsError: err != nil || !reply.Allowed}
					if result.IsError {
						result.Content = []mcp.Content{&mcp.TextContent{Text: "fixture master access revoked or unavailable"}}
					} else {
						result.Content = []mcp.Content{&mcp.TextContent{Text: "permitted fixture Worker"}}
					}
					return result, nil, nil
				})
		} else if prev.Publish {
			server.RemoveTools("master_list")
		}
	}
	refresh := func() {
		reply, err := requestBroker(ctx, path, peerRequest{Kind: "status"})
		if err != nil || !reply.Allowed {
			reply.State = catalogState{}
		}
		if *current.Load() == reply.State {
			return
		}
		publish(reply.State)
		_, _ = requestBroker(ctx, path, peerRequest{Kind: "applied", Revision: reply.State.Revision})
	}
	refresh()
	go func() {
		ticker := time.NewTicker(150 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				refresh()
			}
		}
	}()
	return server.Run(ctx, &mcp.StdioTransport{})
}
