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

const (
	verifiedSum  = "0d5f0e3c2b1a99887766554433221100ffeeddccbbaa99887766554433221100"
	stagingDir   = `C:\Program Files\Minion.staging-test`
	releaseInDir = stagingDir + `\v1.2.3\minion.exe`
)

// winFixture is the up fixture on a (simulated) Windows host whose service
// steps are fakes, running as release v1.2.3.
type winFixture struct {
	*upFixture
	calls    *[]string
	install  *service.BinaryInstall
	cacheDir *string
}

func newWindowsUpFixture(t *testing.T) winFixture {
	t.Helper()
	old := version.Version
	version.Version = "v1.2.3"
	t.Cleanup(func() { version.Version = old })
	w := winFixture{upFixture: newUpFixture(t, false), calls: &[]string{}, install: &service.BinaryInstall{}, cacheDir: new(string)}
	installed := new(bool)
	w.deps.detectEnv = service.Env{GOOS: "windows"}
	w.deps.executable = func() (string, error) { return `C:\Users\ann\AppData\Local\Programs\minion\minion.exe`, nil }
	w.deps.service.NewController = func(string) (service.Controller, error) {
		return winController{calls: w.calls, installed: installed}, nil
	}
	w.deps.service.ServiceBinary = func(bi service.BinaryInstall, activate func(string) error) (string, error) {
		*w.install = bi
		const dst = `C:\Program Files\Minion\minion.exe`
		return dst, activate(dst)
	}
	w.deps.service.ServiceKeyGone = func() bool { return true }
	w.deps.service.SetServiceEnv = func(map[string]string) error { return nil }
	w.deps.service.RemoveServiceKey = func() error { return nil }
	w.deps.service.StopService = func(context.Context) (bool, error) { return false, nil }
	w.deps.serviceStaging = func() (string, func(), error) {
		*w.calls = append(*w.calls, "staging")
		return stagingDir, func() { *w.calls = append(*w.calls, "cleanup") }, nil
	}
	w.deps.serviceRelease = func(_ context.Context, _ *config.Resolver, _ io.Writer, cacheDir string) (version.Fetched, error) {
		*w.cacheDir = cacheDir
		return version.Fetched{Tag: "v1.2.3", Path: releaseInDir, SHA256: verifiedSum}, nil
	}
	return w
}

// The service runs the verified release, fetched and verified in an
// administrator-only staging directory (never the user's update cache), never
// the file the user ran.
func TestWorkerUpWindowsServiceInstallsTheVerifiedRelease(t *testing.T) {
	w := newWindowsUpFixture(t)
	out, err := w.run("--share-existing", "none")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if *w.cacheDir != stagingDir {
		t.Errorf("verified in %q", *w.cacheDir)
	}
	if w.install.Src != releaseInDir || w.install.SHA256 != verifiedSum || w.install.Version != "v1.2.3" || w.install.AllowDowngrade {
		t.Errorf("installed %+v", *w.install)
	}
	got := strings.Join(*w.calls, ",")
	if !strings.HasPrefix(got, "staging,") || !strings.Contains(got, "install,start") || !strings.HasSuffix(got, "cleanup") {
		t.Errorf("calls = %s", got)
	}
	if !strings.Contains(out, "verifying the signed release") || !strings.Contains(out, `C:\Program Files\Minion\minion.exe`) {
		t.Errorf("output:\n%s", out)
	}
}

func TestWorkerUpWindowsServicePassesTheDowngradeFlag(t *testing.T) {
	w := newWindowsUpFixture(t)
	if _, err := w.run("--share-existing", "none", "--"+allowDowngradeFlag); err != nil {
		t.Fatal(err)
	}
	if !w.install.AllowDowngrade {
		t.Errorf("installed %+v", *w.install)
	}
}

// The release is settled before the hooks run (an invitation is used once)
// and before any setting is saved: a release that cannot be verified changes
// nothing.
func TestWorkerUpWindowsServiceStopsWhenTheReleaseCannotBeVerified(t *testing.T) {
	w := newWindowsUpFixture(t)
	hookRan := false
	w.hook("enroll", func(agent.Settings) error { hookRan = true; return nil })
	var deadline bool
	w.deps.serviceRelease = func(ctx context.Context, _ *config.Resolver, _ io.Writer, _ string) (version.Fetched, error) {
		_, deadline = ctx.Deadline()
		return version.Fetched{}, version.ErrChecksum
	}
	_, err := w.run("--share-existing", "none", "--name", "lab")
	if !errors.Is(err, version.ErrChecksum) || !strings.Contains(err.Error(), "verified release") {
		t.Fatalf("err = %v", err)
	}
	if !deadline {
		t.Error("the release fetch has no deadline")
	}
	if got := strings.Join(*w.calls, ","); got != "staging,cleanup" || w.install.Src != "" || hookRan {
		t.Errorf("something ran: calls %s, install %+v, hook %v", got, *w.install, hookRan)
	}
	if _, serr := os.Stat(filepath.Join(w.stateDir(), "config.json")); serr == nil {
		t.Error("settings were saved")
	}
}

// A release mirror URL with a credential in it is not printed.
func TestWorkerUpWindowsServiceRedactsTheReleaseURL(t *testing.T) {
	w := newWindowsUpFixture(t)
	secret := "https://ci:s3cr3t-token@mirror.example/api/s3cr3t-path-token/releases"
	w.deps.serviceRelease = func(context.Context, *config.Resolver, io.Writer, string) (version.Fetched, error) {
		return version.Fetched{}, fmt.Errorf("%w: %w", version.ErrNetwork, &url.Error{Op: "Get", URL: secret, Err: errors.New("connection reset")})
	}
	_, err := w.run("--share-existing", "none")
	if err == nil || strings.Contains(err.Error(), "s3cr3t") || !errors.Is(err, version.ErrNetwork) {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(err.Error(), "mirror.example") {
		t.Errorf("the host is not named: %v", err)
	}
}

// Without the administrator-only staging directory nothing is fetched, and
// the guidance never asks to elevate the user's copy.
func TestWorkerUpWindowsServiceNeedsTheStagingDirectory(t *testing.T) {
	w := newWindowsUpFixture(t)
	w.deps.serviceStaging = func() (string, func(), error) { return "", nil, os.ErrPermission }
	fetched := false
	w.deps.serviceRelease = func(context.Context, *config.Resolver, io.Writer, string) (version.Fetched, error) {
		fetched = true
		return version.Fetched{}, nil
	}
	_, err := w.run("--share-existing", "none")
	if err == nil || fetched || !strings.Contains(err.Error(), "MINION_INSTALL_SERVICE") || strings.Contains(err.Error(), "AppData") {
		t.Fatalf("fetched %v, err = %v", fetched, err)
	}
}

// The bypass exists only in development builds.
func TestWorkerUpWindowsServiceRefusesTheBypassInARelease(t *testing.T) {
	w := newWindowsUpFixture(t)
	_, err := w.run("--share-existing", "none", "--"+allowUnverifiedFlag)
	if err == nil || !strings.Contains(err.Error(), "only for development builds") {
		t.Fatalf("err = %v", err)
	}
	if len(*w.calls) != 0 {
		t.Errorf("calls %v", *w.calls)
	}
}

func TestWorkerUpWindowsServiceUnverifiedNeedsTheFlagAndWarns(t *testing.T) {
	w := newWindowsUpFixture(t)
	version.Version = "dev"
	w.deps.serviceRelease = func(context.Context, *config.Resolver, io.Writer, string) (version.Fetched, error) {
		t.Fatal("fetched although the flag was given")
		return version.Fetched{}, nil
	}
	if _, err := w.run("--share-existing", "none", "--"+allowUnverifiedFlag); err != nil {
		t.Fatal(err)
	}
	if w.install.Src != `C:\Users\ann\AppData\Local\Programs\minion\minion.exe` || w.install.SHA256 != "" {
		t.Errorf("installed %+v", *w.install)
	}
	if !strings.Contains(w.stderr.String(), "without checking that it is a signed release") {
		t.Errorf("stderr = %s", w.stderr.String())
	}
}

// A development build ("dev") has no signed release to install, and nothing
// is created for it.
func TestWorkerUpWindowsServiceRefusesADevelopmentBuild(t *testing.T) {
	w := newWindowsUpFixture(t)
	version.Version = "dev"
	_, err := w.run("--share-existing", "none")
	if err == nil || !strings.Contains(err.Error(), "not a release") || !strings.Contains(err.Error(), "--"+allowUnverifiedFlag) {
		t.Fatalf("err = %v", err)
	}
	if len(*w.calls) != 0 {
		t.Errorf("calls %v", *w.calls)
	}
}
