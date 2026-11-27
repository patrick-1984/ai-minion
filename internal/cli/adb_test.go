package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/patrick-1984/ai-minion/internal/client"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// fakeAdbWorker answers the adb_* ops like a running Android Worker.
type fakeAdbWorker struct {
	status ops.DeviceControlStatus
	shell  func(args ops.AdbShellArgs) ([]ops.AdbOutput, int, error)
	calls  []string
	png    []byte
}

func (f *fakeAdbWorker) Call(ctx context.Context, op string, args any) (json.RawMessage, error) {
	return f.Stream(ctx, op, args, nil)
}

func (f *fakeAdbWorker) Stream(_ context.Context, op string, args any, on func(json.RawMessage) error) (json.RawMessage, error) {
	f.calls = append(f.calls, op)
	emit := func(outs []ops.AdbOutput) error {
		for _, o := range outs {
			b, _ := json.Marshal(o)
			if on != nil {
				if err := on(b); err != nil {
					return err
				}
			}
		}
		return nil
	}
	switch op {
	case ops.OpAdbStatus:
		return json.Marshal(ops.AdbStatusResult{DeviceControl: f.status})
	case ops.OpAdbShell:
		a := args.(ops.AdbShellArgs)
		outs, code, err := f.shell(a)
		if err != nil {
			return nil, err
		}
		if err := emit(outs); err != nil {
			return nil, err
		}
		return json.Marshal(ops.AdbRunResult{ExitCode: code})
	case ops.OpAdbScreencap:
		if err := emit([]ops.AdbOutput{{Stream: "stdout", Data: f.png}}); err != nil {
			return nil, err
		}
		return json.Marshal(ops.AdbRunResult{})
	case ops.OpAdbInput:
		return json.Marshal(ops.AdbRunResult{})
	}
	return nil, proto.NewError(proto.CodeUnsupportedOp, "no")
}

func runAdb(t *testing.T, f *fakeAdbWorker, home string, args ...string) (int, string, string) {
	t.Helper()
	deps := adbDeps{backend: func() client.Backend { return f }, home: func() (string, error) { return home, nil },
		now: func() time.Time { return time.Date(2026, 11, 27, 10, 0, 0, 0, time.UTC) }}
	root := &cobra.Command{Use: "minion", SilenceUsage: true, SilenceErrors: true}
	root.AddCommand(newAdbCmd(deps))
	var out, errb bytes.Buffer
	code := execute(context.Background(), root, append([]string{"adb"}, args...), &out, &errb)
	return code, out.String(), errb.String()
}

func readyWorker() *fakeAdbWorker {
	return &fakeAdbWorker{
		status: ops.DeviceControlStatus{Available: true, Enabled: true, State: "READY", Port: 37000, KeyStorage: "android-keystore (TEE)"},
		shell: func(a ops.AdbShellArgs) ([]ops.AdbOutput, int, error) {
			cmd := strings.Join(a.Argv, " ")
			switch {
			case cmd == "id -u":
				return []ops.AdbOutput{{Stream: "stdout", Data: []byte("2000\n")}}, 0, nil
			case cmd == "sh -c exit 7":
				return nil, 7, nil
			case cmd == "sleep 10":
				return nil, 0, proto.NewError(proto.CodeConflict, "timeout")
			case strings.HasPrefix(cmd, "getprop"):
				return []ops.AdbOutput{{Stream: "stdout", Data: []byte("vendor/x:14/1:user/release-keys\n34\narmeabi-v7a\n0\n")}}, 0, nil
			}
			return []ops.AdbOutput{{Stream: "stderr", Data: []byte("err")}}, 1, nil
		},
		png: append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 100)...),
	}
}

func TestAdbShellExitCodes(t *testing.T) {
	f := readyWorker()
	code, out, _ := runAdb(t, f, t.TempDir(), "shell", "--", "id", "-u")
	if code != 0 || out != "2000\n" {
		t.Fatalf("code %d out %q", code, out)
	}
	if code, _, _ = runAdb(t, f, t.TempDir(), "shell", "--", "sh", "-c", "exit 7"); code != 7 {
		t.Fatalf("remote exit code: %d", code)
	}
	code, _, errText := runAdb(t, f, t.TempDir(), "shell", "--timeout", "3", "--", "sleep", "10")
	if code != 255 || !strings.Contains(errText, "timeout") {
		t.Fatalf("timeout: %d %q", code, errText)
	}
	f.shell = func(ops.AdbShellArgs) ([]ops.AdbOutput, int, error) {
		return nil, 0, proto.NewError(proto.CodeForbidden, "device control is disabled; enable it in the Minion app on the device")
	}
	code, _, errText = runAdb(t, f, t.TempDir(), "shell", "--", "id")
	if code != 255 || errText != "minion adb: device control is disabled; enable it in the Minion app on the device\n" {
		t.Fatalf("disabled: %d %q", code, errText)
	}
	n := len(f.calls)
	if code, _, _ = runAdb(t, f, t.TempDir(), "shell"); code != 2 || len(f.calls) != n {
		t.Fatalf("empty shell: %d", code)
	}
	if code, _, _ = runAdb(t, f, t.TempDir(), "shell", "--timeout", "4000", "--", "id"); code != 2 {
		t.Fatalf("timeout cap: %d", code)
	}
}

func TestAdbInputRefusedLocally(t *testing.T) {
	f := readyWorker()
	code, _, errText := runAdb(t, f, t.TempDir(), "input", "keyevent", "HOME;reboot")
	if code != 2 || !strings.Contains(errText, "invalid arguments") || len(f.calls) != 0 {
		t.Fatalf("code %d %q calls %v", code, errText, f.calls)
	}
	if code, _, _ = runAdb(t, f, t.TempDir(), "input", "keyevent", "KEYCODE_HOME"); code != 0 {
		t.Fatalf("valid input: %d", code)
	}
}

func TestAdbScreencapWritesInsideTheCwd(t *testing.T) {
	f := readyWorker()
	dir := t.TempDir()
	t.Chdir(dir)
	code, out, _ := runAdb(t, f, dir, "screencap", "s.png")
	if code != 0 || !strings.Contains(out, "Wrote s.png (108 bytes)") {
		t.Fatalf("%d %q", code, out)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "s.png")); err != nil || len(b) != 108 {
		t.Fatal(err)
	}
	for _, bad := range []string{"../s.png", "/tmp/s.png"} {
		if code, _, _ := runAdb(t, f, dir, "screencap", bad); code != 2 {
			t.Fatalf("%s: %d", bad, code)
		}
	}
	f.png = []byte("not a png")
	if code, _, _ := runAdb(t, f, dir, "screencap", "t.png"); code != 255 {
		t.Fatalf("bad png: %d", code)
	}
	if _, err := os.Stat(filepath.Join(dir, "t.png")); err == nil {
		t.Fatal("a failed screencap left a file")
	}
}

func TestAdbStatusText(t *testing.T) {
	f := readyWorker()
	f.status.KeyStorage = "app-file"
	code, out, _ := runAdb(t, f, t.TempDir(), "status")
	for _, want := range []string{"Enabled:        yes", "State:          READY", "Port:           37000 (127.0.0.1)", "Every terminal of this Worker could read it."} {
		if code != 0 || !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
	code, out, _ = runAdb(t, f, t.TempDir(), "status", "--json")
	var d ops.DeviceControlStatus
	if code != 0 || json.Unmarshal([]byte(out), &d) != nil || d.Port != 37000 {
		t.Fatalf("%d %s", code, out)
	}
}

func TestAdbStatusShowsTheTransport(t *testing.T) {
	f := readyWorker()
	f.status.Transport, f.status.Port = "legacy-loopback", 5555
	if code, out, _ := runAdb(t, f, t.TempDir(), "status"); code != 0 || !strings.Contains(out, "Transport:      legacy adb (127.0.0.1:5555)") {
		t.Fatalf("%d\n%s", code, out)
	}
	f.status.Transport = "tls-loopback"
	if code, out, _ := runAdb(t, f, t.TempDir(), "status"); code != 0 || !strings.Contains(out, "Transport:      Wireless debugging (TLS on 127.0.0.1)") {
		t.Fatalf("%d\n%s", code, out)
	}
}

func TestAdbSelftestGateReport(t *testing.T) {
	f := readyWorker()
	home := t.TempDir()
	code, out, _ := runAdb(t, f, home, "selftest", "--gate")
	b, err := os.ReadFile(filepath.Join(home, GateReportName))
	if err != nil {
		t.Fatal(err)
	}
	rep := string(b)
	for _, want := range []string{"PASS   G3   id -u printed \"2000\"", "PASS   G3b  exit code 7", "PASS   G3b  20/20 fresh calls ok",
		"PASS   G3c  screencap", "PASS   G12  key_storage = android-keystore (TEE)", "PASS   G0   fingerprint=vendor/x:14/1:user/release-keys sdk=34",
		"MANUAL G5", "MANUAL G10", "2026-11-27T10:00:00Z"} {
		if !strings.Contains(rep, want) {
			t.Fatalf("missing %q in:\n%s", want, rep)
		}
	}
	if code != 0 || !strings.Contains(out, "Report written to") {
		t.Fatalf("%d %s", code, out)
	}
	fi, _ := os.Stat(filepath.Join(home, GateReportName))
	if fi.Mode().Perm() != 0o600 {
		t.Fatal(fi.Mode())
	}
	// Not ready: the automatic tests fail and the command exits 1.
	f.status = ops.DeviceControlStatus{Available: true, State: "OFF", Reason: "device control is disabled"}
	if code, _, _ := runAdb(t, f, home, "selftest", "--gate"); code != 1 {
		t.Fatalf("not ready: %d", code)
	}
	if code, _, _ := runAdb(t, f, home, "selftest"); code != 2 {
		t.Fatalf("without --gate: %d", code)
	}
}

// worker config share refuses every mode but none while device control is
// on, and changes nothing.
func TestConfigShareRefusedWhileDeviceControlIsOn(t *testing.T) {
	dir := t.TempDir()
	cd := testConfigDeps(nil)
	cd.deviceControlOn = func(context.Context, string) bool { return true }
	for _, mode := range []string{"all", "later", "select"} {
		args := []string{"share", mode}
		if mode == "select" {
			args = append(args, "build")
		}
		if _, _, err := configCmd(t, cd, dir, args...); err == nil || !strings.Contains(err.Error(), "device control is on") {
			t.Fatalf("%s: %v", mode, err)
		}
	}
	if _, _, err := configCmd(t, cd, dir, "share", "none"); err != nil {
		t.Fatalf("none: %v", err)
	}
	if sel := loadShare(t, dir); sel.Mode != "none" {
		t.Fatalf("saved %+v", sel)
	}
}
