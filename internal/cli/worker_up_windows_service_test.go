package cli

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/version"
	"github.com/patrick-1984/ai-minion/internal/worker/service"
)

// winController is a Windows service that installs and runs at once.
type winController struct{ calls *[]string }

func (c winController) Install() error { *c.calls = append(*c.calls, "install"); return nil }
func (c winController) Start() error   { *c.calls = append(*c.calls, "start"); return nil }
func (c winController) Uninstall() error {
	*c.calls = append(*c.calls, "uninstall")
	return errors.New("not installed")
}
func (c winController) Status() (service.Status, error) {
	*c.calls = append(*c.calls, "status")
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
	f.deps.service.NewController = func(string) (service.Controller, error) { return winController{calls: calls}, nil }
	f.deps.service.ServiceBinary = func(src, sum string) (string, error) {
		*copied = [2]string{src, sum}
		return `C:\Program Files\Minion\minion.exe`, nil
	}
	f.deps.service.ServiceKeyGone = func() bool { return true }
	f.deps.service.SetServiceEnv = func(map[string]string) error { return nil }
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

func TestWorkerUpWindowsServiceStopsWhenTheReleaseCannotBeVerified(t *testing.T) {
	f, calls, copied := newWindowsUpFixture(t)
	f.deps.serviceRelease = func(context.Context, *config.Resolver, io.Writer) (version.Fetched, error) {
		return version.Fetched{}, version.ErrChecksum
	}
	_, err := f.run("--share-existing", "none")
	if !errors.Is(err, version.ErrChecksum) || !strings.Contains(err.Error(), "verified release") {
		t.Fatalf("err = %v", err)
	}
	if len(*calls) != 0 || copied[0] != "" {
		t.Errorf("the service was touched: calls %v, copied %v", *calls, *copied)
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
