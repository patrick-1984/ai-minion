package server

import (
	"strings"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/hubid/hubidtest"
	"github.com/patrick-1984/ai-minion/internal/identity"
)

// M11: a Hub without a root logs that it has none yet; the first activation
// logs the "hub identity" line with the words and the 52 characters.
func TestHubIdentityLogLineWithoutRootAndAfterActivation(t *testing.T) {
	ih := startIdentityHub(t, idHubOpts{})
	if logs := ih.h.logs.String(); !strings.Contains(logs, "hub identity: none yet; set the Hub passphrase to create it (minion hub identity set-passphrase)") {
		t.Fatalf("no none-yet line in\n%s", logs)
	}
	res := ih.bootstrap(t, idPass1, 90)
	fp := identity.Fingerprint(res.RootFingerprint)
	logs := ih.h.logs.String()
	if !strings.Contains(logs, `msg="hub identity"`) || !strings.Contains(logs, "fingerprint_words="+fp.Words()) ||
		!strings.Contains(logs, "fingerprint="+res.RootFingerprint) || !strings.Contains(logs, `show="minion hub identity --words"`) {
		t.Errorf("no hub identity line after the activation in\n%s", logs)
	}
}

// M11 at start for a Hub that has a root, and again with the daily tick.
func TestHubIdentityLogLineAtStartAndDaily(t *testing.T) {
	old := identityTickEvery
	identityTickEvery = 50 * time.Millisecond
	t.Cleanup(func() { identityTickEvery = old })
	dir := shortDir(t)
	fp, err := identity.FingerprintOf(hubidtest.Provision(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	ih := startIdentityHub(t, idHubOpts{dir: dir})
	line := "fingerprint_words=" + fp.Words() + " fingerprint=" + fp.String()
	if logs := ih.h.logs.String(); !strings.Contains(logs, line) || strings.Contains(logs, "none yet") {
		t.Fatalf("no hub identity line at start in\n%s", logs)
	}
	deadline := time.Now().Add(10 * time.Second)
	for strings.Count(ih.h.logs.String(), line) < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("the daily tick logged no hub identity line:\n%s", ih.h.logs.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
}
