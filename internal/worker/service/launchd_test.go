package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPlistGolden(t *testing.T) {
	got, err := Plist(PlistParams{Binary: "/opt/homebrew/bin/minion", Home: "/Users/ann", LogFile: "/Users/ann/Library/Application Support/minion/worker.log"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"<string>worker</string>", "<string>run</string>", "<key>KeepAlive</key>", "<key>RunAtLoad</key>", LaunchdLabel} {
		if !strings.Contains(got, want) {
			t.Errorf("plist lacks %q", want)
		}
	}
	golden(t, "minion-worker.plist.golden", got)
}

func TestPlistEscapesAndRefusesUnsafeBinary(t *testing.T) {
	got, err := Plist(PlistParams{Binary: "/opt/minion", Home: "/Users/a&b<c>", LogFile: "/x"})
	if err != nil || !strings.Contains(got, "/Users/a&amp;b&lt;c&gt;") {
		t.Fatalf("err = %v\n%s", err, got)
	}
	if _, err := Plist(PlistParams{Binary: "/opt/a b/minion"}); err == nil {
		t.Error("a binary path with a space was accepted")
	}
}

func launchdReq(home string) Request {
	return Request{Manager: ManagerLaunchd, Scope: ScopeUser, Binary: "/opt/homebrew/bin/minion", StateDir: home + "/state",
		Target: Target{User: "ann", UID: 501, GID: 20, Home: home}}
}

func TestInstallLaunchdAgent(t *testing.T) {
	r := &fakeRunner{answer: func(line string) (string, error) {
		if strings.HasPrefix(line, "launchctl print") {
			return "state = running\n", nil
		}
		return "", nil
	}}
	d := testDeps(t, r)
	rec := &recChowner{t: t, uid: 501, gid: 20}
	d.Chown = rec
	home := t.TempDir()
	res, err := Install(context.Background(), d, launchdReq(home), nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Path != PlistPath(home) || res.Unit != LaunchdLabel || res.Manager != ManagerLaunchd {
		t.Errorf("result = %+v", res)
	}
	data, err := os.ReadFile(res.Path)
	if err != nil || !strings.Contains(string(data), "<string>/opt/homebrew/bin/minion</string>") {
		t.Fatalf("plist: %v\n%s", err, data)
	}
	// The directories that did not exist go to the target too, not only the plist.
	wantOwned := []string{filepath.Join(home, "Library"), filepath.Dir(res.Path), res.Path}
	if strings.Join(rec.got, "|") != strings.Join(wantOwned, "|") {
		t.Errorf("chowned = %v, want %v", rec.got, wantOwned)
	}
	tgt := "gui/501/" + LaunchdLabel
	want := []string{
		"launchctl bootout " + tgt,
		"launchctl bootstrap gui/501 " + res.Path,
		"launchctl enable " + tgt,
		"launchctl kickstart -k " + tgt,
	}
	got := r.lines()
	if len(got) < len(want) || strings.Join(got[:len(want)], "|") != strings.Join(want, "|") {
		t.Fatalf("commands:\n%s", strings.Join(got, "\n"))
	}
}

func TestInstallLaunchdBootoutFailureIsIgnoredButBootstrapFailureIsNot(t *testing.T) {
	r := &fakeRunner{answer: func(line string) (string, error) {
		switch {
		case strings.HasPrefix(line, "launchctl bootout"):
			return "Could not find service", errors.New("exit status 113")
		case strings.HasPrefix(line, "launchctl bootstrap"):
			return "Bootstrap failed: 5: Input/output error", errors.New("exit status 5")
		}
		return "", nil
	}}
	_, err := Install(context.Background(), testDeps(t, r), launchdReq(t.TempDir()), nil)
	if err == nil || !strings.Contains(err.Error(), "bootstrap") || !strings.Contains(err.Error(), "Input/output") {
		t.Fatalf("err = %v", err)
	}
}

func TestInstallLaunchdNotRunning(t *testing.T) {
	r := &fakeRunner{answer: func(line string) (string, error) {
		if strings.HasPrefix(line, "launchctl print") {
			return "state = waiting\n", nil
		}
		return "", nil
	}}
	_, err := Install(context.Background(), testDeps(t, r), launchdReq(t.TempDir()), nil)
	if err == nil || !strings.Contains(err.Error(), "not running") {
		t.Fatalf("err = %v", err)
	}
}

func TestPlistPinsTheStateDir(t *testing.T) {
	got, err := Plist(PlistParams{Binary: "/opt/minion", Home: "/Users/ann", LogFile: "/x", StateDir: "/Users/ann/s&t"})
	if err != nil || !strings.Contains(got, "<key>MINION_STATE_DIR</key>\n\t\t<string>/Users/ann/s&amp;t</string>") {
		t.Fatalf("err = %v\n%s", err, got)
	}
	if _, err := Plist(PlistParams{Binary: "/opt/minion", StateDir: "relative"}); err == nil {
		t.Error("a relative state dir was accepted")
	}
}
