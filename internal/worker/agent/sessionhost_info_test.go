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
		code   string
	}{
		{"default socket", tmuxHostFacts{Socket: def, DefaultSocket: def, User: "pat"}, false, transport.HostReasonTmuxDefault},
		{"default socket, unclean path", tmuxHostFacts{Socket: "/tmp/tmux-1000//default", DefaultSocket: def}, false, transport.HostReasonTmuxDefault},
		{"user service on the default socket", tmuxHostFacts{Socket: def, DefaultSocket: def, ServiceManager: true}, false, transport.HostReasonTmuxDefaultService},
		{"private socket", tmuxHostFacts{Socket: "/x/y", DefaultSocket: def, User: "pat"}, true, transport.HostReasonPrivateSocket},
		{"no default socket known", tmuxHostFacts{Socket: def}, true, transport.HostReasonPrivateSocket},
		{"no socket", tmuxHostFacts{DefaultSocket: def}, true, transport.HostReasonSocketUnknown},
		{"root under a service manager", tmuxHostFacts{Socket: def, DefaultSocket: def, Root: true, ServiceManager: true}, true, transport.HostReasonRootService},
		{"root in the foreground", tmuxHostFacts{Socket: def, DefaultSocket: def, Root: true, User: "root"}, false, transport.HostReasonTmuxDefault},
		// Docker, self-contained: the container's own default tmux server.
		{"container, own tmux", tmuxHostFacts{Socket: def, DefaultSocket: def, Container: true}, true, transport.HostReasonContainerTmux},
		// Docker, the host user's socket mounted at /tmux/default (compose sets yes).
		// v0.9.3 Compose: the documented mount without sessions_visible.
		{"container, host socket, auto", tmuxHostFacts{Socket: "/tmux/default", DefaultSocket: def, Container: true, Visible: "auto"}, false, transport.HostReasonContainerHostSocket},
		{"container, host socket, unset", tmuxHostFacts{Socket: "/tmux//default", DefaultSocket: def, Container: true}, false, transport.HostReasonContainerHostSocket},
		{"container, other socket, auto", tmuxHostFacts{Socket: "/sock/default", DefaultSocket: def, Container: true, Visible: "auto"}, true, transport.HostReasonPrivateSocket},
		{"host, /tmux/default outside a container", tmuxHostFacts{Socket: "/tmux/default", DefaultSocket: def}, true, transport.HostReasonPrivateSocket},
		{"container, host socket, no", tmuxHostFacts{Socket: "/tmux/default", DefaultSocket: def, Container: true, Visible: "no"}, true, transport.HostReasonHiddenSetting},
		{"container, host socket, yes", tmuxHostFacts{Socket: "/tmux/default", DefaultSocket: def, Container: true, Visible: "yes"}, false, transport.HostReasonVisibleSetting},
		{"container, own tmux, no", tmuxHostFacts{Socket: def, DefaultSocket: def, Container: true, Visible: "no"}, true, transport.HostReasonHiddenSetting},
		{"no on the default server", tmuxHostFacts{Socket: def, DefaultSocket: def, Visible: "no"}, true, transport.HostReasonHiddenSetting},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := decideTmuxHost(c.f)
			if got.Kind != transport.SessionHostTmux || got.Hidden != c.hidden || got.Code != c.code || got.Reason != transport.HostReasonText(c.code) {
				t.Errorf("decideTmuxHost(%+v) = %+v, want hidden=%v code %q", c.f, got, c.hidden, c.code)
			}
		})
	}
	// The path and the user name stay in the local detail, out of the hello.
	got := decideTmuxHost(tmuxHostFacts{Socket: "/home/pat/s", DefaultSocket: def, User: "pat"})
	var f transport.WorkerFacts
	got.applyTo(&f)
	if strings.Contains(f.SessionHostReason, "/home") || strings.Contains(f.SessionHostReason, "pat") || !strings.Contains(got.localReason(), "/home/pat/s") {
		t.Errorf("hello reason %q, local reason %q", f.SessionHostReason, got.localReason())
	}
}

func TestDecideWindowsHost(t *testing.T) {
	got := decideWindowsHost(windowsHostFacts{Psmux: true, User: `ASUS\pat`})
	if got.Kind != transport.SessionHostPsmux || got.Hidden || got.Code != transport.HostReasonPsmuxDefault || strings.Contains(got.Reason, "pat") || !strings.Contains(got.localReason(), `ASUS\pat`) {
		t.Errorf("psmux = %+v, want visible psmux with the user only in the detail", got)
	}
	got = decideWindowsHost(windowsHostFacts{ConPTYCode: transport.HostReasonConPTYNoPsmux})
	if got.Kind != transport.SessionHostConPTY || !got.Hidden || got.Reason != "ConPTY inside the Worker: psmux is not installed (winget install psmux)" {
		t.Errorf("conpty = %+v, want hidden ConPTY with the reason", got)
	}
	got = decideWindowsHost(windowsHostFacts{ConPTYCode: transport.HostReasonConPTYPsmuxFailed, ConPTYDetail: `C:\Users\pat\psmux.exe: denied`})
	if strings.Contains(got.Reason, "Users") || !strings.Contains(got.localReason(), "denied") {
		t.Errorf("psmux failure = %+v", got)
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
	(&SessionHostInfo{Kind: "conpty", Hidden: true, Code: "c", Reason: "r", Detail: "secret"}).applyTo(&f)
	if f.SessionHost != "conpty" || !f.SessionsHidden || f.SessionHostReason != "r" || f.SessionHostReasonCode != "c" || f.Build != "v0.10.0" {
		t.Fatalf("facts = %+v", f)
	}
}

// Only ConPTY sessions end with the Worker: psmux sessions on Windows and tmux
// sessions elsewhere must not veto a self-update.
func TestRestartEndsSessionsOnlyForConPTY(t *testing.T) {
	for _, tc := range []struct {
		goos, kind string
		want       bool
	}{
		{"windows", "conpty", true},
		{"windows", "", true},
		{"windows", "psmux", false},
		{"linux", "tmux", false},
		{"linux", "", false},
		{"darwin", "conpty", false},
	} {
		if got := restartEndsSessions(tc.goos, tc.kind); got != tc.want {
			t.Errorf("restartEndsSessions(%q, %q) = %v, want %v", tc.goos, tc.kind, got, tc.want)
		}
	}
}
