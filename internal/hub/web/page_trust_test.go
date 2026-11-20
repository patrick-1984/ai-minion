package web

// W3 (v0.11.0 trust ceremony): the Hub fingerprint as six words on the Add
// worker page (M12a), in the "invitation key created" block (M12b), on the
// identity page, and on the pending Worker row with its confirm texts (M12c).

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/ops"
)

func trustFingerprint(t *testing.T) identity.Fingerprint {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	fp, err := identity.FingerprintOf(pub)
	if err != nil {
		t.Fatal(err)
	}
	return fp
}

func TestAddWorkerPageShowsTheHubFingerprintBlock(t *testing.T) {
	fp := trustFingerprint(t)
	connect(t, newFakeOps())
	SetPublicURL(func(*http.Request) string { return "https://hub.example.com" })
	t.Cleanup(func() { SetPublicURL(nil) })
	release := SetHubFingerprint(func() string { return fp.String() })
	t.Cleanup(release)
	body := onbGet(t, onbUser(t), pathAddWorker).Body.String()
	mustHave(t, body, "Give the Worker this Hub's fingerprint", `class="fp-words">`+fp.Words()+`<`,
		"minion worker join https://hub.example.com --hub-fingerprint "+fp.String(), `data-copy="#hub-join"`,
		"The Worker then pairs only with this Hub", "minion hub identity --words", "Worker v0.11.0 or later")
	mustNotHave(t, body, "0.10 or newer")

	// No root: no block.
	release()
	SetHubFingerprint(func() string { return "" })
	t.Cleanup(func() { SetHubFingerprint(nil) })
	body = onbGet(t, onbUser(t), pathAddWorker).Body.String()
	mustNotHave(t, body, "data-hub-fingerprint", "--hub-fingerprint")
}

func TestInvitationCreatedNamesTheHubWords(t *testing.T) {
	fp := trustFingerprint(t)
	key, err := identity.NewInviteKey(fp)
	if err != nil {
		t.Fatal(err)
	}
	create := ops.InvitationCreateResult{ID: onbInviteID, Key: key.String(), Mode: ops.InvitationOneTime, Kind: ops.KindPrivate,
		TTL: ops.TTL1w, CreatedAt: onbTime, ExpiresAt: &onbWeek}
	connect(t, newFakeOps().reply(ops.OpInvitationCreate, create))
	rec := onbPost(t, onbUser(t), pathInvitations, url.Values{"mode": {"one-time"}, "ttl": {"1w"}})
	mustHave(t, rec.Body.String(), "This key is bound to Hub fingerprint", fp.Words(),
		"accepts only that Hub and prints these words", "minion hub identity --words")
}

func TestIdentityPageShowsTheWordsAndTheOperationalKeyNote(t *testing.T) {
	fp := trustFingerprint(t)
	_, body := serveIdentityPage(t, identityStatus(func(s *ops.HubIdentityStatusResult) {
		s.RootFingerprint, s.RootFingerprintWords = fp.String(), fp.Words()
	}), adminHeader)
	mustHave(t, body, "Hub fingerprint (root key)", `class="fp-words">`+fp.Words()+`<`, "<code>"+fp.String()+"</code>",
		"changes at every re-issue; do not compare it", "minion hub identity --words")
}

func TestPendingWorkerRowShowsTheWordsAndTheConfirmTexts(t *testing.T) {
	fp := trustFingerprint(t)
	w := wPending
	w.Fingerprint, w.FingerprintWords = fp.String(), fp.Words()
	v := newDashboardView(Dashboard{Workers: []WorkerRow{wPrivate, w}}, false)
	var buf bytes.Buffer
	if err := fragmentTemplates.ExecuteTemplate(&buf, "workers_results", v); err != nil {
		t.Fatal(err)
	}
	body := buf.String()
	mustHave(t, body, `class="fp-words">`+fp.Words()+`<`, "<code>"+fp.String()+"</code>",
		"Approve worker stolen-box with fingerprint "+fp.Words()+" from 203.0.113.77? The Worker console shows its fingerprint in a box: compare all six words.",
		"Remove worker stolen-box ("+fp.Words()+")?", "compare all six words of the fingerprint",
		`name="fingerprint" value="`+fp.String()+`"`)
	if strings.Count(body, fp.Words()) < 3 {
		t.Errorf("the words appear %d times", strings.Count(body, fp.Words()))
	}
}

// Every signed-in page shows the Hub's words in the top bar, just before the
// theme switch (A52), so a Worker pinned to another key stands out.
func TestTopBarShowsTheHubWords(t *testing.T) {
	fp := trustFingerprint(t)
	connect(t, newFakeOps())
	release := SetHubFingerprint(func() string { return fp.String() })
	t.Cleanup(release)
	body := onbGet(t, onbUser(t), pathAddWorker).Body.String()
	mustHave(t, body, `data-hub-identity`, `<span class="fp-words">`+fp.Words()+`</span></button>`, `popovertarget="hub-id-pop"`, fp.String())
	if i, j := strings.Index(body, "data-hub-identity"), strings.Index(body, `data-theme-choice="system"`); i < 0 || j < 0 || i > j {
		t.Fatalf("the Hub words are not before the theme switch (%d, %d)", i, j)
	}
}

// A55: the Hub key words are compared by eye, so no rule may cut, shorten or
// hide them (no ellipsis, no width cap, no display:none) at any width.
func TestHubKeyWordsAreNeverCutOrHidden(t *testing.T) {
	css, err := staticFiles.ReadFile("static/minion.css")
	if err != nil {
		t.Fatal(err)
	}
	for _, rule := range strings.Split(string(css), "}") {
		i := strings.Index(rule, "{")
		if i < 0 || !strings.Contains(rule[:i], "hub-id") {
			continue
		}
		body := rule[i:]
		for _, bad := range []string{"text-overflow", "max-width", "display: none", "visibility: hidden", "overflow: hidden", "-webkit-line-clamp"} {
			if strings.Contains(body, bad) {
				t.Errorf("rule %q has %q: the Hub key words must stay complete", strings.TrimSpace(rule[:i]), bad)
			}
		}
	}
}
