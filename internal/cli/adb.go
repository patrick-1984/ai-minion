package cli

// minion adb: on-device adb ("Device control") of the Android Worker. The
// commands talk to the running Worker over its control socket; the Worker
// relays to the Minion app's bridge, which holds the key and the adb
// connection. Exit codes: shell returns the remote exit code; refused
// arguments exit 2 before anything is sent; every Minion-side failure exits
// 255 with the real cause.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/patrick-1984/ai-minion/internal/client"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/worker/devctl"
)

// ExitAdbFailure is the exit status of a Minion-side device control failure.
const ExitAdbFailure = 255

func init() {
	Register(func(root *cobra.Command) { root.AddCommand(newAdbCmd(defaultAdbDeps())) })
}

type adbDeps struct {
	backend func() client.Backend
	home    func() (string, error)
	now     func() time.Time
}

func defaultAdbDeps() adbDeps {
	return adbDeps{backend: func() client.Backend { return client.Local() }, home: os.UserHomeDir, now: time.Now}
}

func newAdbCmd(deps adbDeps) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "adb",
		Short: "Device control: run adb operations on this Android Worker's own device",
		Long: `Device control runs a small set of adb operations with shell-uid rights on the
Android device this Worker runs on, through the Minion app (no PC, no adb
server). It works only when the app build has it, the owner enabled it on the
device, the Worker is unshared and Wireless debugging is on (Wi-Fi).

  minion adb status
  minion adb shell [--timeout S] -- CMD...
  minion adb screencap OUT.png
  minion adb input keyevent|text|tap|swipe ARGS...

From a pane use "$MINION_BINARY" adb ... . shell returns the remote exit code;
refused arguments exit 2; every Minion-side failure exits 255.`,
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	cmd.AddCommand(newAdbStatusCmd(deps), newAdbShellCmd(deps), newAdbScreencapCmd(deps), newAdbInputCmd(deps), newAdbSelftestCmd(deps))
	return cmd
}

// adbFail prints the real cause and exits 255.
func adbFail(cmd *cobra.Command, err error) error {
	msg := err.Error()
	var pe *proto.Error
	if errors.As(err, &pe) {
		msg = pe.Message
		if pe.Code == proto.CodeUnsupportedOp {
			msg = "device control is not available on this Worker (Android only, and only in a build that has it)"
		}
	}
	if errors.Is(err, client.ErrWorkerNotRunning) {
		msg = "no Minion Worker is running; start the Minion app"
	}
	_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "minion adb: "+proto.SafeText(msg, 1024))
	return &exitCodeError{ExitAdbFailure}
}

func adbUsage(cmd *cobra.Command, err error) error {
	_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "minion adb: "+err.Error())
	return &exitCodeError{ExitUsage}
}

func newAdbStatusCmd(deps adbDeps) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show the device control status: build flag, enabled, state, port, key storage, last error",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := context.WithTimeout(cmd.Context(), 15*time.Second)
			defer cancel()
			raw, err := deps.backend().Call(ctx, ops.OpAdbStatus, ops.AdbStatusArgs{})
			if err != nil {
				return adbFail(cmd, err)
			}
			var res ops.AdbStatusResult
			if err := json.Unmarshal(raw, &res); err != nil {
				return adbFail(cmd, fmt.Errorf("the Worker sent a status that cannot be read"))
			}
			if asJSON {
				return writeJSONTo(cmd.OutOrStdout(), res.DeviceControl)
			}
			writeAdbStatus(cmd.OutOrStdout(), res.DeviceControl)
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the status as JSON")
	return cmd
}

func writeJSONTo(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func writeAdbStatus(w io.Writer, d ops.DeviceControlStatus) {
	yes := func(b bool) string {
		if b {
			return "yes"
		}
		return "no"
	}
	port := "-"
	if d.Port > 0 {
		port = fmt.Sprintf("%d (127.0.0.1)", d.Port)
	}
	rows := [][2]string{
		{"Available", yes(d.Available)},
		{"Enabled", yes(d.Enabled)},
		{"State", orDashText(d.State)},
		{"Transport", "tls-loopback"},
		{"Port", port},
		{"Key storage", orDashText(d.KeyStorage)},
		{"Active streams", fmt.Sprint(d.ActiveStreams)},
		{"Shared Worker", yes(d.Shared)},
		{"Last ok", orDashText(d.LastOK)},
		{"Last error", orDashText(d.LastError)},
	}
	if d.KeyStorage == "app-file" {
		rows = append(rows, [2]string{"Warning", "Key stored in an app-private file. Every terminal of this Worker could read it."})
	}
	if d.Reason != "" {
		rows = append(rows, [2]string{"Reason", d.Reason})
	}
	for _, r := range rows {
		_, _ = fmt.Fprintf(w, "%-15s %s\n", r[0]+":", r[1])
	}
}

func orDashText(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// adbRun runs one of the streaming ops and writes the data to stdout and
// stderr. It returns the remote exit code.
func adbRun(ctx context.Context, deps adbDeps, op string, args any, timeout time.Duration, stdout, stderr io.Writer) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, devctl.ClampTimeout(timeout)+devctl.ConnectTimeout+10*time.Second)
	defer cancel()
	raw, err := deps.backend().Stream(ctx, op, args, func(b json.RawMessage) error {
		var o ops.AdbOutput
		if err := json.Unmarshal(b, &o); err != nil {
			return fmt.Errorf("the Worker sent output that cannot be read")
		}
		w := stdout
		if o.Stream == "stderr" {
			w = stderr
		}
		_, err := w.Write(o.Data)
		return err
	})
	if err != nil {
		return 0, err
	}
	var res ops.AdbRunResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return 0, fmt.Errorf("the Worker sent a result that cannot be read")
	}
	return res.ExitCode, nil
}

func newAdbShellCmd(deps adbDeps) *cobra.Command {
	var timeoutS int
	cmd := &cobra.Command{
		Use:   "shell [--timeout S] -- CMD...",
		Short: "Run a non-interactive shell command as the shell user; exits with its exit code",
		RunE: func(cmd *cobra.Command, args []string) error {
			if timeoutS < 0 || timeoutS > ops.AdbMaxTimeoutS {
				return adbUsage(cmd, fmt.Errorf("--timeout must be 0 to %d seconds", ops.AdbMaxTimeoutS))
			}
			if _, err := devctl.ShellCommand(args); err != nil {
				return adbUsage(cmd, err)
			}
			code, err := adbRun(cmd.Context(), deps, ops.OpAdbShell, ops.AdbShellArgs{Argv: args, TimeoutS: timeoutS},
				time.Duration(timeoutS)*time.Second, cmd.OutOrStdout(), cmd.ErrOrStderr())
			if err != nil {
				return adbFail(cmd, err)
			}
			if code != 0 {
				return &exitCodeError{code}
			}
			return nil
		},
	}
	cmd.Flags().IntVar(&timeoutS, "timeout", 0, "seconds until the command is ended (default 120, at most 3600)")
	return cmd
}

// screencapPath checks that out stays inside the current directory.
func screencapPath(out string) (string, error) {
	if out == "" || filepath.IsAbs(out) {
		return "", fmt.Errorf("%w: give a file name relative to the current directory", devctl.ErrInvalidArgs)
	}
	clean := filepath.Clean(out)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%w: the file must stay inside the current directory", devctl.ErrInvalidArgs)
	}
	return clean, nil
}

var pngMagic = []byte("\x89PNG\r\n\x1a\n")

func newAdbScreencapCmd(deps adbDeps) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "screencap OUT.png",
		Short: "Save a PNG of the screen into the current directory",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			path, err := screencapPath(args[0])
			if err != nil {
				return adbUsage(cmd, err)
			}
			var png, stderr bytes.Buffer
			code, err := adbRun(cmd.Context(), deps, ops.OpAdbScreencap, ops.AdbScreencapArgs{}, 60*time.Second, &png, &stderr)
			if err != nil {
				return adbFail(cmd, err)
			}
			if code != 0 || !bytes.HasPrefix(png.Bytes(), pngMagic) {
				return adbFail(cmd, fmt.Errorf("screencap failed on the device (exit %d); nothing was written", code))
			}
			tmp := path + ".part"
			if err := os.WriteFile(tmp, png.Bytes(), 0o600); err != nil {
				return adbFail(cmd, err)
			}
			if err := os.Rename(tmp, path); err != nil {
				_ = os.Remove(tmp)
				return adbFail(cmd, err)
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Wrote %s (%d bytes).\n", path, png.Len())
			return nil
		},
	}
	return cmd
}

func newAdbInputCmd(deps adbDeps) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "input keyevent|text|tap|swipe ARGS...",
		Short: "Send a key event, text, tap or swipe (arguments are checked before anything is sent)",
		RunE: func(cmd *cobra.Command, args []string) error {
			if _, err := devctl.InputCommand(args); err != nil {
				return adbUsage(cmd, err)
			}
			code, err := adbRun(cmd.Context(), deps, ops.OpAdbInput, ops.AdbInputArgs{Kind: args[0], Args: args[1:]},
				30*time.Second, cmd.OutOrStdout(), cmd.ErrOrStderr())
			if err != nil {
				return adbFail(cmd, err)
			}
			if code != 0 {
				return &exitCodeError{code}
			}
			return nil
		},
	}
	return cmd
}
