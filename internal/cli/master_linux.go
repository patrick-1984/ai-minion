//go:build linux && !android

package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/sys/unix"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/mcp"
	"github.com/patrick-1984/ai-minion/internal/version"
	"github.com/patrick-1984/ai-minion/internal/worker/localapi"
)

func runMasterClaude(cmd *cobra.Command, args []string) error {
	if len(args) > 0 && (args[0] == "--help" || args[0] == "-h") {
		return cmd.Help()
	}
	if len(args) > 0 && args[0] == "--" {
		args = args[1:]
	}
	for _, arg := range args {
		name, _, _ := strings.Cut(arg, "=")
		switch name {
		case "--", "--mcp-config", "--strict-mcp-config", "--bare", "--safe-mode", "--tmux", "--background", "--bg":
			return fmt.Errorf("%s is incompatible with a prepared Minion master launch", name)
		}
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	self, err = filepath.EvalSymlinks(self)
	if err != nil {
		return err
	}
	claude, err := exec.LookPath("claude")
	if err != nil {
		return errors.New("install Claude Code 2.1.330 on this Linux host first")
	}
	claude, err = filepath.EvalSymlinks(claude)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), 5*time.Second)
	defer cancel()
	binVersion, err := exec.CommandContext(ctx, claude, "--version").Output()
	if err != nil || strings.TrimSpace(string(binVersion)) != "2.1.330 (Claude Code)" {
		return errors.New("only Claude Code 2.1.330 is verified for this Minion master launch")
	}
	paths, err := localapi.MasterSocketPaths(config.SystemPathEnv())
	if err != nil {
		return err
	}
	socket, err := registerMasterLaunch(ctx, paths, claude)
	if err != nil {
		return err
	}
	argv, err := masterClaudeArgs(claude, self, socket, args)
	if err != nil {
		return err
	}
	return unix.Exec(claude, argv, os.Environ())
}

func registerMasterLaunch(ctx context.Context, paths []string, executable string) (string, error) {
	for _, path := range paths {
		_, err := localapi.CallMaster(ctx, path, localapi.MasterRequest{Kind: localapi.MasterRegister, Executable: executable})
		if err == nil {
			return path, nil
		}
		var opErr *net.OpError
		if !errors.Is(err, os.ErrNotExist) && !(errors.As(err, &opErr) && opErr.Op == "dial" && errors.Is(err, unix.ECONNREFUSED)) {
			return "", err
		}
	}
	return "", errors.New("this Worker has no master attachment socket. Use an updated Linux 6.5+ tmux Worker with Unix peer pidfd support")
}

func masterClaudeArgs(claude, self, socket string, args []string) ([]string, error) {
	config, err := json.Marshal(map[string]any{"mcpServers": map[string]any{
		"minion_master": map[string]any{"type": "stdio", "command": self, "args": []string{"master", "mcp", socket}},
	}})
	if err != nil {
		return nil, err
	}
	argv := append([]string{claude}, args...)
	return append(argv, "--mcp-config", string(config)), nil
}

func runMasterMCP(cmd *cobra.Command, args []string) error {
	socket := args[0]
	if !filepath.IsAbs(socket) || filepath.Base(socket) != "master.sock" {
		return errors.New("invalid Minion master socket")
	}
	paths, err := localapi.MasterSocketPaths(config.SystemPathEnv())
	if err != nil {
		return err
	}
	if !slices.Contains(paths, socket) {
		return errors.New("the Minion master socket is outside this user's Worker runtime directory")
	}
	call := func(ctx context.Context, req localapi.MasterRequest) (localapi.MasterResponse, error) {
		return localapi.CallMaster(ctx, socket, req)
	}
	return mcp.ServeMaster(cmd.Context(), cmd.InOrStdin(), cmd.OutOrStdout(), version.Get().Version, mcp.MasterBackend{
		Generation: func(ctx context.Context) (uint64, error) {
			ctx, cancel := context.WithTimeout(ctx, time.Second)
			defer cancel()
			state, err := call(ctx, localapi.MasterRequest{Kind: localapi.MasterStatus})
			return state.Generation, err
		},
		Catalog: func(ctx context.Context, gen uint64, state string) {
			ctx, cancel := context.WithTimeout(ctx, time.Second)
			defer cancel()
			_, _ = call(ctx, localapi.MasterRequest{Kind: localapi.MasterCatalog, Generation: gen, Report: state})
		},
		Call: func(ctx context.Context, gen uint64, op, id string, args json.RawMessage) (json.RawMessage, error) {
			res, err := call(ctx, localapi.MasterRequest{Kind: localapi.MasterCall,
				Generation: gen, Operation: op, RequestID: id, Arguments: args})
			return res.Result, err
		},
	})
}
