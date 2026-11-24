package cli

import (
	"testing"

	"github.com/patrick-1984/ai-minion/internal/worker/agentdetect"
)

func TestHookTmuxTarget(t *testing.T) {
	socket, pid := hookTmuxTarget("/tmp/test,with,commas,123,4")
	if socket != "/tmp/test,with,commas" || pid != 123 {
		t.Fatalf("target = %q, %d", socket, pid)
	}
	for _, value := range []string{"", "/tmp/a,123", "relative,123,0", "/tmp/a,0,0", "/tmp/a,123,-1"} {
		if socket, pid := hookTmuxTarget(value); socket != "" || pid != 0 {
			t.Errorf("%q yielded %q, %d", value, socket, pid)
		}
	}
}

func TestHookHarnessAncestor(t *testing.T) {
	procs := []agentdetect.Proc{
		{PID: 10, PPID: 5, State: "S", TTY: "1", PGRP: 10, TPGID: 20, Args: []string{"bash"}},
		{PID: 20, PPID: 10, State: "S", TTY: "1", PGRP: 20, TPGID: 20, Args: []string{"claude"}},
		{PID: 30, PPID: 20, State: "S", Args: []string{"sh", "-c", "minion hook"}},
		{PID: 40, PPID: 30, State: "S", Args: []string{"minion"}},
	}
	if got := hookHarnessAncestor(procs, 40, 10, 5, "claude"); got != 20 {
		t.Fatalf("ancestor = %d, want 20", got)
	}
	for _, tc := range []struct {
		name string
		edit func([]agentdetect.Proc)
	}{
		{"unrelated pane", func(p []agentdetect.Proc) { p[1].PPID = 12 }},
		{"unrelated server", func(p []agentdetect.Proc) { p[0].PPID = 6 }},
		{"background harness", func(p []agentdetect.Proc) { p[1].TPGID = 10 }},
		{"separate terminal", func(p []agentdetect.Proc) { p[1].TTY = "2" }},
		{"stopped harness", func(p []agentdetect.Proc) { p[1].State = "T" }},
		{"different harness", func(p []agentdetect.Proc) { p[1].Args = []string{"codex"} }},
		{"command-line mention", func(p []agentdetect.Proc) { p[1].Args = []string{"sh", "-c", "claude"} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cp := append([]agentdetect.Proc(nil), procs...)
			tc.edit(cp)
			if got := hookHarnessAncestor(cp, 40, 10, 5, "claude"); got != 0 {
				t.Errorf("ancestor = %d, want rejection", got)
			}
		})
	}
}
