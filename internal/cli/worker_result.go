package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/spf13/cobra"
	"io"

	"github.com/patrick-1984/ai-minion/internal/client"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// The result contract of the Worker lifecycle and tray commands (tray
// architecture v2 section 3): with --json a command prints exactly one JSON
// object on stdout, {"ok":bool,"code":"...","message":"...", ...fields},
// writes its human text to stderr, and ends with the exit status that the code
// word names. The tray reads only this object and the exit status.

// resultCodes maps an exit status to its code word.
var resultCodes = map[int]string{
	ExitOK:               "ok",
	ExitError:            "error",
	ExitUsage:            "usage",
	ExitWorkerTooOld:     "worker_too_old",
	ExitNeedsAdmin:       "needs_admin",
	ExitNotInForce:       "saved_not_in_force",
	ExitSessionsWouldEnd: "sessions_would_end",
	ExitNotRunning:       "not_running",
	ExitPaused:           "paused",
	ExitNotInstalled:     "not_installed",
	ExitStateChanged:     "state_changed",
	ExitSchemaMismatch:   "schema_mismatch",
	ExitCountUnavailable: "count_unavailable",
}

// resultCode returns the code word of an exit status ("error" for one this
// table does not know).
func resultCode(exit int) string {
	if c, ok := resultCodes[exit]; ok {
		return c
	}
	return "error"
}

// resultError is a failure with its exit status. Code overrides the default
// code word of the status (for example saved_not_confirmed for 22), and
// Fields are added to the JSON object (for example sessions_lost).
type resultError struct {
	Exit    int
	Code    string
	Message string
	Fields  map[string]any
	Err     error
}

func (e *resultError) Error() string {
	switch {
	case e.Message != "":
		return e.Message
	case e.Err != nil:
		return e.Err.Error()
	}
	return resultCode(e.Exit)
}

func (e *resultError) Unwrap() error { return e.Err }

// newResultError returns a failure with the exit status exit and the message
// msg.
func newResultError(exit int, msg string) *resultError {
	return &resultError{Exit: exit, Message: msg}
}

// classifyResult maps err to the exit status, code word and message of the
// result. A refusal by the Worker of an op it does not know (unsupported_op,
// or the client's "Worker too old") is 20; a usage error is 2; every other
// error is 1.
func classifyResult(err error) (exit int, code, msg string, fields map[string]any) {
	if err == nil {
		return ExitOK, "ok", "", nil
	}
	var re *resultError
	if errors.As(err, &re) {
		code = re.Code
		if code == "" {
			code = resultCode(re.Exit)
		}
		return re.Exit, code, re.Error(), re.Fields
	}
	var pe *proto.Error
	if errors.Is(err, client.ErrWorkerTooOld) || (errors.As(err, &pe) && pe.Code == proto.CodeUnsupportedOp) {
		return ExitWorkerTooOld, resultCode(ExitWorkerTooOld), err.Error(), nil
	}
	var ue *usageError
	if errors.As(err, &ue) {
		return ExitUsage, resultCode(ExitUsage), err.Error(), nil
	}
	return ExitError, resultCode(ExitError), err.Error(), nil
}

// resultObject builds the one JSON object of a result: the members of fields
// (a struct or map that marshals to an object, or nil), then ok, code and
// message, which always win over a member of the same name.
func resultObject(fields any, err error) (map[string]json.RawMessage, int, error) {
	exit, code, msg, extra := classifyResult(err)
	obj := map[string]json.RawMessage{}
	if fields != nil {
		raw, merr := json.Marshal(fields)
		if merr != nil {
			return nil, ExitError, fmt.Errorf("encode the result: %w", merr)
		}
		if !bytes.Equal(raw, []byte("null")) {
			if uerr := json.Unmarshal(raw, &obj); uerr != nil {
				return nil, ExitError, fmt.Errorf("the result is not a JSON object: %w", uerr)
			}
		}
	}
	for k, v := range extra {
		raw, merr := json.Marshal(v)
		if merr != nil {
			return nil, ExitError, fmt.Errorf("encode the result member %s: %w", k, merr)
		}
		obj[k] = raw
	}
	obj["ok"], _ = json.Marshal(exit == ExitOK)
	obj["code"], _ = json.Marshal(code)
	obj["message"], _ = json.Marshal(msg)
	return obj, exit, nil
}

// emitResult ends a Worker lifecycle or tray command. With jsonOut it writes
// the one JSON object to stdout; without it, the message of a failure goes to
// stderr (the command prints its own human text for a success). It returns
// nil for a success and an *exitCodeError carrying the status otherwise, so
// that the root command prints nothing more.
func emitResult(stdout, stderr io.Writer, jsonOut bool, fields any, err error) error {
	obj, exit, oerr := resultObject(fields, err)
	if oerr != nil {
		_, _ = fmt.Fprintln(stderr, "Error:", oerr)
		return &exitCodeError{ExitError}
	}
	if jsonOut {
		b, merr := json.Marshal(obj)
		if merr != nil {
			_, _ = fmt.Fprintln(stderr, "Error:", merr)
			return &exitCodeError{ExitError}
		}
		b = append(b, '\n')
		if _, werr := stdout.Write(b); werr != nil {
			return &exitCodeError{ExitError}
		}
	}
	if exit == ExitOK {
		return nil
	}
	if !jsonOut {
		var msg string
		_ = json.Unmarshal(obj["message"], &msg)
		_, _ = fmt.Fprintln(stderr, "Error:", oneLine(msg))
	} else if err != nil {
		_, _ = fmt.Fprintln(stderr, oneLine(err.Error()))
	}
	return &exitCodeError{exit}
}

// wantsJSON reports whether args ask for the JSON result (--json, --json=true
// or --json true is not a form of a bool flag, so only the first two), before
// any "--" delimiter.
func wantsJSON(args []string) bool {
	for _, a := range args {
		if a == "--" {
			return false
		}
		if a == "--json" || a == "--json=true" || a == "--json=1" || a == "--json=t" || a == "--json=T" || a == "--json=TRUE" || a == "--json=True" {
			return true
		}
	}
	return false
}

// installJSONUsage makes a usage failure (a wrong number of arguments, an
// unknown or malformed flag) of a command run with --json a result object
// with exit status 2, like every other failure of that command; cobra
// validates arguments and flags before RunE, where the commands emit their
// own results. It changes nothing without --json in args.
func installJSONUsage(root *cobra.Command, args []string) {
	if !wantsJSON(args) {
		return
	}
	root.SetFlagErrorFunc(func(c *cobra.Command, err error) error {
		return emitResult(c.OutOrStdout(), c.ErrOrStderr(), true, nil, &usageError{err})
	})
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		if orig := c.Args; orig != nil && c.Flags().Lookup("json") != nil {
			c.Args = func(cmd *cobra.Command, a []string) error {
				if err := orig(cmd, a); err != nil {
					var ue *usageError
					if !errors.As(err, &ue) {
						err = &usageError{err}
					}
					return emitResult(cmd.OutOrStdout(), cmd.ErrOrStderr(), true, nil, err)
				}
				return nil
			}
		}
		for _, k := range c.Commands() {
			walk(k)
		}
	}
	walk(root)
}
