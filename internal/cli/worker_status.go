package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/spf13/cobra"

	"github.com/patrick-1984/ai-minion/internal/client"
	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/version"
	"github.com/patrick-1984/ai-minion/internal/worker/agent"
	"github.com/patrick-1984/ai-minion/internal/worker/store"
	"github.com/patrick-1984/ai-minion/internal/worker/term"
	"github.com/patrick-1984/ai-minion/internal/worker/term/tmux"
)

func init() {
	Register(func(root *cobra.Command) {
		worker := workerCommand(root)
		worker.AddCommand(newWorkerStatusCmd(defaultWorkerStatusDeps()))
	})
}

// workerStatusDeps are the inputs of "minion worker status" that tests replace.
type workerStatusDeps struct {
	lookupEnv  func(string) (string, bool)
	loadConfig func(stateDir string) (config.FileSource, error)
	version    func() string
	// live returns the source that asks a running Worker over its local
	// socket (AD-22). It never returns nil; the default asks the real socket.
	live func(stateDir string) agent.LiveSource
	// sessions returns the session lister for the tmux socket; nil or a nil
	// result means "cannot count".
	sessions func(socket string) agent.StatusSessions
}

func defaultWorkerStatusDeps() workerStatusDeps {
	return workerStatusDeps{
		lookupEnv:  os.LookupEnv,
		loadConfig: loadWorkerConfig,
		version:    func() string { return version.Version },
		live: func(stateDir string) agent.LiveSource {
			return localLive{stateDir: stateDir, backend: client.Local()}
		},
		sessions: tmuxSessions,
	}
}

// liveStatusTimeout bounds the question to a running Worker, so that status
// still answers when a Worker hangs.
const liveStatusTimeout = 5 * time.Second

// localLive asks the running Worker of this user for its live status over the
// local control socket (AD-22).
type localLive struct {
	// stateDir is the state directory this status is about; an answer from a
	// Worker that uses another one is not about this Worker.
	stateDir string
	backend  client.Backend
}

// LiveStatus implements [agent.LiveSource]. It returns [agent.ErrNoLiveWorker]
// when no Worker runs, or when the one that runs uses another state directory.
// Any other failure (an untrusted socket, a broken answer) is returned as is,
// and status reports it as a problem.
func (l localLive) LiveStatus(ctx context.Context) (agent.LiveStatus, error) {
	ctx, cancel := context.WithTimeout(ctx, liveStatusTimeout)
	defer cancel()
	raw, err := l.backend.Call(ctx, agent.OpLocalStatus, nil)
	if err != nil {
		if errors.Is(err, client.ErrWorkerNotRunning) {
			return agent.LiveStatus{}, agent.ErrNoLiveWorker
		}
		return agent.LiveStatus{}, err
	}
	var reply agent.LiveStatusReply
	if err := json.Unmarshal(raw, &reply); err != nil {
		return agent.LiveStatus{}, fmt.Errorf("the Worker sent a status that cannot be read: %w", err)
	}
	if reply.StateDir != "" && !sameDir(reply.StateDir, l.stateDir) {
		return agent.LiveStatus{}, agent.ErrNoLiveWorker
	}
	return reply.LiveStatus()
}

// sameDir reports whether a and b name the same directory, following symbolic
// links where both exist.
func sameDir(a, b string) bool {
	if filepath.Clean(a) == filepath.Clean(b) {
		return true
	}
	ia, errA := os.Stat(a)
	ib, errB := os.Stat(b)
	return errA == nil && errB == nil && os.SameFile(ia, ib)
}

// tmuxSessions lists the sessions of the tmux server on socket. It does not
// start a server. An empty socket (Windows, where ConPTY hosts the sessions
// inside the Worker) or a missing tmux means "cannot count".
func tmuxSessions(socket string) agent.StatusSessions {
	if socket == "" {
		return nil
	}
	h, err := tmux.New(socket)
	if err != nil {
		return failingSessions{err: err}
	}
	return h
}

// failingSessions reports why the sessions could not be counted.
type failingSessions struct{ err error }

func (f failingSessions) ListSessions(context.Context) ([]term.Session, error) { return nil, f.err }

func newWorkerStatusCmd(deps workerStatusDeps) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show the state of this Worker",
		Long: `Show the state of this Worker: version, mode (Solo or Hub), Hub URL,
connection state and last error, pinned Hub fingerprint, Worker fingerprint,
owner, pending pairing code, share mode, preview-only flag, tmux socket and the
number of tmux sessions.

Status reads the Worker's state directory, so it works whether or not the
Worker is running. The connection state, the last error and the pairing code
are live data: status asks the running Worker for them over its local socket.
When no Worker answers (or it cannot tell, as for the pairing code), status
prints "unknown" for them.

--json prints the same facts as JSON.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runWorkerStatus(cmd, deps, asJSON)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the status as JSON")
	addNamedSettingFlags(cmd, config.Default, config.KeyStateDir, config.KeyTmuxSocket, config.KeyServer)
	return cmd
}

func runWorkerStatus(cmd *cobra.Command, deps workerStatusDeps, asJSON bool) error {
	res := &config.Resolver{
		Role:      config.ScopeWorker,
		Flags:     changedFlags{cmd: cmd, reg: config.Default},
		LookupEnv: deps.lookupEnv,
	}
	if deps.loadConfig != nil {
		file, err := loadConfigFile(res, deps.loadConfig)
		if err != nil {
			return err
		}
		res.File = file
	}
	core, err := config.ResolveWorkerCore(res)
	if err != nil {
		return err
	}
	hub, err := config.ResolveWorkerHub(res)
	if err != nil {
		return err
	}
	in := agent.StatusInputs{
		Version:     deps.version(),
		Name:        core.Name,
		HubURL:      hub.Server,
		TmuxSocket:  core.TmuxSocket,
		PreviewOnly: core.PreviewOnly,
		Platform:    runtime.GOOS,
		Live:        deps.live(core.StateDir),
	}
	if deps.sessions != nil {
		if s := deps.sessions(core.TmuxSocket); s != nil {
			in.Sessions = s
		}
	}

	// Without a database nothing was ever stored: the Worker never started.
	// Opening one here would create the state directory.
	var problems []string
	enr := agent.NewEnrollments(nil, core.StateDir)
	in.Keys = enr
	if _, err := os.Stat(filepath.Join(core.StateDir, store.DBFile)); err == nil {
		st, err := store.OpenContext(cmd.Context(), core.StateDir)
		if err != nil {
			problems = append(problems, fmt.Sprintf("cannot open the Worker state in %s: %v", core.StateDir, err))
		} else {
			defer func() { _ = st.Close() }()
			enr = agent.NewEnrollments(st, core.StateDir)
			in.Enrollment, in.Keys, in.Share = enr, enr, st
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		problems = append(problems, fmt.Sprintf("cannot look at the Worker state in %s: %v", core.StateDir, err))
	}

	report := agent.BuildStatus(cmd.Context(), in)
	report.Problems = append(problems, report.Problems...)
	if asJSON {
		return report.WriteJSON(cmd.OutOrStdout())
	}
	return report.WriteText(cmd.OutOrStdout())
}

// addNamedSettingFlags adds the flags of the named settings only. Commands
// that do not use every Worker setting use it instead of addSettingFlags, so
// that a flag is never accepted and then ignored.
func addNamedSettingFlags(cmd *cobra.Command, reg *config.Registry, keys ...string) {
	for _, key := range keys {
		s, ok := reg.Lookup(key)
		if !ok || !s.HasFlag() || cmd.Flags().Lookup(s.FlagName()) != nil {
			continue
		}
		help := s.Help
		if help == "" {
			help = s.Key
		}
		cmd.Flags().String(s.FlagName(), "", fmt.Sprintf("%s (env %s)", help, s.EnvName()))
	}
}
