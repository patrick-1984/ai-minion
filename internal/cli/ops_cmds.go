package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"

	"github.com/spf13/cobra"

	"github.com/patrick-1984/ai-minion/internal/client"
	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/worker/localapi"
)

// Every op of the catalog (internal/ops) is a subcommand of `minion` and of
// `mn`, named by op.CLICommand() and with flags derived from the argument
// struct (AD-2, AD-22). Nothing here lists an op by name: a story that adds an
// op to the catalog gets its command for free.

func init() {
	Register(func(root *cobra.Command) {
		addOpCommands(root, defaultOpsDeps(), ops.All())
	})
}

// HubBackendFactory returns an explicit backend for the Hub REST API, or
// (nil, nil) when it names none. It is an override (a host program or a test);
// the settings of the command (MINION_HUB_URL, MINION_API_KEY_FILE and the
// --hub-* flags) are read by the command itself, see [opsDeps.hubSettings].
type HubBackendFactory func() (client.Backend, error)

var (
	hubMu      sync.RWMutex
	hubFactory HubBackendFactory
)

// SetHubBackend installs the factory of the Hub backend. Without one (or when
// it reports no Hub configured) the op commands read the Hub client settings,
// and without those use the local control socket.
func SetHubBackend(f HubBackendFactory) {
	hubMu.Lock()
	defer hubMu.Unlock()
	hubFactory = f
}

func currentHubBackend() (client.Backend, error) {
	hubMu.RLock()
	f := hubFactory
	hubMu.RUnlock()
	if f == nil {
		return nil, nil
	}
	return f()
}

// opsDeps are the seams of the op commands that tests replace.
type opsDeps struct {
	// local returns the backend on the Worker's local control socket.
	local func() client.Backend
	// localSockets lists the candidate sockets of the local Worker, in the
	// order the client tries them, and localAt returns the backend bound to
	// exactly one of them. Both are set together. They let an implicit target
	// be confirmed and used on one and the same socket (see runOp); without
	// them local is used for both.
	localSockets func() ([]string, error)
	localAt      func(path string) client.Backend
	// hub returns an explicit Hub backend, or (nil, nil) when none is given.
	hub HubBackendFactory
	// hubSettings resolves the Hub client settings of one run (flags of cmd,
	// environment, config file). Nil means the settings are not read.
	hubSettings func(cmd *cobra.Command) (config.ClientHub, error)
	// session finds the current tmux session for the default target.
	session SessionResolver
	// confirm proves that the tmux server of the current pane is the one the
	// local Worker serves, before the current session is used as the target.
	// Nil refuses every implicit target.
	confirm ServerConfirmer
	// newID returns a fresh request id (UUIDv7).
	newID func() string
}

func defaultOpsDeps() opsDeps {
	return opsDeps{
		local: func() client.Backend { return client.Local() },
		localSockets: func() ([]string, error) {
			return localapi.SocketCandidates(config.SystemPathEnv())
		},
		localAt: func(path string) client.Backend { return client.Local(client.WithSocketPath(path)) },
		hub:     currentHubBackend,
		hubSettings: func(cmd *cobra.Command) (config.ClientHub, error) {
			return resolveHubSettings(cmd.Flags(), os.LookupEnv, loadWorkerConfig)
		},
		session: resolveTmuxSession(os.Getenv, newSessionLister),
		confirm: confirmTmuxServer(os.Getenv, newSessionLister, newProbeNonce, defaultProbeLocker),
		newID:   ops.NewUUIDv7,
	}
}

// addOpCommands adds one command per op to root. A broken declaration (two
// arguments with one flag name) is a programming error and panics, which the
// tests of the whole catalog catch.
func addOpCommands(root *cobra.Command, deps opsDeps, list []ops.Op) {
	for _, op := range list {
		cmd, err := newOpCmd(op, deps)
		if err != nil {
			panic("cli: op " + op.Name + ": " + err.Error())
		}
		root.AddCommand(cmd)
	}
}

func newOpCmd(op ops.Op, deps opsDeps) (*cobra.Command, error) {
	fields, err := deriveFields(op.Args)
	if err != nil {
		return nil, err
	}
	fields = slices.DeleteFunc(fields, func(f argField) bool { return ops.HiddenMember(op.Name, f.JSON) })
	hasTarget := false
	for _, f := range fields {
		hasTarget = hasTarget || f.Kind == kindTarget
	}
	cmd := &cobra.Command{
		Use:           op.CLICommand(),
		Short:         op.Summary,
		Long:          longHelp(op),
		Args:          noArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runOp(cmd, deps, op, fields, hasTarget)
		},
	}
	cmd.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return &usageError{err} })
	bindFlags(cmd.Flags(), op.Name, fields)
	cmd.Flags().Bool(flagJSON, false, "print the result as JSON (one line; one line per element for a stream)")
	addHubFlags(cmd.Flags())
	return cmd, nil
}

// noArgs refuses positional arguments: everything is a flag.
func noArgs(cmd *cobra.Command, args []string) error {
	if len(args) > 0 {
		return usagef("unexpected argument %q; %s takes only flags", args[0], cmd.CommandPath())
	}
	return nil
}

// longHelp is the long help text, derived from the declaration.
func longHelp(op ops.Op) string {
	var b strings.Builder
	b.WriteString(op.Summary)
	b.WriteString("\n\n")
	if op.Scope == ops.ScopeHub {
		b.WriteString("This is a Hub operation: it needs a Hub (MINION_HUB_URL and MINION_API_KEY_FILE) and is not served by the local control socket.\n")
	} else {
		b.WriteString("This is a Worker operation. It runs on the local Worker through its control socket, or through the Hub when one is configured (MINION_HUB_URL and MINION_API_KEY_FILE, --hub-url); a configured Hub is never replaced by the local Worker.\n")
	}
	fmt.Fprintf(&b, "Accepted actions: %s.\n", op.Actions)
	switch {
	case op.Write:
		b.WriteString("\nThis is a write. The result reports the outcome, the reason, whether the refusal is forceable and any warnings.\n")
		b.WriteString("Exit status: 0 delivered, 3 failed_not_sent, 4 partial or unknown, 1 error, 2 usage.\n")
		b.WriteString("A FORCE refusal is repeated with --force only when you pass it.\n")
		if guardedOp(op) {
			b.WriteString("A Worker that is older than this command and lacks the safeguard this request relies on refuses it before anything is sent (exit 1, not an outcome); update and restart the Worker.\n")
		}
	case op.Mutates:
		b.WriteString("\nThis operation changes state. Exit status: 0 done, 4 unknown (the request may have been delivered and the change may or may not have been made: ask for the current state before repeating it), 1 error, 2 usage.\n")
	case op.Stream:
		b.WriteString("\nThis operation streams: every element is printed as it arrives, until it ends or you press Ctrl-C.\n")
	}
	if op.Scope == ops.ScopeWorker && hasWorkerIDArg(op) {
		b.WriteString("\n--worker-id names the Worker that runs the operation and is required. It is routed by the Hub (MINION_HUB_URL and MINION_API_KEY_FILE). Without a Hub the command is refused: it is never sent to the local Worker, and the local Worker would refuse an id that is not its own anyway. To change the sharing of the local Worker without a Hub use its own settings (minion worker config).\n")
	}
	return b.String()
}

// hasWorkerIDArg reports whether the arguments of op have a top-level worker_id
// member of their own.
func hasWorkerIDArg(op ops.Op) bool {
	for i := range op.Args.NumField() {
		f := op.Args.Field(i)
		if name, _, _ := strings.Cut(f.Tag.Get("json"), ","); name == "worker_id" {
			return true
		}
	}
	return false
}

// runOp runs one op command.
func runOp(cmd *cobra.Command, deps opsDeps, op ops.Op, fields []argField, hasTarget bool) error {
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	asJSON, _ := cmd.Flags().GetBool(flagJSON)
	out := printer{w: cmd.OutOrStdout(), json: asJSON}

	members, err := collectArgs(ctx, cmd, fields, &stdinOnce{r: cmd.InOrStdin()})
	if err != nil {
		return err
	}
	var implicit string // the current session, when it is the target
	if hasTarget {
		value, _ := cmd.Flags().GetString("target")
		raw, name, err := targetMember(ctx, value, cmd.Flags().Changed("target"), deps.session)
		if err != nil {
			return err
		}
		members["target"] = raw
		implicit = name
	}
	var requestID string
	if op.Write {
		if raw, ok := members["request_id"]; ok {
			_ = json.Unmarshal(raw, &requestID)
		} else {
			requestID = deps.newID()
			members["request_id"], _ = json.Marshal(requestID)
		}
	}
	args, err := validateArgs(op, members)
	if err != nil {
		return err
	}

	backend, local, err := pickBackend(cmd, deps, op)
	if err != nil {
		return err
	}
	if local && !delegatesTarget(op) {
		// The local control socket serves its own Worker and ignores the
		// Worker part of a target, so a foreign Worker id would silently act
		// on a same-named local session.
		if err := rejectForeignWorker(args); err != nil {
			return err
		}
	}
	if implicit != "" {
		// The session name of $TMUX_PANE belongs to the tmux server of the
		// shell; the Worker may serve another server with a session of the
		// same name. Nothing is sent until both are shown to be one server.
		if !local {
			return usagef("the current session cannot be used as the target with a Hub backend: it does not say which Worker owns it; pass --target <worker-id>:<session>")
		}
		if deps.confirm == nil {
			return usagef("the current session cannot be used as the target: the tmux server of this pane cannot be matched to the Worker; pass --target <session>")
		}
		// The write goes to the very socket that was confirmed. The client
		// would otherwise look for a Worker again on every call and could
		// fall over to another one that serves a same-named session on a
		// different tmux server.
		backend, err = confirmLocal(ctx, deps, implicit, backend)
		if err != nil {
			return err
		}
	}
	var raw json.RawMessage
	if op.Stream {
		raw, err = backend.Stream(ctx, op.WireName(), args, func(el json.RawMessage) error {
			return out.element(op, el)
		})
	} else {
		raw, err = backend.Call(ctx, op.WireName(), args)
	}
	if err != nil {
		return failure(cmd, out, op, requestID, err)
	}
	if op.Write {
		// The result is decoded before anything is printed: a result that
		// does not decode came from a request that was delivered, so it is an
		// unknown outcome like a lost connection (AD-15), not an error.
		var res ops.WriteResult
		if err := json.Unmarshal(raw, &res); err != nil {
			return unknownOutcome(cmd, out, op, requestID)
		}
		if err := out.result(op, raw); err != nil {
			return err
		}
		if code := outcomeExitCode(res.Outcome); code != ExitOK {
			return &exitCodeError{code}
		}
		return nil
	}
	return out.result(op, raw)
}

// confirmLocal proves that the tmux server of the current pane is the one the
// local Worker serves and returns the backend bound to the socket of that
// Worker. With the socket seams of deps every candidate socket is tried in
// order and the first Worker that proves the identity is used for the call;
// without them the one backend given is confirmed and used.
func confirmLocal(ctx context.Context, deps opsDeps, session string, fallback client.Backend) (client.Backend, error) {
	refuse := func(err error) error {
		return usagef("the current session %q is not used as the target: %v; pass --target <session> to name it", session, err)
	}
	if deps.localSockets == nil || deps.localAt == nil {
		if err := deps.confirm(ctx, session, fallback); err != nil {
			return nil, refuse(err)
		}
		return fallback, nil
	}
	paths, err := deps.localSockets()
	if err != nil {
		return nil, refuse(err)
	}
	var first error // the most telling refusal: not "no Worker here"
	var notRunning error
	for _, p := range paths {
		b := deps.localAt(p)
		err := deps.confirm(ctx, session, b)
		if err == nil {
			return b, nil
		}
		if errors.Is(err, client.ErrWorkerNotRunning) {
			if notRunning == nil {
				notRunning = err
			}
			continue
		}
		if first == nil {
			first = err
		}
	}
	if first == nil {
		first = notRunning
	}
	if first == nil {
		first = client.ErrWorkerNotRunning
	}
	return nil, refuse(first)
}

// pickBackend chooses the backend of one command. A Hub that is given (the
// explicit factory) or configured (MINION_HUB_URL, MINION_API_KEY_FILE, the
// --hub-* flags) always wins, also when a local control socket exists, and its
// failure is returned: the request never falls back to the local Worker or to
// another Hub. Without a Hub the local control socket is used (local is true).
// A Hub operation cannot run without a Hub.
func pickBackend(cmd *cobra.Command, deps opsDeps, op ops.Op) (b client.Backend, local bool, err error) {
	if deps.hub != nil {
		b, err := deps.hub()
		if err != nil {
			return nil, false, fmt.Errorf("configure the Hub backend: %w", err)
		}
		if b != nil {
			return b, false, nil
		}
	}
	if deps.hubSettings != nil {
		cfg, err := deps.hubSettings(cmd)
		if err != nil {
			return nil, false, fmt.Errorf("configure the Hub backend: %w", err)
		}
		if cfg.Configured() {
			hb, err := hubBackend(cfg, cmd.ErrOrStderr())
			if err != nil {
				return nil, false, fmt.Errorf("configure the Hub backend: %w", err)
			}
			return hb, false, nil
		}
	}
	if op.Scope == ops.ScopeHub {
		return nil, false, fmt.Errorf("%s is a Hub operation and no Hub is configured: set MINION_HUB_URL and MINION_API_KEY_FILE", op.Name)
	}
	lb := deps.local()
	if deps.hubSettings != nil {
		// The settings were consulted and name no Hub: a failure to find the
		// Worker also says how to configure one.
		lb = client.NoHubHint(lb)
	}
	return lb, true, nil
}

// guardedOp reports whether a request of op can rely on a safeguard of the
// Worker (ops.RequiredFeatures): the reviewed answer, the atomic submit and the
// verbatim text.
func guardedOp(op ops.Op) bool {
	switch op.Name {
	case ops.OpAnswer, ops.OpSendText, ops.OpPipeSend:
		return true
	}
	return false
}

// delegatesTarget reports whether the Worker, and not this command, decides
// what the Worker part of the target means. It is true for the pipe ops: the
// local pipe service knows whether the Worker is enrolled with a Hub, which a
// command on a shell cannot tell (and must not guess from the environment). In
// Hub mode it forwards the structured target, worker_id included, to the Hub
// with the identity of the Worker; without a Hub (Solo) it refuses a target that
// names a Worker, so nothing foreign is ever done to a local terminal. Every
// other op runs on the local terminals themselves and is refused here.
func delegatesTarget(op ops.Op) bool {
	switch op.Name {
	case ops.OpPipeSend, ops.OpPipeRead, ops.OpPipeWait:
		return true
	}
	return false
}

// rejectForeignWorker refuses a request that names a Worker, in its target or in
// its own worker_id argument (the share operations), when the request goes to
// the local control socket. Without a Hub there is no way to route to another
// Worker: such a command can only be about the Worker it runs against, and it
// says so by leaving the Worker out. A named Worker is never sent to the local
// socket, not even when it might be the local one: the command cannot tell, and
// the local Worker holds the same line for a caller that does not come through
// this command (see Executor.requireSelf).
func rejectForeignWorker(args json.RawMessage) error {
	var a struct {
		Target   *ops.Target `json:"target"`
		WorkerID string      `json:"worker_id"`
	}
	if json.Unmarshal(args, &a) != nil {
		return nil
	}
	if a.Target != nil && a.Target.WorkerID != "" {
		return usagef("--target names the Worker %s, but no Hub is configured, so the command can only reach the local Worker; give just the session name for a session of the local Worker", a.Target.WorkerID)
	}
	if a.WorkerID != "" {
		return usagef("--worker-id names the Worker %s, but no Hub is configured, so there is nothing to route to it and the command is not sent to the local Worker instead; configure a Hub (MINION_HUB_URL, MINION_API_KEY_FILE), or change the sharing of the local Worker with its own settings (minion worker config)", a.WorkerID)
	}
	return nil
}

// retryWindow is how long the Worker remembers the result of a request id
// (store.ResultTTL). A repeat inside the window is answered from the cache; a
// repeat after it runs the write again.
const retryWindow = "10 minutes"

// outcomeUnknown reports whether err of a write leaves its outcome unknown: the
// connection broke, the reply was not understood (client.ErrProtocol is only
// returned after the request frame went out) or the command was interrupted
// (Ctrl-C, SIGTERM, a deadline), after the request may already have been
// delivered (AD-15). The
// local backend reports an interrupted call as the context error.
func outcomeUnknown(err error) bool {
	return errors.Is(err, client.ErrConnectionLost) ||
		errors.Is(err, client.ErrProtocol) ||
		errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded)
}

// failure turns an error of the call into the command result. A write whose
// connection broke or that was interrupted after the request may have gone out
// has an unknown outcome (AD-15): it is printed like any write result and ends
// with status 4, never as a plain error and never as "not sent".
func failure(cmd *cobra.Command, out printer, op ops.Op, requestID string, err error) error {
	var pe *proto.Error
	switch {
	case errors.Is(err, client.ErrWorkerTooOld):
		// The running Worker refused the guarded request before anything
		// happened: a plain, definite failure with the update-needed message.
		return err
	case op.Write && outcomeUnknown(err):
		return unknownOutcome(cmd, out, op, requestID)
	case op.Mutates && outcomeUnknown(err):
		// A change of state that has no WriteResult to print (share_set): say
		// plainly that it may have been made, and how to find out.
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "The outcome of %s is unknown: the request may have been delivered and the change may or may not have been made. Do not assume either; ask for the current state (for example share_get for the sharing of a Worker) before repeating it.\n", op.Name)
		return &exitCodeError{ExitPartial}
	case errors.As(err, &pe):
		msg := pe.Message
		if msg == "" {
			msg = pe.Code.Description()
		}
		return fmt.Errorf("%s: %s", pe.Code, oneLine(msg))
	}
	return err
}

// unknownOutcome prints the unknown result of a write, tells how to repeat it
// safely and ends with status 4.
func unknownOutcome(cmd *cobra.Command, out printer, op ops.Op, requestID string) error {
	res := ops.WriteResult{Outcome: ops.OutcomeUnknown, Reason: proto.CodeConnectionLost.String()}
	raw, _ := json.Marshal(res)
	if perr := out.value(op, raw); perr != nil {
		return perr
	}
	_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "The outcome is unknown. Do not retry blindly. Repeating the command with --request-id %s within %s is safe, because the Worker answers a duplicate from its cache. After that the Worker has forgotten the id and would deliver again: look at the session first (for example with session_capture) and only then repeat the command.\n", requestID, retryWindow)
	return &exitCodeError{ExitPartial}
}
