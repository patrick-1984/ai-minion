package cli

// First-run offer to install psmux on Windows (v0.9.2, A23). A foreground
// Worker started in a console on Windows without psmux asks once whether to
// install it with winget; the answer is saved as psmux_offer, so the question
// never comes back. Yes runs "winget install psmux"; the Worker then hosts
// sessions with psmux (session_host auto). No keeps ConPTY.

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"

	"github.com/spf13/cobra"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/worker/term/tmux"
)

// psmuxOfferDeps are the seams of the offer that tests replace.
type psmuxOfferDeps struct {
	goos        string
	interactive func(in io.Reader, out io.Writer) bool
	findPsmux   func() string
	winget      func(ctx context.Context, out, errOut io.Writer) error
}

func defaultPsmuxOfferDeps() psmuxOfferDeps {
	return psmuxOfferDeps{
		goos:        runtime.GOOS,
		interactive: func(in io.Reader, out io.Writer) bool { return isTerminalStream(in) && isTerminalWriter(out) },
		findPsmux:   tmux.FindPsmux,
		winget: func(ctx context.Context, out, errOut io.Writer) error {
			c := exec.CommandContext(ctx, "winget", "install", "psmux", "--accept-source-agreements", "--accept-package-agreements")
			c.Stdout, c.Stderr = out, errOut
			return c.Run()
		},
	}
}

func isTerminalWriter(w io.Writer) bool {
	f, ok := w.(*os.File)
	return ok && isTerminalStream(f)
}

// offerPsmux asks once, on Windows in a console, whether to install psmux.
// It never fails the start of the Worker: problems are printed and ConPTY is
// used.
func offerPsmux(cmd *cobra.Command, res *config.Resolver, d psmuxOfferDeps) {
	if d.goos != "windows" {
		return
	}
	in, out, errOut := cmd.InOrStdin(), cmd.OutOrStdout(), cmd.ErrOrStderr()
	if !d.interactive(in, out) || d.findPsmux() != "" {
		return
	}
	if r, err := res.Resolve(config.KeySessionHost); err == nil && r.Value == config.SessionHostConPTY {
		return
	}
	if r, err := res.Resolve(config.KeyPsmuxOffer); err != nil || r.Value != "ask" {
		return
	}
	dir, err := res.Resolve(config.KeyStateDir)
	if err != nil || dir.Value == "" {
		return
	}
	_, _ = fmt.Fprint(out, "\npsmux is not installed. With psmux the Worker serves your psmux sessions (tmux for Windows),\n"+
		"and they keep running when the Worker stops or updates. Without it, sessions live inside the Worker.\n"+
		"Install psmux now with winget? [Y/n] ")
	line, _ := bufio.NewReader(in).ReadString('\n')
	answer := strings.ToLower(strings.TrimSpace(line))
	result := "declined"
	if answer == "" || answer == "y" || answer == "yes" {
		_, _ = fmt.Fprintln(out, "Running: winget install psmux")
		if err := d.winget(cmd.Context(), out, errOut); err != nil {
			_, _ = fmt.Fprintf(errOut, "winget did not install psmux (%v); the Worker uses ConPTY. Install it later with `winget install psmux`.\n", err)
			return // asked again next time: the user said yes
		}
		if d.findPsmux() == "" {
			_, _ = fmt.Fprintln(errOut, "psmux was installed but is not found yet; open a new terminal and start the Worker again.")
		} else {
			_, _ = fmt.Fprintln(out, "psmux is installed: the Worker hosts its sessions with psmux.")
		}
		result = "installed"
	} else {
		_, _ = fmt.Fprintln(out, "OK: the Worker uses ConPTY. `minion worker config set psmux_offer ask` asks again.")
	}
	saveWorkerSetting(dir.Value, config.KeyPsmuxOffer, result, errOut)
}

// saveWorkerSetting writes one setting to the Worker's config file, under the
// config lock, and reports a failure on errOut.
func saveWorkerSetting(stateDir, key, value string, errOut io.Writer) {
	if _, err := writeWorkerSettings(stateDir, map[string]string{key: value}); err != nil {
		_, _ = fmt.Fprintf(errOut, "cannot save %s: %v\n", key, err)
	}
}
