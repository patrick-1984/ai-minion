package cli

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/identity"
)

// pinOfferRun runs sessions_list against a Hub whose certificate this client
// does not trust, with stdin typed by the user, and returns the exit code,
// the output and the saved config file (empty when none was written).
func pinOfferRun(t *testing.T, interactive bool, typed string) (hub *hubStub, code int, out, errs, saved string) {
	t.Helper()
	hub = newHubStub(t, func(w http.ResponseWriter, r *http.Request, s *hubStub) { stubJSON(w, 200, `{"sessions":[]}`) })
	stateDir := t.TempDir()
	env := map[string]string{"MINION_HUB_URL": hub.srv.URL, "MINION_API_KEY_FILE": hub.keyFile, "MINION_STATE_DIR": stateDir}
	deps := depsWithSettings(localAnswers(`{}`), env)
	deps.pinOffer = &hubPinOffer{
		interactive: func(io.Reader, io.Writer) bool { return interactive },
		save: func(pin string) (string, error) {
			return saveClientSetting(envFrom(env), config.KeyHubTLSPin, pin)
		},
	}
	code, out, errs = runWith(t, deps, typed, "sessions_list", "--hub-worker", hubWorkerID)
	data, err := os.ReadFile(filepath.Join(stateDir, workerConfigName))
	if err == nil {
		saved = string(data)
	}
	return hub, code, out, errs, saved
}

func TestHubPinOfferSavesOnlyTheExactPinAndTriesOnceMore(t *testing.T) {
	// The pin is that of the shared httptest certificate.
	probe := newHubStub(t, func(http.ResponseWriter, *http.Request, *hubStub) {})
	pin := identity.SPKIPin(probe.srv.Certificate().RawSubjectPublicKeyInfo)

	hub, code, out, errs, saved := pinOfferRun(t, true, pin+"\n")
	if code != 0 || hub.hits.Load() != 1 {
		t.Fatalf("exact pin: exit %d, hits %d, stdout %q, stderr %q", code, hub.hits.Load(), out, errs)
	}
	if !strings.Contains(out, "not trusted by this computer (signed by an unknown authority)") || !strings.Contains(out, "Its key pin is "+pin) ||
		!strings.Contains(out, "minion hub identity --words") || !strings.Contains(out, "Saved hub_tls_pin") {
		t.Errorf("prompt = %q", out)
	}
	if !strings.Contains(saved, `"hub_tls_pin": "`+pin+`"`) {
		t.Errorf("config file = %q", saved)
	}

	for name, typed := range map[string]string{
		"y":              "y\n",
		"yes":            "yes\n",
		"empty":          "\n",
		"end of input":   "",
		"other pin":      "sha256//47DEQpj8HBSa+/TImW+5JCeuQeRkm5NMpJWZG3hSuFU=\n",
		"without prefix": strings.TrimPrefix(pin, "sha256//") + "\n",
	} {
		hub, code, out, errs, saved := pinOfferRun(t, true, typed)
		if code != ExitError || hub.hits.Load() != 0 || saved != "" {
			t.Errorf("%s: exit %d, hits %d, saved %q, stdout %q", name, code, hub.hits.Load(), saved, out)
		}
		if !strings.Contains(out, "nothing was saved") || !strings.Contains(errs, "Its key pin is "+pin) {
			t.Errorf("%s: stdout %q, stderr %q", name, out, errs)
		}
	}
}

func TestHubPinOfferIsNotMadeWithoutATerminal(t *testing.T) {
	hub, code, out, errs, saved := pinOfferRun(t, false, "anything\n")
	if code != ExitError || hub.hits.Load() != 0 || saved != "" || strings.Contains(out, "Type the pin") {
		t.Fatalf("exit %d, hits %d, saved %q, stdout %q", code, hub.hits.Load(), saved, out)
	}
	if !strings.Contains(errs, "unknown authority") || !strings.Contains(errs, "--hub-tls-pin") || !strings.Contains(errs, "Its key pin is sha256//") {
		t.Errorf("stderr = %q", errs)
	}
}

func TestHubTLSPinFlagAndEnvironmentReachTheClient(t *testing.T) {
	hub := newHubStub(t, func(w http.ResponseWriter, r *http.Request, s *hubStub) { stubJSON(w, 200, `{"sessions":[]}`) })
	pin := identity.SPKIPin(hub.srv.Certificate().RawSubjectPublicKeyInfo)
	env := map[string]string{"MINION_HUB_URL": hub.srv.URL, "MINION_API_KEY_FILE": hub.keyFile}
	if code, _, errs := runWith(t, depsWithSettings(localAnswers(`{}`), env), "", "sessions_list", "--hub-worker", hubWorkerID, "--hub-tls-pin", pin); code != 0 {
		t.Errorf("flag: exit %d, stderr %q", code, errs)
	}
	env["MINION_HUB_TLS_PIN"] = pin
	if code, _, errs := runWith(t, depsWithSettings(localAnswers(`{}`), env), "", "sessions_list", "--hub-worker", hubWorkerID); code != 0 {
		t.Errorf("environment: exit %d, stderr %q", code, errs)
	}
	env["MINION_HUB_TLS_PIN"] = "sha256//bad"
	if code, _, errs := runWith(t, depsWithSettings(localAnswers(`{}`), env), "", "sessions_list", "--hub-worker", hubWorkerID); code != ExitError || !strings.Contains(errs, "TLS key pin") {
		t.Errorf("malformed: exit %d, stderr %q", code, errs)
	}
}

func TestMCPHubClientUsesTheTLSPin(t *testing.T) {
	hub := newHubStub(t, func(w http.ResponseWriter, r *http.Request, s *hubStub) {
		stubJSON(w, 200, `{"sessions":[{"name":"remote","created":"2026-11-09T10:00:00.000Z","windows":1,"attached":false}]}`)
	})
	pin := identity.SPKIPin(hub.srv.Certificate().RawSubjectPublicKeyInfo)
	call := `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"sessions_list","arguments":{}}}`
	env := map[string]string{"MINION_HUB_URL": hub.srv.URL, "MINION_API_KEY_FILE": hub.keyFile, "MINION_HUB_TLS_PIN": pin}
	deps, _ := hubMCPDeps(env, &stubBackend{})
	got, _ := rpc(t, deps, []string{"--hub-worker", hubWorkerID}, listRequest, call)
	if !strings.Contains(string(got[3]), "remote") || strings.Contains(string(got[3]), `"isError":true`) {
		t.Fatalf("pinned: tools/call = %s", got[3])
	}
	// Without the pin the tool reports the reason and the presented pin.
	delete(env, "MINION_HUB_TLS_PIN")
	deps, _ = hubMCPDeps(env, &stubBackend{})
	got, _ = rpc(t, deps, []string{"--hub-worker", hubWorkerID}, listRequest, call)
	if s := string(got[3]); !strings.Contains(s, "unknown authority") || !strings.Contains(s, pin) {
		t.Errorf("unpinned: tools/call = %s", s)
	}
}
