package cli

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/ops"
)

// Every command of the tree accepts -v and --verbose.
func TestEveryCommandAcceptsVerbose(t *testing.T) {
	var walk func(c *cobra.Command)
	n := 0
	walk = func(c *cobra.Command) {
		n++
		f := c.Flags().Lookup(flagVerbose)
		if f == nil {
			f = c.InheritedFlags().Lookup(flagVerbose)
		}
		if f == nil {
			f = c.PersistentFlags().Lookup(flagVerbose)
		}
		if f == nil || f.Shorthand != "v" {
			t.Errorf("%q does not accept -v/--verbose", c.CommandPath())
		}
		for _, sub := range c.Commands() {
			walk(sub)
		}
	}
	walk(NewRoot())
	if n < 10 {
		t.Fatalf("only %d commands walked", n)
	}
}

// -v means debug, MINION_VERBOSE=1 too, and an explicit level wins over both.
func TestVerboseFlagResolvesTheLogLevel(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		env  map[string]string
		want slog.Level // the lowest enabled level
	}{
		{name: "nothing", want: slog.LevelInfo},
		{name: "-v", args: []string{"-v"}, want: slog.LevelDebug},
		{name: "--verbose", args: []string{"--verbose"}, want: slog.LevelDebug},
		{name: "MINION_VERBOSE=1", env: map[string]string{"MINION_VERBOSE": "1"}, want: slog.LevelDebug},
		{name: "explicit flag level wins", args: []string{"-v", "--log-level", "warn"}, want: slog.LevelWarn},
		{name: "explicit env level wins", args: []string{"-v"}, env: map[string]string{"MINION_LOG_LEVEL": "error"}, want: slog.LevelError},
		{name: "--verbose=false", args: []string{"--verbose=false"}, want: slog.LevelInfo},
		{name: "--verbose=false wins over MINION_VERBOSE", args: []string{"--verbose=false"}, env: map[string]string{"MINION_VERBOSE": "1"}, want: slog.LevelInfo},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got *slog.Logger
			root := &cobra.Command{Use: "minion", SilenceUsage: true, SilenceErrors: true}
			addVerboseFlag(root)
			sub := &cobra.Command{Use: "x", RunE: func(cmd *cobra.Command, _ []string) error {
				res := &config.Resolver{Role: config.ScopeWorker, Flags: changedFlags{cmd: cmd, reg: config.Default},
					LookupEnv: func(k string) (string, bool) { v, ok := tc.env[k]; return v, ok }}
				var err error
				got, err = commandLogger(cmd, res, &bytes.Buffer{})
				return err
			}}
			addSettingFlags(sub, config.Default, config.ScopeWorker)
			root.AddCommand(sub)
			if code := execute(context.Background(), root, append([]string{"x"}, tc.args...), &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
				t.Fatalf("exit %d", code)
			}
			ctx := context.Background()
			if !got.Enabled(ctx, tc.want) || (tc.want > slog.LevelDebug && got.Enabled(ctx, tc.want-4)) {
				t.Errorf("lowest enabled level is not %v", tc.want)
			}
		})
	}
}

// An op command with -v writes one debug line about the call, without its
// arguments; without -v it writes nothing.
func TestVerboseOpCommandLogsTheCall(t *testing.T) {
	run := func(args ...string) string {
		b := &fakeOpsBackend{call: answerWith(writeResult(ops.OutcomeDelivered, "", false), nil)}
		root := &cobra.Command{Use: "minion", SilenceUsage: true, SilenceErrors: true}
		addVerboseFlag(root)
		addOpCommands(root, testDeps(b), ops.All())
		var errb bytes.Buffer
		if code := execute(context.Background(), root, args, &bytes.Buffer{}, &errb); code != 0 {
			t.Fatalf("exit %d: %s", code, errb.String())
		}
		return errb.String()
	}
	t.Setenv("MINION_VERBOSE", "")
	t.Setenv("MINION_LOG_LEVEL", "")
	if got := run("send_text", "--target", "minion-3", "--text", "secret words"); got != "" {
		t.Errorf("without -v stderr = %q", got)
	}
	got := run("-v", "send_text", "--target", "minion-3", "--text", "secret words")
	for _, want := range []string{"op call", "send_text", "local", "ok"} {
		if !strings.Contains(got, want) {
			t.Errorf("stderr %q lacks %q", got, want)
		}
	}
	if strings.Contains(got, "secret words") {
		t.Errorf("the debug line carries the arguments: %q", got)
	}
	// An explicit --verbose=false wins over MINION_VERBOSE.
	t.Setenv("MINION_VERBOSE", "1")
	if got := run("send_text", "--target", "minion-3", "--text", "x"); !strings.Contains(got, "op call") {
		t.Errorf("MINION_VERBOSE=1 stderr = %q", got)
	}
	if got := run("--verbose=false", "send_text", "--target", "minion-3", "--text", "x"); got != "" {
		t.Errorf("--verbose=false with MINION_VERBOSE=1 stderr = %q", got)
	}
}
