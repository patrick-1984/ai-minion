package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"testing"
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

func TestListErrorTextHasNoPathsAndIsBounded(t *testing.T) {
	got := listErrorText(errors.New(`psmux: list-sessions: open C:\Users\pat\AppData\psmux\default: access denied` + "\nsecond line"))
	if strings.Contains(got, "Users") || strings.Contains(got, "second") || !strings.Contains(got, "<path>") || !strings.Contains(got, "access denied") {
		t.Errorf("listErrorText = %q", got)
	}
	if got := listErrorText(fmt.Errorf("run: %w", exec.ErrNotFound)); got != "the session host program was not found" {
		t.Errorf("not found = %q", got)
	}
	if got := listErrorText(context.DeadlineExceeded); got != "listing the sessions timed out" {
		t.Errorf("timeout = %q", got)
	}
	long := listErrorText(errors.New(strings.Repeat("x", 500)))
	if len(long) > maxListErrorText+3 {
		t.Errorf("long error is %d bytes", len(long))
	}
	var h *SessionListHealth
	h.Set(errors.New("x"))
	if h.Text() != "" {
		t.Error("a nil health has a text")
	}
	h = &SessionListHealth{}
	h.Set(errors.New("boom"))
	if h.Text() != "boom" {
		t.Errorf("text = %q", h.Text())
	}
	h.Set(nil)
	if h.Text() != "" {
		t.Errorf("a success did not clear the error: %q", h.Text())
	}
}

// The run mode and the session host travel over the local socket and show in
// `minion worker status`.
func TestStatusShowsRunModeAndSessionHost(t *testing.T) {
	health := &SessionListHealth{}
	health.Set(errors.New("psmux: server not running"))
	src := LiveStatusSource{RunMode: RunModeWindowsService, Health: health,
		Host: &SessionHostInfo{Kind: "conpty", Hidden: true, Reason: "ConPTY inside the Worker: the Worker runs as a Windows service (LocalSystem)"}}
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
		"cannot list them: psmux: server not running",
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
