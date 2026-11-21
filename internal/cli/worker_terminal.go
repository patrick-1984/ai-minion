package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/worker/term/tmux"
)

// checkTerminalHost uses the adapter's own version check. Constructing a host
// runs only -V, so it cannot start or modify a tmux server.
func checkTerminalHost(ctx context.Context, res *config.Resolver, socket string, owner *ownerSetup) error {
	if runtime.GOOS == "windows" {
		return checkPsmux(res)
	}
	var opts []tmux.Option
	managed, err := config.ResolveManagedHost(res)
	if err != nil {
		return err
	}
	if owner == nil {
		opts = tmux.OptionsFor(managed)
	}
	if owner != nil {
		socket = owner.Socket
		opts = append(opts, tmux.WithUserProbe(), tmux.WithExec(func(_ context.Context, args []string, _ []byte) ([]byte, []byte, error) {
			ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			var out bytes.Buffer
			err := runAsUser(ctx, owner.Target, "tmux", args, &out)
			return out.Bytes(), nil, err
		}))
	}
	_, err = tmux.New(socket, opts...)
	return tmuxProblem(err)
}

var errMissingTmux = errors.New("tmux is missing from PATH")

func tmuxProblem(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, exec.ErrNotFound):
		return fmt.Errorf("%w. %s", errMissingTmux, tmuxInstallHint())
	case strings.Contains(err.Error(), "too old"):
		if runtime.GOOS == "darwin" {
			return fmt.Errorf("%w. Update tmux with: brew upgrade tmux", err)
		}
		return fmt.Errorf("%w. Follow the upgrade instructions at https://github.com/patrick-1984/ai-minion/blob/main/docs/install/worker-linux.md#upgrading-an-old-tmux", err)
	case errors.Is(err, os.ErrPermission):
		return fmt.Errorf("cannot execute tmux as the Worker user: %w. Give that user execute permission on the tmux binary and its parent directories", err)
	}
	return err
}

func tmuxInstallHint() string {
	for _, install := range []string{"apt-get install tmux", "dnf install tmux", "yum install tmux", "apk add tmux", "pacman -S tmux", "zypper install tmux", "brew install tmux"} {
		manager, _, _ := strings.Cut(install, " ")
		if _, err := exec.LookPath(manager); err == nil {
			if manager != "brew" && os.Geteuid() != 0 {
				install = "sudo " + install
			}
			return "Run: " + install
		}
	}
	return "Install tmux from https://github.com/tmux/tmux/wiki/Installing"
}

// Honor an explicit ConPTY choice, including a saved decline from releases
// that previously offered psmux on a terminal.
func checkPsmux(res *config.Resolver) error {
	host, err := res.Resolve(config.KeySessionHost)
	if err != nil || host.Value == config.SessionHostConPTY {
		return err
	}
	auto := host.Value == config.SessionHostAuto || host.Value == ""
	bin := tmux.FindPsmux()
	if bin == "" {
		if auto {
			return nil // session_host auto: the Worker uses ConPTY
		}
		return errors.New("psmux is missing (session_host is psmux). Install it with: winget install psmux, or set session_host to auto")
	}
	if _, err := tmux.NewPsmux(bin); err != nil {
		if auto {
			return nil // an unusable psmux: auto falls back to ConPTY, as the Worker does
		}
		return fmt.Errorf("%w. Update psmux with: winget upgrade psmux", err)
	}
	return nil
}
