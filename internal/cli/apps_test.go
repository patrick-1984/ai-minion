package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/patrick-1984/ai-minion/internal/client"
	"github.com/patrick-1984/ai-minion/internal/ops"
)

func appsDeps(b *fakeOpsBackend) opsDeps {
	return opsDeps{hub: func() (client.Backend, error) { return b, nil }}
}

func runApps(t *testing.T, deps opsDeps, args ...string) (string, error) {
	t.Helper()
	root := &cobra.Command{Use: "minion", SilenceUsage: true, SilenceErrors: true}
	addAppsCommands(root, deps)
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(args)
	err := root.Execute()
	return out.String(), err
}

func exitCodeOf(err error) int {
	var e *exitCodeError
	if errors.As(err, &e) {
		return e.code
	}
	if err != nil {
		return -1
	}
	return ExitOK
}

func TestAppsInstallSendsTheRequest(t *testing.T) {
	id := ops.NewUUIDv7()
	b := &fakeOpsBackend{call: func(op string, _ json.RawMessage) (json.RawMessage, error) {
		res, _ := json.Marshal(ops.AppInstallResult{Outcome: ops.AppInstallNeedsConfirmation, Path: "apps/x.apk",
			Package: "org.example.app", Label: "Example", VersionCode: 7, NewInstall: true, SignerSHA256: strings.Repeat("ab", 32)})
		return res, nil
	}}
	out, err := runApps(t, appsDeps(b), "apps", "install", "--target", id, "apps/x.apk", "--allow-new")
	if err != nil {
		t.Fatalf("err %v\n%s", err, out)
	}
	c := b.last(t)
	var a ops.AppInstallArgs
	if c.Op != ops.OpAppInstall || json.Unmarshal(c.Args, &a) != nil || a.WorkerID != id || a.Path != "apps/x.apk" || !a.AllowNew {
		t.Fatalf("call = %s %s", c.Op, c.Args)
	}
	for _, want := range []string{"outcome: needs_confirmation_on_device", "Example (org.example.app)", "new install",
		"signer sha256: " + strings.Repeat("ab", 32), "Confirm the install on the device: open Minion and press Install Example"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

func TestAppsInstallResolvesAWorkerName(t *testing.T) {
	id := ops.NewUUIDv7()
	b := &fakeOpsBackend{call: func(op string, _ json.RawMessage) (json.RawMessage, error) {
		if op == ops.OpWorkersList {
			return json.Marshal(ops.WorkersListResult{Workers: []ops.WorkerInfo{{ID: ops.NewUUIDv7(), Name: "other"}, {ID: id, Name: "tv"}}})
		}
		return json.Marshal(ops.AppInstallResult{Outcome: ops.AppInstallInstalled})
	}}
	if _, err := runApps(t, appsDeps(b), "apps", "install", "--target", "tv", "x.apk"); err != nil {
		t.Fatal(err)
	}
	var a ops.AppInstallArgs
	if c := b.last(t); json.Unmarshal(c.Args, &a) != nil || a.WorkerID != id || a.AllowNew {
		t.Fatalf("args = %s", c.Args)
	}
	if _, err := runApps(t, appsDeps(b), "apps", "install", "--target", "missing", "x.apk"); err == nil {
		t.Fatal("an unknown Worker name was accepted")
	}
}

func TestAppsInstallExitCodes(t *testing.T) {
	for outcome, want := range map[string]int{
		ops.AppInstallInstalled: ExitOK, ops.AppInstallNeedsConfirmation: ExitOK, ops.AppInstallInstalling: ExitOK,
		ops.AppInstallRefused: ExitFailedNotSent, ops.AppInstallFailed: ExitError,
	} {
		b := &fakeOpsBackend{call: func(string, json.RawMessage) (json.RawMessage, error) {
			return json.Marshal(ops.AppInstallResult{Outcome: outcome, Reason: "why"})
		}}
		_, err := runApps(t, appsDeps(b), "apps", "install", "--target", ops.NewUUIDv7(), "x.apk", "--json")
		if got := exitCodeOf(err); got != want {
			t.Errorf("%s: exit %d (%v), want %d", outcome, got, err, want)
		}
	}
}

func TestAppsInstallUsage(t *testing.T) {
	b := &fakeOpsBackend{call: func(string, json.RawMessage) (json.RawMessage, error) {
		t.Fatal("the backend was called")
		return nil, nil
	}}
	for _, args := range [][]string{
		{"apps", "install", "x.apk"},
		{"apps", "install", "--target", ops.NewUUIDv7()},
		{"apps", "install", "--target", ops.NewUUIDv7(), "../x.apk"},
		{"apps", "install", "--target", ops.NewUUIDv7(), "notes.txt"},
		{"apps", "install", "--target", ops.NewUUIDv7() + ":s", "x.apk"},
		{"apps", "status"},
	} {
		_, err := runApps(t, appsDeps(b), args...)
		var ue *usageError
		if !errors.As(err, &ue) {
			t.Errorf("%v: err %v, want a usage error", args, err)
		}
	}
}

func TestAppsStatusPrints(t *testing.T) {
	b := &fakeOpsBackend{call: func(string, json.RawMessage) (json.RawMessage, error) {
		return json.Marshal(ops.AppInstallStatus{Ready: false, BlockReason: "Install unknown apps is not allowed for Minion",
			Last:  &ops.AppInstallResult{Outcome: ops.AppInstallInstalled, Package: "org.example.app", VersionCode: 3},
			Files: []ops.AppInstallFile{{Path: "x.apk", Size: 42}}})
	}}
	out, err := runApps(t, appsDeps(b), "apps", "status", "--target", ops.NewUUIDv7())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"ready: no (Install unknown apps", "last install:", "  outcome: installed", "org.example.app", "42  x.apk"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if c := b.last(t); c.Op != ops.OpAppInstallStatus {
		t.Fatalf("op = %s", c.Op)
	}
}
