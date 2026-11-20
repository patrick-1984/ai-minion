package cli

// minion worker config share list: the names-only listing behind the session
// picker of the tray. It reads the Worker state read-only and asks the
// terminal host for its sessions itself, so it also lists the sessions that
// the Hub's sessions_list leaves out (they are not shared), and it works with
// a Worker of any version, running or not.

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/worker/store"
)

// shareListSession is one session of the host.
type shareListSession struct {
	Name   string `json:"name"`
	Shared bool   `json:"shared"`
}

// shareListReply is the JSON of "share list". It holds names only: no pane,
// content or command of any session.
type shareListReply struct {
	Mode       string `json:"mode"`
	Configured bool   `json:"configured"`
	// Revision is the share revision to give back as --revision when saving;
	// empty for a state that never saw a change.
	Revision string `json:"revision"`
	// Locked is share_lock: the Hub cannot change the choice (this command
	// still can).
	Locked bool `json:"locked"`
	// Selected are the saved names (kept for every mode, used by select),
	// also those of sessions that do not exist now.
	Selected []string `json:"selected"`
	// Sessions are the sessions of this host, sorted by name; SessionsKnown is
	// false when they cannot be listed from here.
	Sessions      []shareListSession `json:"sessions"`
	SessionsKnown bool               `json:"sessions_known"`
}

func runConfigShareList(cmd *cobra.Command, deps workerConfigDeps, asJSON bool) error {
	ce, err := deps.env(cmd)
	if err != nil {
		return err
	}
	res := &config.Resolver{
		Role: config.ScopeWorker, Registry: deps.reg(), File: ce.file,
		Flags: changedFlags{cmd: cmd, reg: deps.reg()}, LookupEnv: deps.lookupEnv,
	}
	core, cerr := config.ResolveWorkerCore(res)
	stateDir, out := ce.stateDir, ce.out
	ce.release() // nothing below writes
	if cerr != nil {
		return cerr
	}

	state := store.ShareState{ShareSelection: store.ShareSelection{Mode: store.ShareLater, Sessions: []string{}}}
	if _, err := os.Stat(filepath.Join(stateDir, store.DBFile)); err == nil {
		st, err := store.OpenReadOnlyContext(cmd.Context(), stateDir)
		if err != nil {
			return fmt.Errorf("open the Worker state in %s: %w", stateDir, err)
		}
		state, err = st.LoadShareStateContext(cmd.Context())
		_ = st.Close()
		if err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}

	reply := shareListReply{
		Mode: string(state.Mode), Configured: state.Configured, Revision: ops.FormatShareRevision(state.Revision),
		Locked: core.ShareLock, Selected: slices.Clone(state.Sessions), Sessions: []shareListSession{},
	}
	// Only a host that is certainly the Worker's is listed: a socket saved in
	// the config file (the service reads the same one) or the default one when
	// this shell does not move it with TMUX_TMPDIR (the service would not
	// know). Otherwise the same session name could belong to another tmux
	// server, and the picker would check the wrong session: no inventory.
	if deps.sessions != nil && core.TmuxSocket != "" && hostIsTheWorkers(res, deps.lookupEnv) {
		if h := deps.sessions(core.TmuxSocket); h != nil {
			all, err := h.ListSessions(cmd.Context())
			if err != nil {
				return fmt.Errorf("cannot list the sessions of this host: %w", err)
			}
			reply.SessionsKnown = true
			for _, s := range all {
				reply.Sessions = append(reply.Sessions, shareListSession{Name: s.Name, Shared: state.Includes(s.Name)})
			}
			sort.Slice(reply.Sessions, func(i, j int) bool { return reply.Sessions[i].Name < reply.Sessions[j].Name })
		}
	}
	if asJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(reply)
	}
	return writeShareList(out, reply)
}

func writeShareList(out interface{ Write([]byte) (int, error) }, r shareListReply) error {
	var b strings.Builder
	fmt.Fprintf(&b, "Share mode: %s (revision %s)\n", r.Mode, orNone(r.Revision))
	if r.Locked {
		b.WriteString("share_lock is on: the Hub cannot change this choice; you can, here.\n")
	}
	if !r.SessionsKnown {
		b.WriteString("The sessions of this host cannot be listed from here; the saved selection is: " + orNone(strings.Join(r.Selected, ", ")) + "\n")
	}
	for _, s := range r.Sessions {
		mark := "[ ]"
		if s.Shared {
			mark = "[x]"
		}
		fmt.Fprintf(&b, "%s %s\n", mark, s.Name)
	}
	_, err := out.Write([]byte(b.String()))
	return err
}

func orNone(v string) string {
	if v == "" {
		return "none"
	}
	return v
}

// hostIsTheWorkers reports whether the tmux socket this command resolves is
// the one the Worker uses.
func hostIsTheWorkers(res *config.Resolver, lookupEnv func(string) (string, bool)) bool {
	r, err := res.Resolve(config.KeyTmuxSocket)
	if err != nil {
		return false
	}
	switch r.Source {
	case config.SourceFile:
		return true
	case config.SourceDefault:
		v, _ := lookupEnv("TMUX_TMPDIR")
		return v == ""
	}
	return false
}
