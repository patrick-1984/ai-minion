package cli

import (
	"errors"
	"io"
	"log/slog"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/logx"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// flagVerbose is the flag of the verbose setting ([config.KeyVerbose]). It is
// registered once, as a persistent flag of the root, so that every command
// accepts -v and --verbose; addSettingFlags leaves it out for that reason.
const flagVerbose = "verbose"

// addVerboseFlag adds -v/--verbose to root for every command.
func addVerboseFlag(root *cobra.Command) {
	root.PersistentFlags().BoolP(flagVerbose, "v", false,
		"verbose: log at debug level for troubleshooting; an explicit --log-level or MINION_LOG_LEVEL wins (env MINION_VERBOSE)")
}

// verboseFlag returns the value of -v/--verbose when it was given to cmd, and
// nil when it was not: an explicit --verbose=false must win over
// MINION_VERBOSE, so "false" and "not given" are kept apart.
func verboseFlag(cmd *cobra.Command) *bool {
	f := cmd.Flags().Lookup(flagVerbose)
	if f == nil || !f.Changed {
		return nil
	}
	on := f.Value.String() == "true"
	return &on
}

// commandLogger builds the logger of a command from log_level, log_format
// and the verbose switch ([config.ResolveLogLevel]), writing to out.
func commandLogger(cmd *cobra.Command, res *config.Resolver, out io.Writer) (*slog.Logger, error) {
	level, err := config.ResolveLogLevel(res, verboseFlag(cmd))
	if err != nil {
		return nil, err
	}
	format, err := res.Resolve(config.KeyLogFormat)
	if err != nil {
		return nil, err
	}
	return logx.New(logx.Options{Level: level, Format: logx.Format(format.Value), Output: out})
}

// debugOpCall writes one debug line about an op command's call: the op, the
// backend (hub or local), the outcome (ok or the error code) and the
// duration. It writes nothing unless the level is debug (-v, MINION_VERBOSE
// or MINION_LOG_LEVEL=debug). Arguments and results are never logged.
func debugOpCall(cmd *cobra.Command, op ops.Op, local bool, start time.Time, err error) {
	res := &config.Resolver{Role: config.ScopeWorker, LookupEnv: os.LookupEnv}
	log, lerr := commandLogger(cmd, res, cmd.ErrOrStderr())
	if lerr != nil || !log.Enabled(cmd.Context(), slog.LevelDebug) {
		return
	}
	backend := "hub"
	if local {
		backend = "local"
	}
	log.Debug("op call", "op", op.Name, "backend", backend, "outcome", outcomeCode(err),
		"duration", time.Since(start).String())
}

// outcomeCode is "ok" for nil, the code of a *proto.Error, else "error".
func outcomeCode(err error) string {
	if err == nil {
		return "ok"
	}
	var pe *proto.Error
	if errors.As(err, &pe) {
		return string(pe.Code)
	}
	return "error"
}
