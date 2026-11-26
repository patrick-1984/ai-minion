package server

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// The keep form on a real Hub: page route -> in-process REST -> authz ->
// router -> Worker. A post without the CSRF header is refused before anything
// runs; the owner's post reaches the Worker as session_keep_set; a user
// without access sees the refusal and the Worker sees nothing.
func TestSessionKeepFormOnARealHub(t *testing.T) {
	sp := startSessionPageHubWith(t, append(append([]string(nil), modernWorkerFeatures...), proto.FeatureSessionKeepV1))
	const view = "/s/lab/minion-1"

	_, body, _ := sp.browser("GET", view, sp.adminTok, "", nil)
	if !strings.Contains(body, `hx-post="/s/lab/minion-1/keep"`) {
		t.Fatalf("the session page offers no keep checkbox:\n%s", body)
	}

	before := sp.wk.total()
	if code, _, _ := sp.browser("POST", view+"/keep", sp.adminTok, "", url.Values{"keep": {"0", "1"}}); code != http.StatusForbidden {
		t.Errorf("POST without CSRF = %d, want 403", code)
	}
	if sp.wk.total() != before {
		t.Fatal("a post without a CSRF token reached the Worker")
	}

	code, body, _ := sp.browser("POST", view+"/keep", sp.adminTok, sp.adminCSRF, url.Values{"keep": {"0", "1"}})
	if code != 200 || !strings.Contains(body, "Kept.") {
		t.Fatalf("keep = %d\n%s", code, body)
	}
	calls := sp.wk.calls(ops.OpSessionKeepSet)
	if len(calls) != 1 || calls[0]["keep"] != true {
		t.Fatalf("session_keep_set calls = %+v", calls)
	}
	if tgt, _ := calls[0]["target"].(map[string]any); tgt["session_name"] != "minion-1" {
		t.Errorf("target = %+v", calls[0]["target"])
	}

	before = sp.wk.total()
	code, body, _ = sp.browser("POST", view+"/keep", sp.bobTok, sp.bobCSRF, url.Values{"keep": {"0"}})
	if code != 200 || !strings.Contains(body, "result-danger") {
		t.Errorf("keep as bob = %d\n%s", code, body)
	}
	if sp.wk.total() != before {
		t.Error("bob's keep reached the Worker")
	}
}
