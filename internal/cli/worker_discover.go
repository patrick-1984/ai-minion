package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/worker/agent"
	"github.com/patrick-1984/ai-minion/internal/worker/presence"
)

func init() {
	Register(func(root *cobra.Command) {
		worker := workerCommand(root)
		worker.AddCommand(newWorkerDiscoverCmd(defaultWorkerDiscoverDeps()))
	})
}

// Exit statuses of "minion worker discover".
const (
	// DiscoverRunning: at least one Worker is running.
	DiscoverRunning = 0
	// DiscoverNone: no Worker is running (stopped, stale or none found).
	DiscoverNone = 1
	// DiscoverError: the search itself failed, or nothing runs and some
	// record could not be trusted, so a running Worker may be hidden.
	DiscoverError = 2
)

// discoverSchema names the JSON output of the command.
const discoverSchema = "minion.worker-discovery/1"

type workerDiscoverDeps struct {
	lookupEnv  func(string) (string, bool)
	loadConfig func(stateDir string) (config.FileSource, error)
	// reader builds the verified reader; stateDir is the selected state
	// directory (flag, environment, config), empty when none.
	reader func(stateDir string) presence.Reader
	// live asks the selected profile's running Worker over its local socket.
	live func(stateDir string) agent.LiveSource
}

func defaultWorkerDiscoverDeps() workerDiscoverDeps {
	d := defaultWorkerStatusDeps()
	return workerDiscoverDeps{
		lookupEnv:  os.LookupEnv,
		loadConfig: loadWorkerConfig,
		reader:     func(stateDir string) presence.Reader { return presence.NewReader(stateDir) },
		live:       d.live,
	}
}

// discoverOutput is the JSON document of the command.
type discoverOutput struct {
	Schema  string           `json:"schema"`
	Workers []presence.Found `json:"workers"`
	// Note explains an empty or doubtful result.
	Note string `json:"note,omitempty"`
}

func newWorkerDiscoverCmd(deps workerDiscoverDeps) *cobra.Command {
	var asJSON, withUsage bool
	cmd := &cobra.Command{
		Use:   "discover",
		Short: "Find every Worker this account can see, and read its usage meters",
		Long: `Find every Worker this account can see on this computer, not only the one of the
selected profile ("minion worker status" answers for that one).

Discovery reads the presence records the Workers publish, with the same checks
for each: the private records of this account (including the Workers that use
another state directory, which leave a pointer in the default place) and, on
Linux and macOS, the public records of root Workers. A record counts only when
the file has the expected owner and mode, is a regular file, fits its size
limit, matches its schema and is not dated in the future.

Each Worker is reported as running (updated within its stale_after_s, 90 s by
default), stale (says running but stopped updating: a crash or a hung daemon),
stopped (a clean stop) or unavailable (a record that could not be trusted, with
the reason). A Worker of a release without presence records is reported from the
local socket of the selected profile when it answers.

--usage adds the usage-meters document the Worker stored (schema
minion.usage-meters/1), when there is one. --json prints one document with a
"workers" array.

Exit status: 0 at least one Worker is running, 1 none is running, 2 the search
failed, or none runs and a record could not be trusted (a running Worker may be
hidden).`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runWorkerDiscover(cmd, deps, asJSON, withUsage)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the result as JSON")
	cmd.Flags().BoolVar(&withUsage, "usage", false, "include the stored usage-meters document of each Worker")
	addNamedSettingFlags(cmd, config.Default, config.KeyStateDir)
	return cmd
}

func runWorkerDiscover(cmd *cobra.Command, deps workerDiscoverDeps, asJSON, withUsage bool) error {
	out := cmd.OutOrStdout()
	stateDir, err := deps.stateDir(changedFlags{cmd: cmd, reg: config.Default})
	if err != nil {
		return &exitCodeError{discoverFail(cmd, err)}
	}
	res := deps.reader(stateDir).Discover(withUsage)
	doc := discoverOutput{Schema: discoverSchema, Workers: res}
	if res == nil {
		doc.Workers = []presence.Found{}
	}
	if !anyRunning(doc.Workers) && deps.live != nil && stateDir != "" {
		// An older daemon publishes no record but answers on its socket.
		ctx, cancel := context.WithTimeout(cmdContext(cmd), liveStatusTimeout)
		ls, lerr := deps.live(stateDir).LiveStatus(ctx)
		cancel()
		if lerr == nil {
			f := presence.Found{Status: presence.StatusRunning, Source: "socket"}
			if ls.Daemon != nil {
				f.Instance, f.DaemonVersion, f.PID = ls.Daemon.Instance, ls.Daemon.Version, ls.Daemon.PID
			}
			doc.Workers = append([]presence.Found{f}, doc.Workers...)
			doc.Note = "a running Worker without a presence record (an older release) answered on its local socket"
		}
	}
	if len(doc.Workers) == 0 {
		doc.Note = "no presence record found; a Worker of an older release publishes none, so also try `minion worker status`"
	}
	if asJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		if err := enc.Encode(doc); err != nil {
			return err
		}
	} else {
		writeDiscoverText(out, doc, withUsage)
	}
	switch {
	case anyRunning(doc.Workers):
		return nil
	case anyUnavailable(doc.Workers):
		return &exitCodeError{DiscoverError}
	}
	return &exitCodeError{DiscoverNone}
}

func discoverFail(cmd *cobra.Command, err error) int {
	_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "worker discover: %v\n", err)
	return DiscoverError
}

// stateDir resolves the explicitly selected state directory (flag, then
// environment, then the config file); empty means "the defaults only".
func (deps workerDiscoverDeps) stateDir(flags config.FlagSource) (string, error) {
	r := &config.Resolver{Role: config.ScopeWorker, Flags: flags, LookupEnv: deps.lookupEnv}
	if deps.loadConfig != nil {
		file, err := loadConfigFile(r, deps.loadConfig)
		if err != nil {
			return "", err
		}
		r.File = file
	}
	core, err := config.ResolveWorkerCore(r)
	if err != nil {
		return "", err
	}
	return core.StateDir, nil
}

func anyRunning(ws []presence.Found) bool {
	for _, w := range ws {
		if w.Status == presence.StatusRunning {
			return true
		}
	}
	return false
}

func anyUnavailable(ws []presence.Found) bool {
	for _, w := range ws {
		if w.Status == presence.StatusUnavailable {
			return true
		}
	}
	return false
}

func writeDiscoverText(w io.Writer, doc discoverOutput, withUsage bool) {
	if len(doc.Workers) == 0 {
		_, _ = fmt.Fprintln(w, doc.Note)
		return
	}
	for _, f := range doc.Workers {
		id := f.Instance
		if id == "" {
			id = "-"
		}
		line := fmt.Sprintf("%-11s %s (%s)", f.Status, id, f.Source)
		if f.DaemonVersion != "" {
			line += " version " + f.DaemonVersion
		}
		if f.OSScope != "" {
			line += ", " + f.OSScope + " scope"
		}
		if f.UpdatedAgeS != nil {
			line += ", updated " + (time.Duration(*f.UpdatedAgeS) * time.Second).String() + " ago"
		}
		if f.StartedAgeS != nil && f.Status == presence.StatusRunning {
			line += ", up " + (time.Duration(*f.StartedAgeS) * time.Second).String()
		}
		if f.HubOrigin != "" {
			line += ", Hub " + f.HubOrigin
		}
		if f.Error != "" {
			line += ": " + f.Error
		}
		_, _ = fmt.Fprintln(w, line)
		if withUsage && len(f.Usage) > 0 {
			_, _ = fmt.Fprintln(w, "  usage: "+strings.TrimSpace(string(f.Usage)))
		}
	}
	if doc.Note != "" {
		_, _ = fmt.Fprintln(w, doc.Note)
	}
}
