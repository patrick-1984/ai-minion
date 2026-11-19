package workers

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/patrick-1984/ai-minion/internal/transport"
)

// The hello facts are display only: anything malformed is dropped, the
// reason is bounded and made safe; nothing is rejected.
func TestCleanFactsKeepsOnlyWellFormedFacts(t *testing.T) {
	ok := transport.WorkerFacts{Build: "v0.9.0", OS: "linux", Arch: "arm64", UpdateBlock: "installed by a package"}
	if got := CleanFacts(ok); got != ok {
		t.Fatalf("%+v", got)
	}
	for _, b := range []string{"v99999.0.0", "0.9.0", "v0.9", "dev2", "<b>", strings.Repeat("v", 100), "v0.9.0\n"} {
		if got := CleanFacts(transport.WorkerFacts{Build: b}); got.Build != "" {
			t.Errorf("build %q kept", b)
		}
	}
	if got := CleanFacts(transport.WorkerFacts{Build: "v1.2.3-rc.4"}); got.Build != "v1.2.3-rc.4" {
		t.Errorf("rc dropped: %q", got.Build)
	}
	if got := CleanFacts(transport.WorkerFacts{Build: "dev"}); got.Build != "dev" {
		t.Errorf("dev dropped")
	}
	for _, p := range []string{"Linux", "linux x", "", strings.Repeat("a", 17)} {
		if got := CleanFacts(transport.WorkerFacts{OS: p, Arch: p}); got.OS != "" || got.Arch != "" {
			t.Errorf("platform %q kept", p)
		}
	}
	got := CleanFacts(transport.WorkerFacts{UpdateBlock: strings.Repeat("x", 500) + "\x1b[2J"})
	if utf8.RuneCountInString(got.UpdateBlock) > maxUpdateBlock+1 || // +1: the ellipsis of a cut
		strings.ContainsRune(got.UpdateBlock, 0x1b) {
		t.Errorf("update block not bounded/safe: %q", got.UpdateBlock)
	}
}

// The Worker's hub_check fact (v0.11.0) is kept only as one of the known values.
func TestCleanFactsKeepsAKnownHubCheckOnly(t *testing.T) {
	for in, want := range map[string]string{"none": "none", "fingerprint": "fingerprint", "invite": "invite", "mismatch": "mismatch", "bogus\x1b": "", "": ""} {
		if got := CleanFacts(transport.WorkerFacts{HubCheck: in}).HubCheck; got != want {
			t.Errorf("%q -> %q, want %q", in, got, want)
		}
	}
}

// The Worker's traffic_payload fact is a plain bool that survives cleaning, so
// the Hub can show the notice on that Worker's sessions.
func TestCleanFactsKeepsTheTrafficPayloadFact(t *testing.T) {
	if !CleanFacts(transport.WorkerFacts{TrafficPayload: true}).TrafficPayload {
		t.Fatal("dropped")
	}
	if CleanFacts(transport.WorkerFacts{}).TrafficPayload {
		t.Fatal("invented")
	}
}
