package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/patrick-1984/ai-minion/internal/client"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/worker/store"
)

// Every result is exactly one JSON object on stdout with ok, code and
// message, the command fields beside them, and the exit status of its code.
func TestResultWriterOneObject(t *testing.T) {
	type fields struct {
		Instance string `json:"instance"`
		Code     string `json:"code"` // overridden by the contract member
	}
	for _, tc := range []struct {
		name     string
		fields   any
		err      error
		exit     int
		code     string
		extraKey string
	}{
		{name: "ok", fields: fields{Instance: "i1", Code: "x"}, exit: ExitOK, code: "ok", extraKey: "instance"},
		{name: "ok without fields", exit: ExitOK, code: "ok"},
		{name: "plain error", err: errors.New("boom"), exit: ExitError, code: "error"},
		{name: "usage", err: &usageError{errors.New("bad flag")}, exit: ExitUsage, code: "usage"},
		{name: "unsupported op", err: proto.NewError(proto.CodeUnsupportedOp, "unknown op"), exit: ExitWorkerTooOld, code: "worker_too_old"},
		{name: "worker too old", err: fmt.Errorf("%w: needs x", client.ErrWorkerTooOld), exit: ExitWorkerTooOld, code: "worker_too_old"},
		{name: "needs admin", err: newResultError(ExitNeedsAdmin, "run elevated"), exit: ExitNeedsAdmin, code: "needs_admin"},
		{name: "sessions would end", fields: map[string]any{"instance": "i2"}, err: &resultError{Exit: ExitSessionsWouldEnd, Message: "2 sessions would end", Fields: map[string]any{"sessions_lost": 2}},
			exit: ExitSessionsWouldEnd, code: "sessions_would_end", extraKey: "sessions_lost"},
		{name: "own code word", err: &resultError{Exit: ExitNotInForce, Code: "saved_not_confirmed", Message: "not confirmed"}, exit: ExitNotInForce, code: "saved_not_confirmed"},
		{name: "count unavailable", err: newResultError(ExitCountUnavailable, "cannot count"), exit: ExitCountUnavailable, code: "count_unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			err := emitResult(&out, &errOut, true, tc.fields, tc.err)
			var ec *exitCodeError
			switch {
			case tc.exit == ExitOK && err != nil:
				t.Fatalf("err = %v", err)
			case tc.exit != ExitOK && (!errors.As(err, &ec) || ec.code != tc.exit):
				t.Fatalf("err = %v, want exit %d", err, tc.exit)
			}
			if strings.Count(out.String(), "\n") != 1 || !strings.HasSuffix(out.String(), "\n") {
				t.Fatalf("stdout is not one line: %q", out.String())
			}
			dec := json.NewDecoder(&out)
			var obj map[string]any
			if err := dec.Decode(&obj); err != nil {
				t.Fatal(err)
			}
			if dec.More() {
				t.Fatal("more than one JSON value on stdout")
			}
			if obj["ok"] != (tc.exit == ExitOK) || obj["code"] != tc.code {
				t.Fatalf("object = %v", obj)
			}
			if _, ok := obj["message"].(string); !ok {
				t.Fatalf("no message: %v", obj)
			}
			if tc.extraKey != "" && obj[tc.extraKey] == nil {
				t.Fatalf("member %s missing: %v", tc.extraKey, obj)
			}
			if tc.err != nil && !strings.Contains(errOut.String(), tc.err.Error()) {
				t.Fatalf("stderr = %q, want the message", errOut.String())
			}
		})
	}
}

// Without --json nothing goes to stdout; a failure is one line on stderr.
func TestResultWriterHumanMode(t *testing.T) {
	var out, errOut bytes.Buffer
	err := emitResult(&out, &errOut, false, map[string]int{"n": 1}, newResultError(ExitNotRunning, "no Worker runs"))
	var ec *exitCodeError
	if !errors.As(err, &ec) || ec.code != ExitNotRunning {
		t.Fatalf("err = %v", err)
	}
	if out.Len() != 0 || errOut.String() != "Error: no Worker runs\n" {
		t.Fatalf("stdout %q, stderr %q", out.String(), errOut.String())
	}
}

func TestResultCodesAreDistinct(t *testing.T) {
	seen := map[string]int{}
	for exit, code := range resultCodes {
		if other, dup := seen[code]; dup {
			t.Fatalf("code %q used for %d and %d", code, exit, other)
		}
		seen[code] = exit
	}
	if resultCode(99) != "error" {
		t.Fatal("an unknown status must map to error")
	}
}

// Every CLI path that opens worker.db does so without migrating it; another
// schema ends the command with 28 (tray W6a, PR-1 rule).
func TestSchemaMismatchExits28(t *testing.T) {
	root := &cobra.Command{Use: "minion", SilenceUsage: true, SilenceErrors: true,
		RunE: func(*cobra.Command, []string) error { return fmt.Errorf("open: %w", store.ErrSchemaMismatch) }}
	var out, errOut bytes.Buffer
	if code := execute(context.Background(), root, nil, &out, &errOut); code != ExitSchemaMismatch {
		t.Fatalf("exit %d, want 28", code)
	}
}
