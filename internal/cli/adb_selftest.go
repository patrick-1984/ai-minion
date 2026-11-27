package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/version"
	"github.com/patrick-1984/ai-minion/internal/worker/devctl"
)

// GateReportName is the report `minion adb selftest --gate` writes into the
// home directory.
const GateReportName = "dc-gate-report.txt"

// gateLine is one line of the gate report.
type gateLine struct {
	Time   time.Time
	Test   string
	Result string // PASS, FAIL or MANUAL
	Note   string
}

func newAdbSelftestCmd(deps adbDeps) *cobra.Command {
	var gate bool
	var report string
	cmd := &cobra.Command{
		Use:   "selftest --gate",
		Short: "Run the automatic parts of the device control release gate and write ~/dc-gate-report.txt",
		Long: `Runs the automatic parts of the device control release gate (G0, G3, G3b, G3c,
G12) against this device and lists the manual steps (G1, G2, G4 to G11) with
what to check. Writes PASS, FAIL or MANUAL per test with timestamps to
~/dc-gate-report.txt. The report holds no secrets: no pairing code, no key,
no token, no command output beyond the baseline build values.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !gate {
				return adbUsage(cmd, errors.New("selftest needs --gate"))
			}
			path := report
			if path == "" {
				home, err := deps.home()
				if err != nil {
					return adbFail(cmd, fmt.Errorf("no home directory for the report: %w", err))
				}
				path = filepath.Join(home, GateReportName)
			}
			lines := runGate(cmd.Context(), deps)
			if err := writeGateReport(path, deps.now(), lines); err != nil {
				return adbFail(cmd, err)
			}
			failed := 0
			for _, l := range lines {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%-6s %-4s %s\n", l.Result, l.Test, l.Note)
				if l.Result == "FAIL" {
					failed++
				}
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Report written to %s.\n", path)
			if failed > 0 {
				return &exitCodeError{ExitError}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&gate, "gate", false, "run the release gate checks")
	cmd.Flags().StringVar(&report, "report", "", "write the report to this file instead of ~/"+GateReportName)
	return cmd
}

// gateShell runs one shell command and returns stdout and the exit code.
func gateShell(ctx context.Context, deps adbDeps, timeoutS int, argv ...string) (string, int, time.Duration, error) {
	var out, errb bytes.Buffer
	start := time.Now()
	code, err := adbRun(ctx, deps, ops.OpAdbShell, ops.AdbShellArgs{Argv: argv, TimeoutS: timeoutS},
		time.Duration(timeoutS)*time.Second, &out, &errb)
	return out.String(), code, time.Since(start), err
}

func runGate(ctx context.Context, deps adbDeps) []gateLine {
	var lines []gateLine
	add := func(test, result, note string) {
		lines = append(lines, gateLine{Time: deps.now(), Test: test, Result: result, Note: note})
	}
	pass := func(ok bool) string {
		if ok {
			return "PASS"
		}
		return "FAIL"
	}

	// Status first: without READY nothing else can pass.
	var st ops.AdbStatusResult
	raw, err := deps.backend().Call(ctx, ops.OpAdbStatus, ops.AdbStatusArgs{})
	if err == nil {
		err = json.Unmarshal(raw, &st)
	}
	ready := err == nil && st.DeviceControl.Enabled && st.DeviceControl.State == devctl.StateReady
	if err != nil {
		add("G12", "FAIL", "status: "+errText(err))
	} else {
		ks := st.DeviceControl.KeyStorage
		add("G12", pass(strings.HasPrefix(ks, "android-keystore (") || ks == "app-file"), "key_storage = "+orDashText(ks))
	}
	add("G0", "MANUAL", "Minion "+version.Version+"; check the values below: SDK 34, armeabi-v7a, ro.debuggable=0, app not debuggable")
	if !ready {
		add("G3", "FAIL", "device control is not READY: "+orDashText(st.DeviceControl.Reason))
	} else {
		out, code, _, err := gateShell(ctx, deps, 30, "getprop ro.build.fingerprint; getprop ro.build.version.sdk; getprop ro.product.cpu.abi; getprop ro.debuggable")
		if err != nil || code != 0 {
			add("G0", "FAIL", "baseline: "+errText(err))
		} else {
			v := strings.Split(strings.TrimSpace(out), "\n")
			for len(v) < 4 {
				v = append(v, "")
			}
			add("G0", pass(strings.TrimSpace(v[1]) == "34" && strings.TrimSpace(v[3]) == "0"),
				fmt.Sprintf("fingerprint=%s sdk=%s abi=%s debuggable=%s", safeValue(v[0]), safeValue(v[1]), safeValue(v[2]), safeValue(v[3])))
		}
		out, code, _, err = gateShell(ctx, deps, 30, "id -u")
		add("G3", pass(err == nil && code == 0 && strings.TrimSpace(out) == "2000"), fmt.Sprintf("id -u printed %q, exit %d %s", safeValue(out), code, errText(err)))

		_, code, _, err = gateShell(ctx, deps, 30, "sh", "-c", "exit 7")
		add("G3b", pass(err == nil && code == 7), fmt.Sprintf("exit code %d (want 7) %s", code, errText(err)))
		_, _, d, err := gateShell(ctx, deps, 3, "sleep", "10")
		add("G3b", pass(devctlCode(err) == "timeout" && d < 8*time.Second), fmt.Sprintf("--timeout 3 -- sleep 10 ended after %.1f s: %s", d.Seconds(), errText(err)))
		okN := 0
		for range 20 {
			if out, code, _, err := gateShell(ctx, deps, 10, "id -u"); err == nil && code == 0 && strings.TrimSpace(out) == "2000" {
				okN++
			}
		}
		add("G3b", pass(okN == 20), fmt.Sprintf("%d/20 fresh calls ok", okN))

		var png, errb bytes.Buffer
		code, err = adbRun(ctx, deps, ops.OpAdbScreencap, ops.AdbScreencapArgs{}, 60*time.Second, &png, &errb)
		add("G3c", pass(err == nil && code == 0 && bytes.HasPrefix(png.Bytes(), pngMagic)), fmt.Sprintf("screencap: %d bytes, PNG %v %s", png.Len(), bytes.HasPrefix(png.Bytes(), pngMagic), errText(err)))
		add("G3c", "MANUAL", "run: \"$MINION_BINARY\" adb input keyevent KEYCODE_HOME and check that the TV goes home")
	}
	_, ierr := devctl.InputCommand([]string{"keyevent", "HOME; reboot"})
	add("G3c", pass(errors.Is(ierr, devctl.ErrInvalidArgs)), "invalid input arguments are refused locally (exit 2), nothing sent")

	for _, m := range [][2]string{
		{"G1", "before enabling: PC Test-NetConnection <tv-ip> -Port 5555 and -Port 5037; pane: toybox nc -z 127.0.0.1 5037; echo $?"},
		{"G2", "enable on an unshared Worker, pair with the D-pad keypad; Settings lists minion@cc.nasz.minion; the pairing screen stayed open"},
		{"G3d", "update to a second gate RC in place, run this selftest again: G3 passes without pairing again"},
		{"G4", "wrong code, expired code, OS Forget (status says pair again), Wireless debugging off/on (port change), second device on the LAN"},
		{"G5", "pane: toybox nc -l -p 40123 & ; enter 40123 as connection port: ENDPOINT_UNVERIFIED, audit denied:provenance"},
		{"G6", "pane recorder loop to ~/dc-net.log; Wi-Fi off 3 min / on; hotspot; Ethernet-only; cold boot: fresh calls exit 255 \"needs Wi-Fi\" within 5 s"},
		{"G7", "repeat G1 after enable, disable, Wireless debugging toggle and reboot: no new 5555 or 5037 listener"},
		{"G8", "while enabled: app share switch, worker config share all, Hub grant: all refused with the reason"},
		{"G9", "shell -- sleep 600, then disable in the app, on the Hub, and OS Forget: the stream ends within 2 s with exit 255"},
		{"G10", "search tmux scrollback, state/ logs, audit log, Hub events and OTEL for the pairing codes and key material: no match"},
		{"G11", "compare the audit entries with the actions of G3 to G9; Hub panel and app agree"},
	} {
		add(m[0], "MANUAL", m[1])
	}
	return lines
}

func devctlCode(err error) string {
	if err == nil {
		return ""
	}
	if strings.Contains(err.Error(), "timeout") {
		return "timeout"
	}
	return "other"
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return "(" + firstLine(err.Error()) + ")"
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 160 {
		s = s[:160]
	}
	return s
}

// safeValue keeps a printed device value to one short printable line.
func safeValue(s string) string {
	s = strings.TrimSpace(s)
	var b strings.Builder
	for _, r := range s {
		if r >= 0x20 && r < 0x7f {
			b.WriteRune(r)
		}
		if b.Len() >= 160 {
			break
		}
	}
	return b.String()
}

func writeGateReport(path string, now time.Time, lines []gateLine) error {
	var b strings.Builder
	fmt.Fprintf(&b, "Minion device control gate report\nMinion %s, written %s\nResult per test: PASS, FAIL or MANUAL (to be done and recorded by the owner).\n\n",
		version.Version, now.UTC().Format(time.RFC3339))
	for _, l := range lines {
		fmt.Fprintf(&b, "%s  %-6s %-4s %s\n", l.Time.UTC().Format(time.RFC3339), l.Result, l.Test, l.Note)
	}
	return writeFileAtomic(path, []byte(b.String()))
}

func writeFileAtomic(path string, data []byte) error {
	tmp := path + ".part"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}
