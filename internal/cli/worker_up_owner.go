package cli

// The sharing question when "worker up" runs as root for another user (A57).
// Root can see that user's tmux sessions (it connects to their tmux socket,
// read only), so it asks on its terminal like the wizard does. The answer is
// saved by "minion worker config share" started as that user, so root never
// writes into the user's state directory itself. "r" makes the Worker run as
// root instead: on some hosts the sessions run as root on purpose.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/patrick-1984/ai-minion/internal/worker/service"
	"github.com/patrick-1984/ai-minion/internal/worker/term"
	"github.com/patrick-1984/ai-minion/internal/worker/term/tmux"
)

// errRunAsRoot asks "worker up" to start over with the Worker running as root.
var errRunAsRoot = errors.New("run the Worker as root")

// ownerSetup describes the user that "worker up" (as root) sets up a Worker for.
type ownerSetup struct {
	Target   service.Target
	StateDir string
	Socket   string // the target user's tmux socket
	Binary   string // this minion executable
}

// ownerSessions lists the session names in the target user's tmux server.
// An error says, in plain words, why they could not be listed.
func (d workerSetupDeps) ownerSessions(ctx context.Context, socket string) ([]string, error) {
	var l interface {
		ListSessions(context.Context) ([]term.Session, error)
	}
	if d.sessions != nil {
		if l = d.sessions(socket); l == nil {
			return nil, errors.New("tmux cannot be asked")
		}
	} else {
		h, err := tmux.New(socket)
		if err != nil {
			return nil, tmuxProblem(err)
		}
		l = h
	}
	ss, err := l.ListSessions(ctx)
	if err != nil {
		if strings.Contains(err.Error(), "no server running") || strings.Contains(err.Error(), "No such file") ||
			strings.Contains(err.Error(), "error connecting") {
			return nil, nil // no tmux server yet: no sessions
		}
		return nil, err
	}
	names := make([]string, 0, len(ss))
	for _, s := range ss {
		names = append(names, s.Name)
	}
	sort.Strings(names)
	return names, nil
}

// askOwnerShare asks root's terminal what the Hub may see of the target
// user's sessions and saves the answer as that user.
func (d workerSetupDeps) askOwnerShare(cmd *cobra.Command, o *ownerSetup) error {
	ctx := cmdContext(cmd)
	in, out := cmd.InOrStdin(), cmd.OutOrStdout()
	user := o.Target.User
	names, lerr := d.ownerSessions(ctx, o.Socket)
	_, _ = fmt.Fprintf(out, "\nMinion Worker setup\nThe Worker will run as %s and serve the tmux sessions of %s.\n", user, user)
	switch {
	case lerr != nil:
		_, _ = fmt.Fprintf(out, "The sessions of %s could not be listed: %v\n", user, lerr)
	case len(names) == 0:
		_, _ = fmt.Fprintf(out, "%s has no tmux sessions right now.\n", user)
	default:
		_, _ = fmt.Fprintf(out, "%s has %d tmux session(s): %s\n", user, len(names), strings.Join(names, ", "))
	}
	_, _ = fmt.Fprintf(out, "What may the Hub see?\n"+
		"  a  all sessions, also new ones (default)\n"+
		"  n  none\n"+
		"  l  decide later in the Hub (nothing is shared until then)\n"+
		"  r  run the Worker as root instead (serves root's tmux sessions)\n"+
		"Choice [a/n/l/r]: ")
	line, err := readLine(in)
	if err != nil {
		_, _ = fmt.Fprintln(out)
		return errors.New("nothing was started: no answer to what the Hub may see")
	}
	var mode string
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "", "a", "all":
		mode = "all"
	case "n", "none":
		mode = "none"
	case "l", "later":
		mode = "later"
	case "r", "root":
		return errRunAsRoot
	default:
		return fmt.Errorf("nothing was started: %q is not one of a, n, l or r", strings.TrimSpace(line))
	}
	if lerr != nil && mode == "all" {
		// Sharing works without listing; tmux itself must work, though.
		if errors.Is(lerr, errMissingTmux) || strings.Contains(lerr.Error(), "too old") {
			return fmt.Errorf("nothing was started: %w", lerr)
		}
	}
	return d.saveOwnerShare(ctx, o, mode, out)
}

func (d workerSetupDeps) saveOwnerShare(ctx context.Context, o *ownerSetup, mode string, out io.Writer) error {
	run := d.runAsUser
	if run == nil {
		run = runAsUser
	}
	if err := run(ctx, o.Target, o.Binary, []string{"worker", "config", "share", mode, "--state-dir", o.StateDir}, out); err != nil {
		return fmt.Errorf("nothing was started: saving the choice as %s failed: %w", o.Target.User, err)
	}
	return nil
}

// readLine reads one line byte by byte, so nothing after it is consumed.
func readLine(in io.Reader) (string, error) {
	var b strings.Builder
	buf := make([]byte, 1)
	for {
		n, err := in.Read(buf)
		if n == 1 {
			if buf[0] == '\n' {
				return b.String(), nil
			}
			b.WriteByte(buf[0])
		}
		if err != nil {
			if b.Len() > 0 {
				return b.String(), nil
			}
			return "", err
		}
	}
}
