package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/transport"
)

func TestUnixRunMode(t *testing.T) {
	for _, tc := range []struct {
		env  map[string]string
		want string
	}{
		{map[string]string{}, RunModeForeground},
		{map[string]string{"INVOCATION_ID": "abc"}, RunModeSystemd},
		{map[string]string{"XPC_SERVICE_NAME": "0"}, RunModeForeground},
		{map[string]string{"XPC_SERVICE_NAME": "com.minion.worker"}, RunModeLaunchd},
	} {
		if got := unixRunMode(func(k string) string { return tc.env[k] }); got != tc.want {
			t.Errorf("unixRunMode(%v) = %q, want %q", tc.env, got, tc.want)
		}
	}
}

// The hello gets a fixed reason code, never the error itself; the local
// status keeps the error, bounded to its first line.
func TestListErrorCodesAndLocalText(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want string
	}{
		{fmt.Errorf("run: %w", exec.ErrNotFound), transport.ListFailedNotFound},
		{context.DeadlineExceeded, transport.ListFailedTimeout},
		{&exec.ExitError{}, transport.ListFailedExec},
		{errors.New("tmux: list-panes row 2 has 3 fields, want 9"), transport.ListFailedFormat},
		{errors.New(`psmux: open C:\Users\pat\x: access denied`), transport.ListFailedExec},
	} {
		if got := listErrorCode(tc.err); got != tc.want {
			t.Errorf("listErrorCode(%v) = %q, want %q", tc.err, got, tc.want)
		}
	}
	long := listErrorDetail(errors.New(strings.Repeat("x", 500) + "\nsecond"))
	if len(long) > maxListErrorText+3 || strings.Contains(long, "second") {
		t.Errorf("long error is %d bytes", len(long))
	}
	var h *SessionListHealth
	h.Set(errors.New("x"))
	if h.Code() != "" || h.LocalText() != "" {
		t.Error("a nil health has a text")
	}
	h = &SessionListHealth{}
	h.Set(errors.New(`psmux: open C:\Users\pat: boom`))
	if h.Code() != transport.ListFailedExec || !strings.Contains(h.LocalText(), "boom") {
		t.Errorf("code = %q, local text = %q", h.Code(), h.LocalText())
	}
	var f transport.WorkerFacts
	applyListHealth(&f, h)
	if f.SessionsListErrorCode != transport.ListFailedExec || strings.Contains(f.SessionsListError, "Users") || strings.Contains(f.SessionsListError, "boom") {
		t.Errorf("hello facts = %+v", f)
	}
	h.Set(nil)
	if h.LocalText() != "" || h.Code() != "" {
		t.Errorf("a success did not clear the error: %q", h.LocalText())
	}
}

// The run mode and the session host travel over the local socket and show in
// `minion worker status`.
func TestStatusShowsRunModeAndSessionHost(t *testing.T) {
	health := &SessionListHealth{}
	health.Set(errors.New("psmux: server not running"))
	src := LiveStatusSource{RunMode: RunModeWindowsService, Health: health,
		Host: &SessionHostInfo{Kind: "conpty", Hidden: true, Reason: "ConPTY inside the Worker: the Worker runs as a Windows service (LocalSystem)", Detail: "user NT AUTHORITY\\SYSTEM"}}
	live, err := src.LiveStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	back, err := NewLiveStatusReply("/state", live).LiveStatus()
	if err != nil {
		t.Fatal(err)
	}
	r := BuildStatus(context.Background(), StatusInputs{Version: "v0.10.0", Name: "asus", Live: fixedLive{back}})
	var buf bytes.Buffer
	if err := r.WriteText(&buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{
		"Run mode:", "Windows service",
		"Session host:", "ConPTY inside the Worker (hidden): the Worker runs as a Windows service (LocalSystem)",
		"could not list them at its latest look: psmux: server not running",
		`(user NT AUTHORITY\SYSTEM)`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("status lacks %q:\n%s", want, out)
		}
	}
	if r.RunMode != RunModeWindowsService || r.SessionHost != "conpty" || !r.SessionsHidden {
		t.Errorf("report = %+v", r)
	}
}

type fixedLive struct{ s LiveStatus }

func (f fixedLive) LiveStatus(context.Context) (LiveStatus, error) { return f.s, nil }
