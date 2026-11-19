package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/patrick-1984/ai-minion/internal/client"
	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/worker/agent"
	"github.com/patrick-1984/ai-minion/internal/worker/localapi"
	"github.com/patrick-1984/ai-minion/internal/worker/store"
)

func init() {
	Register(func(root *cobra.Command) {
		workerCommand(root).AddCommand(newWorkerShareCmd(defaultWorkerShareDeps()))
	})
}

// workerShareDeps are the inputs of "minion worker share" that tests replace.
type workerShareDeps struct {
	lookupEnv  func(string) (string, bool)
	loadConfig func(stateDir string) (config.FileSource, error)
	lockState  func(stateDir string) (io.Closer, error)
	lockConfig func(path string) (func(), error)
	// openStore opens the Worker database without migrating it.
	openStore func(ctx context.Context, stateDir string) (*store.Store, error)
	// sessions lists the sessions offline (tmux); nil: not known.
	sessions func(socket string) agent.StatusSessions
	call     func(ctx context.Context, op string, args any) (json.RawMessage, error)
	// beforeCommit runs offline between the config file change and the
	// database commit (tests inject a crash).
	beforeCommit func() error
}

func defaultWorkerShareDeps() workerShareDeps {
	return workerShareDeps{
		lookupEnv:  os.LookupEnv,
		loadConfig: loadWorkerConfig,
		lockState:  agent.LockLifecycle,
		lockConfig: lockConfigFile,
		openStore: func(ctx context.Context, dir string) (*store.Store, error) {
			return store.OpenContext(ctx, dir, store.NoMigrate())
		},
		sessions: tmuxSessions,
		call: func(ctx context.Context, op string, args any) (json.RawMessage, error) {
			return client.Local().Call(ctx, op, args)
		},
	}
}

func newWorkerShareCmd(deps workerShareDeps) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "share",
		Short: "List and change the sessions shared with the Hub",
		Long: `List and change which sessions of this Worker are shared with the Hub.

  minion worker share list
  minion worker share add <session>...
  minion worker share remove <session>...
  minion worker share set all|none|later
  minion worker share set select [<session>...]

A running Worker applies the change itself; without one the change is saved in
the state directory. --if-revision R refuses the change when the selection is
no longer at revision R (exit 27; minion worker share list shows it).
"set select" with no names shares nothing. These commands never write
share_existing into the config file; when the file holds it, they remove it, so
that the next start does not undo the change. When share_existing comes from
the environment or a flag of the Worker, the Worker applies it again at its
next start: the change is saved and the command exits 22.

--json prints one JSON object with ok, code, message and the selection.`,
	}
	var ifRev uint64
	var asJSON bool
	add := func(c *cobra.Command, withRev bool) *cobra.Command {
		c.Flags().BoolVar(&asJSON, "json", false, "print one JSON result object")
		if withRev {
			c.Flags().Uint64Var(&ifRev, "if-revision", 0, "apply only when the selection is still at this revision")
		}
		addNamedSettingFlags(c, config.Default, config.KeyStateDir, config.KeyTmuxSocket)
		return c
	}
	run := func(c *cobra.Command, ch *store.ShareChange) error {
		res, err := runWorkerShare(c, deps, ch, ifRev)
		if err == nil && !asJSON {
			printShareList(c.OutOrStdout(), res)
		}
		return emitResult(c.OutOrStdout(), c.ErrOrStderr(), asJSON, res, err)
	}
	cmd.AddCommand(
		add(&cobra.Command{Use: "list", Short: "List the sessions and whether each is shared", Args: cobra.NoArgs,
			RunE: func(c *cobra.Command, _ []string) error { return run(c, nil) }}, false),
		add(&cobra.Command{Use: "add <session>...", Short: "Share these sessions too", Args: cobra.MinimumNArgs(1),
			RunE: func(c *cobra.Command, args []string) error { return run(c, &store.ShareChange{Add: args}) }}, true),
		add(&cobra.Command{Use: "remove <session>...", Short: "Stop sharing these sessions", Args: cobra.MinimumNArgs(1),
			RunE: func(c *cobra.Command, args []string) error { return run(c, &store.ShareChange{Remove: args}) }}, true),
		add(&cobra.Command{Use: "set all|none|later|select [<session>...]", Short: "Set the share rule", Args: cobra.MinimumNArgs(1),
			RunE: func(c *cobra.Command, args []string) error {
				mode := store.ShareMode(args[0])
				if !mode.Valid() {
					return emitResult(c.OutOrStdout(), c.ErrOrStderr(), asJSON, nil, &usageError{fmt.Errorf("unknown share rule %q; use all, none, later or select", args[0])})
				}
				if mode != store.ShareSelect && len(args) > 1 {
					return emitResult(c.OutOrStdout(), c.ErrOrStderr(), asJSON, nil, &usageError{fmt.Errorf("session names only go with select")})
				}
				return run(c, &store.ShareChange{Mode: mode, Add: args[1:]})
			}}, true),
	)
	return cmd
}

func printShareList(w io.Writer, r agent.ShareListResult) {
	rule := r.Rule
	if rule == string(store.ShareLater) {
		rule = "later (not decided: nothing shared)"
	}
	_, _ = fmt.Fprintf(w, "Sharing: %s (revision %d)\n", rule, r.Revision)
	if r.Lock {
		_, _ = fmt.Fprintln(w, "The Hub cannot change it (share_lock).")
	}
	if len(r.OverriddenBy) > 0 {
		_, _ = fmt.Fprintf(w, "The Worker applies share_existing from %s at its next start.\n", strings.Join(r.OverriddenBy, ", "))
	}
	if r.Inventory != agent.InventoryOK {
		_, _ = fmt.Fprintln(w, "The sessions cannot be listed now.")
		return
	}
	for _, s := range r.Sessions {
		mark := "not shared"
		if s.Shared {
			mark = "shared"
		}
		_, _ = fmt.Fprintf(w, "  %-24s %s\n", oneLine(s.Name), mark)
	}
}

// runWorkerShare lists (ch nil) or changes the selection.
func runWorkerShare(cmd *cobra.Command, deps workerShareDeps, ch *store.ShareChange, ifRev uint64) (agent.ShareListResult, error) {
	var none agent.ShareListResult
	res := &config.Resolver{Role: config.ScopeWorker, Flags: changedFlags{cmd: cmd, reg: config.Default}, LookupEnv: deps.lookupEnv}
	if deps.loadConfig != nil {
		file, err := loadConfigFile(res, deps.loadConfig)
		if err != nil {
			return none, err
		}
		res.File = file
	}
	core, err := config.ResolveWorkerCore(res)
	if err != nil {
		return none, err
	}
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	if ch != nil {
		ch.IfRevision = ifRev
	}
	return shareWithCore(ctx, deps, core, ch)
}

// shareWithCore lists or changes the selection of the Worker in
// core.StateDir: offline under the lifecycle lock, else through the running
// Worker.
func shareWithCore(ctx context.Context, deps workerShareDeps, core config.WorkerCore, ch *store.ShareChange) (agent.ShareListResult, error) {
	var none agent.ShareListResult
	l, err := deps.lockState(core.StateDir)
	switch {
	case err == nil:
		defer func() { _ = l.Close() }()
		return shareOffline(ctx, deps, core, ch)
	case !errors.Is(err, agent.ErrStateInUse):
		return none, fmt.Errorf("cannot take the lifecycle lock of %s (%w); nothing was changed", core.StateDir, err)
	}
	return shareLive(ctx, deps, core.StateDir, ch)
}

// shareLive asks the running Worker; it owns the database. Every call is
// made only while the Worker that answered first still answers; a replaced
// Worker (another profile or instance) ends the command with exit 27.
func shareLive(ctx context.Context, deps workerShareDeps, stateDir string, ch *store.ShareChange) (agent.ShareListResult, error) {
	var r agent.ShareListResult
	inst, ok := liveInstance(ctx, deps.call, stateDir)
	if !ok {
		return r, newResultError(ExitError, "a Worker runs on "+stateDir+" but does not answer on this user's control socket; nothing was changed")
	}
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if ch == nil {
		r, err := shareCall(cctx, deps, agent.OpLocalShareList, nil)
		return r, err
	}
	// "set select" needs no list: it replaces the selection by exactly what
	// it says, so it also works when the saved selection is too large to be
	// listed (an explicit --if-revision is still checked by the apply).
	// Everything else reads first, so an old Worker, a stale revision or a
	// no-op is refused before anything is written, and the revision read is
	// sent with the apply.
	var (
		a    agent.LocalShareApplyArgs
		cur  agent.ShareListResult
		done bool
		err  error
	)
	if ch.Mode != store.ShareSelect {
		if cur, err = shareCall(cctx, deps, agent.OpLocalShareList, nil); err != nil {
			return r, err
		}
		if a, done, err = shareApplyArgs(cur, *ch); err != nil || done {
			return cur, err
		}
	} else {
		a, _ = applyArgsFor(cur, *ch)
	}
	features := []string{agent.FeatureShareLocal}
	if a.Replace {
		features = append(features, agent.FeatureShareReplace)
	}
	// The removal of the file's rule comes before the commit (a crash must
	// never leave a wider rule to restart from) and goes back when the
	// commit is refused.
	undo, err := removeShareExisting(deps, stateDir)
	if err != nil {
		return r, err
	}
	// Still the same Worker?
	if now, ok := liveInstance(ctx, deps.call, stateDir); !ok || now != inst {
		undo()
		return r, newResultError(ExitStateChanged, "the Worker that answered was replaced; nothing was changed")
	}
	r, err = shareCall(cctx, deps, agent.OpLocalShareApply, a, features...)
	if err != nil {
		// Only a definite refusal puts the file back; after a timeout the
		// commit may have happened and the file must not widen it again.
		var re *resultError
		if errors.As(err, &re) && re.Exit == ExitStateChanged {
			undo()
		}
		return r, err
	}
	return r, overrideResult(r, ch)
}

// shareApplyArgs turns a change into the arguments of worker_share_apply
// from the rule that was just read. The revision read is the condition when
// the caller gave none, so that a change between the read and the apply is
// refused (27) and never merged; an explicit revision is also checked for a
// change that needs nothing (done).
func shareApplyArgs(cur agent.ShareListResult, ch store.ShareChange) (agent.LocalShareApplyArgs, bool, error) {
	if ch.IfRevision != 0 && cur.Revision != ch.IfRevision {
		return agent.LocalShareApplyArgs{}, false, &resultError{Exit: ExitStateChanged,
			Message: "the share selection changed since it was read; nothing was changed", Err: store.ErrShareRevision}
	}
	a, done := applyArgsFor(cur, ch)
	if a.IfRevision == 0 {
		a.IfRevision = cur.Revision
	}
	return a, done, nil
}

func shareCall(ctx context.Context, deps workerShareDeps, op string, args any, features ...string) (agent.ShareListResult, error) {
	if len(features) == 0 {
		features = []string{agent.FeatureShareLocal}
	}
	var r agent.ShareListResult
	raw, err := deps.call(ctx, localapi.QualifiedOp(op, features), args)
	var pe *proto.Error
	if errors.As(err, &pe) && pe.Code == proto.CodeConflict {
		return r, &resultError{Exit: ExitStateChanged, Message: pe.Message, Err: err}
	}
	if err != nil {
		return r, err
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return r, fmt.Errorf("the running Worker returned an invalid share list: %w", err)
	}
	return r, nil
}

// applyArgsFor turns a change into the arguments of worker_share_apply,
// based on the current rule: adding to a rule other than select starts a
// select (all already shares everything: nothing to do), and removing needs
// select. done is true when nothing is to change.
func applyArgsFor(cur agent.ShareListResult, ch store.ShareChange) (agent.LocalShareApplyArgs, bool) {
	a := agent.LocalShareApplyArgs{Mode: string(ch.Mode), Add: ch.Add, Remove: ch.Remove, IfRevision: ch.IfRevision}
	if ch.Mode == "" && len(ch.Add) > 0 && cur.Rule != string(store.ShareSelect) {
		if cur.Rule == string(store.ShareAll) {
			return a, true
		}
		a.Mode = string(store.ShareSelect)
	}
	if ch.Mode == store.ShareSelect {
		// set select replaces the list: exactly the names given (none:
		// nothing shared), decided where the selection is saved, not from a
		// list that another writer may have changed since.
		if a.Add == nil {
			a.Add = []string{}
		}
		a.Replace = true
	}
	return a, false
}

// overrideResult is exit 22 for a mode change that a share_existing from the
// Worker's environment or flags undoes at the next start.
func overrideResult(r agent.ShareListResult, ch *store.ShareChange) error {
	if ch == nil || len(r.OverriddenBy) == 0 {
		return nil
	}
	return &resultError{Exit: ExitNotInForce, Code: "saved_not_in_force",
		Message: "the change is saved, but share_existing from the Worker's " + strings.Join(r.OverriddenBy, ", ") +
			" sets the rule again at its next start; remove it there"}
}

// liveInstance returns the instance of the Worker that answers for dir.
func liveInstance(ctx context.Context, call func(context.Context, string, any) (json.RawMessage, error), dir string) (string, bool) {
	cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	raw, err := call(cctx, agent.OpLocalStatus, nil)
	if err != nil {
		return "", false
	}
	var r agent.LiveStatusReply
	if json.Unmarshal(raw, &r) != nil || r.StateDir == "" || !sameDir(r.StateDir, dir) {
		return "", false
	}
	return r.Instance, true
}

// removeShareExisting deletes share_existing from the config file, never
// writes it: a crash after this leaves the database's choice, never a wider
// mode from the file. The returned func puts the old value back, for a change
// that was refused afterwards.
func removeShareExisting(deps workerShareDeps, stateDir string) (func(), error) {
	noop := func() {}
	release, err := deps.lockConfig(filepath.Join(stateDir, configLockName))
	if err != nil {
		return noop, err
	}
	defer release()
	path := filepath.Join(stateDir, workerConfigName)
	f, err := config.LoadFile(path)
	if err != nil {
		return noop, err
	}
	prev, ok, _ := f.Lookup(config.KeyShareExisting)
	if !ok {
		return noop, nil
	}
	f.Delete(config.KeyShareExisting)
	if err := f.Save(); err != nil {
		return noop, fmt.Errorf("the config file %s could not be updated, nothing was changed: %w", f.Path(), err)
	}
	return func() {
		rel, err := deps.lockConfig(filepath.Join(stateDir, configLockName))
		if err != nil {
			return
		}
		defer rel()
		g, err := config.LoadFile(path)
		if err != nil {
			return
		}
		if _, set, _ := g.Lookup(config.KeyShareExisting); !set {
			g.Set(config.KeyShareExisting, prev)
			_ = g.Save()
		}
	}, nil
}

// shareOffline changes the database directly while the CLI holds the
// lifecycle lock (no Worker runs).
func shareOffline(ctx context.Context, deps workerShareDeps, core config.WorkerCore, ch *store.ShareChange) (agent.ShareListResult, error) {
	var r agent.ShareListResult
	st, err := deps.openStore(ctx, core.StateDir)
	if errors.Is(err, store.ErrSchemaMismatch) {
		return r, &resultError{Exit: ExitSchemaMismatch, Message: err.Error(), Err: err}
	}
	if err != nil {
		return r, fmt.Errorf("open the Worker state in %s: %w", core.StateDir, err)
	}
	defer func() { _ = st.Close() }()
	state, err := st.LoadShareStateContext(ctx)
	if err != nil {
		return r, err
	}
	overridden := shareEnvOverride(deps.lookupEnv)
	if ch != nil {
		cur := agent.ShareListResult{Rule: string(state.Mode), Selected: state.Sessions}
		cur.Revision = state.Revision
		a, done, aerr := shareApplyArgs(cur, *ch)
		if aerr != nil {
			return r, aerr
		}
		if !done {
			undo, err := removeShareExisting(deps, core.StateDir)
			if err != nil {
				return r, err
			}
			if deps.beforeCommit != nil {
				if err := deps.beforeCommit(); err != nil {
					return r, err
				}
			}
			state, err = st.ApplyShareChangeContext(ctx, store.ShareChange{Mode: store.ShareMode(a.Mode), Add: a.Add, Remove: a.Remove, IfRevision: a.IfRevision, Replace: a.Replace})
			if errors.Is(err, store.ErrShareRevision) || errors.Is(err, store.ErrInvalidShare) {
				undo()
			}
			switch {
			case errors.Is(err, store.ErrShareRevision):
				return r, &resultError{Exit: ExitStateChanged, Message: "the share selection changed since it was read; nothing was changed", Err: err}
			case errors.Is(err, store.ErrInvalidShare):
				return r, &usageError{err}
			case err != nil:
				return r, err
			}
		}
	}
	r = agent.ShareListResult{Rule: string(state.Mode), Selected: state.Sessions, Revision: state.Revision,
		Lock: core.ShareLock, OverriddenBy: overridden, Inventory: agent.InventoryUnavailable, Sessions: []agent.ShareListSession{}}
	if r.Selected == nil {
		r.Selected = []string{}
	}
	if deps.sessions != nil {
		if lister := deps.sessions(core.TmuxSocket); lister != nil {
			if list, err := lister.ListSessions(ctx); err == nil {
				r.Inventory = agent.InventoryOK
				for _, s := range list {
					r.Sessions = append(r.Sessions, agent.ShareListSession{Name: s.Name, Shared: state.Includes(s.Name)})
				}
				sort.Slice(r.Sessions, func(i, j int) bool { return r.Sessions[i].Name < r.Sessions[j].Name })
			}
		}
	}
	return r, overrideResult(r, ch)
}

// shareEnvOverride names "env" when this shell sets MINION_SHARE_EXISTING.
func shareEnvOverride(lookupEnv func(string) (string, bool)) []string {
	if lookupEnv != nil {
		if v, ok := lookupEnv("MINION_SHARE_EXISTING"); ok && strings.TrimSpace(v) != "" {
			return []string{string(config.SourceEnv)}
		}
	}
	return []string{}
}
