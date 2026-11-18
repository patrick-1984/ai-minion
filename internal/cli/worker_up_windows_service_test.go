package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/version"
	"github.com/patrick-1984/ai-minion/internal/worker/agent"
	"github.com/patrick-1984/ai-minion/internal/worker/service"
)

// winController is a Windows service that is not installed yet and runs as
// soon as it is.
type winController struct {
	calls     *[]string
	installed *bool
}

func (c winController) Install() error {
	*c.calls = append(*c.calls, "install")
	*c.installed = true
	return nil
}
func (c winController) Start() error { *c.calls = append(*c.calls, "start"); return nil }
func (c winController) Uninstall() error {
	*c.calls = append(*c.calls, "uninstall")
	return errors.New("not installed")
}
func (c winController) Status() (service.Status, error) {
	*c.calls = append(*c.calls, "status")
	if !*c.installed {
		return service.StatusUnknown, service.ErrNotInstalled
	}
	return service.StatusRunning, nil
}

const verifiedSum = "0d5f0e3c2b1a99887766554433221100ffeeddccbbaa99887766554433221100"

// newWindowsUpFixture is the up fixture on a (simulated) Windows host whose
// service steps are fakes; copied records what the install copied.
func newWindowsUpFixture(t *testing.T) (f *upFixture, calls *[]string, copied *[2]string) {
	f = newUpFixture(t, false)
	calls, copied = &[]string{}, &[2]string{}
	f.deps.detectEnv = service.Env{GOOS: "windows"}
	f.deps.executable = func() (string, error) { return `C:\Users\ann\AppData\Local\Programs\minion\minion.exe`, nil }
	installed := new(bool)
	f.deps.service.NewController = func(string) (service.Controller, error) {
		return winController{calls: calls, installed: installed}, nil
	}
	f.deps.service.ServiceBinary = func(src, sum string) (string, error) {
		*copied = [2]string{src, sum}
		return `C:\Program Files\Minion\minion.exe`, nil
	}
	f.deps.service.ServiceKeyGone = func() bool { return true }
	f.deps.service.SetServiceEnv = func(map[string]string) error { return nil }
	f.deps.service.RemoveServiceKey = func() error { return nil }
	f.deps.service.StopService = func(context.Context) (bool, error) { return false, nil }
	f.deps.serviceRelease = func(context.Context, *config.Resolver, io.Writer) (version.Fetched, error) {
		return version.Fetched{Tag: "v1.2.3", Path: `C:\Users\ann\AppData\Local\minion\cache\v1.2.3\minion.exe`, SHA256: verifiedSum}, nil
	}
	return f, calls, copied
}

// The service runs the verified release, never the file the user ran.
func TestWorkerUpWindowsServiceInstallsTheVerifiedRelease(t *testing.T) {
	f, calls, copied := newWindowsUpFixture(t)
	out, err := f.run("--share-existing", "none")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if copied[0] != `C:\Users\ann\AppData\Local\minion\cache\v1.2.3\minion.exe` || copied[1] != verifiedSum {
		t.Errorf("copied %q with digest %q", copied[0], copied[1])
	}
	if !strings.Contains(strings.Join(*calls, ","), "install,start") {
		t.Errorf("calls = %v", *calls)
	}
	if !strings.Contains(out, "verifying the signed release") || !strings.Contains(out, `C:\Program Files\Minion\minion.exe`) {
		t.Errorf("output:\n%s", out)
	}
}

// The release is settled before the hooks run (an invitation is used once)
// and before any setting is saved: a release that cannot be verified changes
// nothing.
func TestWorkerUpWindowsServiceStopsWhenTheReleaseCannotBeVerified(t *testing.T) {
	f, calls, copied := newWindowsUpFixture(t)
	hookRan := false
	f.hook("enroll", func(agent.Settings) error { hookRan = true; return nil })
	var deadline bool
	f.deps.serviceRelease = func(ctx context.Context, _ *config.Resolver, _ io.Writer) (version.Fetched, error) {
		_, deadline = ctx.Deadline()
		return version.Fetched{}, version.ErrChecksum
	}
	_, err := f.run("--share-existing", "none", "--name", "lab")
	if !errors.Is(err, version.ErrChecksum) || !strings.Contains(err.Error(), "verified release") {
		t.Fatalf("err = %v", err)
	}
	if !deadline {
		t.Error("the release fetch has no deadline")
	}
	if len(*calls) != 0 || copied[0] != "" || hookRan {
		t.Errorf("something ran: calls %v, copied %v, hook %v", *calls, *copied, hookRan)
	}
	if _, serr := os.Stat(filepath.Join(f.stateDir(), "config.json")); serr == nil {
		t.Error("settings were saved")
	}
}

// A release mirror URL with a credential in it is not printed.
func TestWorkerUpWindowsServiceRedactsTheReleaseURL(t *testing.T) {
	f, _, _ := newWindowsUpFixture(t)
	secret := "https://ci:s3cr3t-token@mirror.example/api/s3cr3t-path-token/releases"
	f.deps.serviceRelease = func(context.Context, *config.Resolver, io.Writer) (version.Fetched, error) {
		return version.Fetched{}, fmt.Errorf("%w: %w", version.ErrNetwork, &url.Error{Op: "Get", URL: secret, Err: errors.New("connection reset")})
	}
	_, err := f.run("--share-existing", "none")
	if err == nil || strings.Contains(err.Error(), "s3cr3t") || !errors.Is(err, version.ErrNetwork) {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(err.Error(), "mirror.example") {
		t.Errorf("the host is not named: %v", err)
	}
}

// The bypass exists only in development builds.
func TestWorkerUpWindowsServiceRefusesTheBypassInARelease(t *testing.T) {
	f, calls, _ := newWindowsUpFixture(t)
	old := version.Version
	version.Version = "v1.2.3"
	t.Cleanup(func() { version.Version = old })
	_, err := f.run("--share-existing", "none", "--"+allowUnverifiedFlag)
	if err == nil || !strings.Contains(err.Error(), "only for development builds") {
		t.Fatalf("err = %v", err)
	}
	if len(*calls) != 0 {
		t.Errorf("calls %v", *calls)
	}
}

func TestWorkerUpWindowsServiceUnverifiedNeedsTheFlagAndWarns(t *testing.T) {
	f, _, copied := newWindowsUpFixture(t)
	f.deps.serviceRelease = func(context.Context, *config.Resolver, io.Writer) (version.Fetched, error) {
		t.Fatal("fetched although the flag was given")
		return version.Fetched{}, nil
	}
	if _, err := f.run("--share-existing", "none", "--"+allowUnverifiedFlag); err != nil {
		t.Fatal(err)
	}
	if copied[0] != `C:\Users\ann\AppData\Local\Programs\minion\minion.exe` || copied[1] != "" {
		t.Errorf("copied %v", *copied)
	}
	if !strings.Contains(f.stderr.String(), "without checking that it is a signed release") {
		t.Errorf("stderr = %s", f.stderr.String())
	}
}

// A development build ("dev") has no signed release to install.
func TestFetchServiceReleaseRefusesADevelopmentBuild(t *testing.T) {
	if version.Version != "dev" {
		t.Skip("built as a release")
	}
	res := &config.Resolver{Role: config.ScopeWorker, LookupEnv: func(string) (string, bool) { return "", false }}
	_, err := fetchServiceRelease(context.Background(), res, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "not a release") || !strings.Contains(err.Error(), "--"+allowUnverifiedFlag) {
		t.Fatalf("err = %v", err)
	}
}
