package agent

import (
	"strings"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/transport"
)

func TestDecideTmuxHost(t *testing.T) {
	const def = "/tmp/tmux-1000/default"
	cases := []struct {
		name   string
		f      tmuxHostFacts
		hidden bool
		reason string
	}{
		{"default socket", tmuxHostFacts{Socket: def, DefaultSocket: def, User: "pat"}, false, "default tmux server of pat"},
		{"default socket, unclean path", tmuxHostFacts{Socket: "/tmp/tmux-1000//default", DefaultSocket: def}, false, "tmux ls"},
		{"user service on the default socket", tmuxHostFacts{Socket: def, DefaultSocket: def, ServiceManager: true}, false, "service manager"},
		{"private socket", tmuxHostFacts{Socket: "/x/y", DefaultSocket: def, User: "pat"}, true, "private tmux socket /x/y"},
		{"no default socket known", tmuxHostFacts{Socket: def}, true, "private tmux socket"},
		{"no socket", tmuxHostFacts{DefaultSocket: def}, true, "not known"},
		{"root under a service manager", tmuxHostFacts{Socket: def, DefaultSocket: def, Root: true, ServiceManager: true}, true, "root"},
		{"root in the foreground", tmuxHostFacts{Socket: def, DefaultSocket: def, Root: true, User: "root"}, false, "default tmux server of root"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := decideTmuxHost(c.f)
			if got.Kind != transport.SessionHostTmux || got.Hidden != c.hidden || !strings.Contains(got.Reason, c.reason) {
				t.Errorf("decideTmuxHost(%+v) = %+v, want hidden=%v with %q in the reason", c.f, got, c.hidden, c.reason)
			}
		})
	}
}

func TestDecideWindowsHost(t *testing.T) {
	got := decideWindowsHost(windowsHostFacts{Psmux: true, User: `ASUS\pat`})
	if got.Kind != transport.SessionHostPsmux || got.Hidden || !strings.Contains(got.Reason, `ASUS\pat`) {
		t.Errorf("psmux = %+v, want visible psmux", got)
	}
	got = decideWindowsHost(windowsHostFacts{ConPTYReason: "psmux is not installed (winget install psmux)"})
	if got.Kind != transport.SessionHostConPTY || !got.Hidden || got.Reason != "ConPTY inside the Worker: psmux is not installed (winget install psmux)" {
		t.Errorf("conpty = %+v, want hidden ConPTY with the reason", got)
	}
	if got := decideWindowsHost(windowsHostFacts{}); !got.Hidden || got.Reason != "ConPTY inside the Worker" {
		t.Errorf("conpty without a reason = %+v", got)
	}
}

func TestSessionHostInfoApplyTo(t *testing.T) {
	f := transport.WorkerFacts{Build: "v0.10.0"}
	(&SessionHostInfo{}).applyTo(&f)
	var nilInfo *SessionHostInfo
	nilInfo.applyTo(&f)
	if f.SessionHost != "" || f.SessionsHidden || f.SessionHostReason != "" {
		t.Fatalf("an unknown info changed the facts: %+v", f)
	}
	(&SessionHostInfo{Kind: "conpty", Hidden: true, Reason: "r"}).applyTo(&f)
	if f.SessionHost != "conpty" || !f.SessionsHidden || f.SessionHostReason != "r" || f.Build != "v0.10.0" {
		t.Fatalf("facts = %+v", f)
	}
}
