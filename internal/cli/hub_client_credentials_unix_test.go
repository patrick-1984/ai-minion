//go:build unix

package cli

import (
	"net/http"
	"os"
	"strings"
	"testing"
)

// A key file that other users can read or write is refused by both entry
// points, before anything is sent, without showing the key or its content; the
// same key in a private file works.
func TestExposedAPIKeyFileIsRefusedByCLIAndMCP(t *testing.T) {
	hub := newHubStub(t, func(w http.ResponseWriter, r *http.Request, s *hubStub) { stubJSON(w, 200, `{"sessions":[]}`) })
	for _, mode := range []os.FileMode{0o644, 0o666, 0o640} {
		if err := os.Chmod(hub.keyFile, mode); err != nil {
			t.Fatal(err)
		}
		code, out, errs := runWith(t, depsWithSettings(localAnswers(`{}`), hub.env()), "", "sessions_list")
		if code != 1 || !strings.Contains(errs, "API key file cannot be used") || strings.Contains(out+errs, hub.key) {
			t.Errorf("mode %04o, command: exit %d, stdout %q, stderr %q", mode, code, out, errs)
		}
		deps, stderr := hubMCPDeps(hub.env(), &stubBackend{})
		err := runMCPUntilItEnds(t, deps)
		if err == nil || !strings.Contains(err.Error(), "API key file cannot be used") || strings.Contains(err.Error()+stderr.String(), hub.key) {
			t.Errorf("mode %04o, mcp: %v, log %q", mode, err, stderr.String())
		}
	}
	if hub.hits.Load() != 0 {
		t.Error("a Hub was contacted with an exposed key file")
	}
	if err := os.Chmod(hub.keyFile, 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _, errs := runWith(t, depsWithSettings(localAnswers(`{}`), hub.env()), "", "sessions_list"); code != 0 {
		t.Errorf("a private key file: exit %d, stderr %q", code, errs)
	}
}
