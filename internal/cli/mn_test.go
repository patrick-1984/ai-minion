package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestIsMN(t *testing.T) {
	for arg0, want := range map[string]bool{
		"mn": true, "/usr/local/bin/mn": true, "./mn": true, "mn.exe": true, `C:\bin\MN.EXE`: true, "MN": true,
		"minion": false, "/usr/bin/minion": false, "minion.exe": false, "mnx": false, "xmn": false, "": false, "mn.sh": false, "/opt/mn/minion": false,
	} {
		if got := IsMN(arg0); got != want {
			t.Errorf("IsMN(%q) = %t, want %t", arg0, got, want)
		}
	}
}

func TestRunUnderMnHasTheOpsAtTopLevel(t *testing.T) {
	for _, arg0 := range []string{"mn", "/usr/local/bin/mn", "mn.exe"} {
		var out, errb bytes.Buffer
		code := Run(context.Background(), []string{arg0, "send_text", "--help"}, &out, &errb)
		if code != 0 {
			t.Fatalf("%s: exit %d: %s", arg0, code, errb.String())
		}
		if !strings.Contains(out.String(), "mn send_text [flags]") || strings.Contains(out.String(), "minion send_text") {
			t.Errorf("%s: help names the wrong program:\n%s", arg0, out.String())
		}
		if !strings.Contains(out.String(), "--text") || !strings.Contains(out.String(), "--target") {
			t.Errorf("%s: help lacks the derived flags:\n%s", arg0, out.String())
		}
	}
	var out bytes.Buffer
	if code := Run(context.Background(), []string{"mn", "--help"}, &out, &out); code != 0 || !strings.Contains(out.String(), "sessions_list") || !strings.Contains(out.String(), "pipe_send") {
		t.Errorf("mn --help (exit %d) does not list the ops:\n%s", code, out.String())
	}
}

func TestRunUnderMinionKeepsTheName(t *testing.T) {
	var out, errb bytes.Buffer
	if code := Run(context.Background(), []string{"minion", "pipe_send", "--help"}, &out, &errb); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	if !strings.Contains(out.String(), "minion pipe_send [flags]") {
		t.Errorf("help:\n%s", out.String())
	}
}

func TestRunExitStatuses(t *testing.T) {
	var out, errb bytes.Buffer
	if code := Run(context.Background(), []string{"mn", "send_text", "--bogus"}, &out, &errb); code != ExitUsage {
		t.Errorf("usage error: exit %d", code)
	}
	if !strings.HasPrefix(errb.String(), "Error: ") {
		t.Errorf("stderr = %q", errb.String())
	}
	errb.Reset()
	if code := Run(context.Background(), []string{"minion", "no_such_command"}, &out, &errb); code == ExitOK {
		t.Error("unknown command succeeded")
	}
	if code := Run(context.Background(), nil, &out, &errb); code != ExitOK {
		t.Errorf("no args: exit %d", code)
	}
}
