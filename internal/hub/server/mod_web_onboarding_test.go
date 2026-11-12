package server

import (
	"github.com/patrick-1984/ai-minion/internal/testutil/hubstore"
	"html"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"
)

// The onboarding pages (Story 6.7) on a real Hub: the Hub's own initialisation
// wires them, a signed-in user works through the pages, and what the pages show
// comes from the real services and store.

// The Worker commands name the configured MINION_PUBLIC_URL, the address a
// remote Worker can reach, and not the Host the browser used (the Hub is
// reached here through its loopback address). Without a configured URL they
// name the address of the request.
func TestInvitationCommandsUseTheConfiguredPublicURL(t *testing.T) {
	t.Setenv("MINION_ADMIN_USERNAME", "root")
	t.Setenv("MINION_ADMIN_PASSWORD", "onboarding-integration-1")
	create := func(t *testing.T, cfg *Config) (body, requestBase string) {
		t.Helper()
		h := startHub(t, cfg, DefaultRegistry)
		hc := newHubClient(t, h, "https")
		code, _, csrf := hc.login("root", "onboarding-integration-1")
		if code != 200 {
			t.Fatalf("login = %d", code)
		}
		code, body, _ = formDo(hc, "POST", "/invitations", csrf, url.Values{"mode": {"one_time"}, "ttl": {"24h"}, "name": {"laptop"}})
		if code != 200 || !strings.Contains(body, "minion worker up --server ") {
			t.Fatalf("create invitation = %d\n%s", code, body)
		}
		return html.UnescapeString(body), hc.base
	}

	t.Run("configured", func(t *testing.T) {
		body, base := create(t, &Config{DataDir: hubstore.Dir(t), PublicURL: "https://hub.public.example"})
		if !strings.Contains(body, "--server https://hub.public.example ") || !strings.Contains(body, `MINION_SERVER: "https://hub.public.example"`) {
			t.Errorf("the commands do not use the configured public URL:\n%s", body)
		}
		if strings.Contains(body, base) {
			t.Errorf("the commands name the address of the request, %s:\n%s", base, body)
		}
	})
	t.Run("not configured", func(t *testing.T) {
		body, base := create(t, &Config{DataDir: hubstore.Dir(t)})
		if !strings.Contains(body, "--server "+base+" ") {
			t.Errorf("without a public URL the commands should name %s:\n%s", base, body)
		}
	})
}

// The API key page, end to end: create a key in the page, use it as a bearer
// token on the REST API, and the list shows when it was last used. Before the
// first use the list says so instead of showing a time.
func TestAPIKeyPageShowsTheRealLastUse(t *testing.T) {
	t.Setenv("MINION_ADMIN_USERNAME", "root")
	t.Setenv("MINION_ADMIN_PASSWORD", "onboarding-integration-2")
	t.Setenv("MINION_PUBLIC_URL", "")
	h := startHub(t, &Config{DataDir: hubstore.Dir(t)}, DefaultRegistry)
	hc := newHubClient(t, h, "https")
	code, _, csrf := hc.login("root", "onboarding-integration-2")
	if code != 200 {
		t.Fatalf("login = %d", code)
	}
	code, body, _ := formDo(hc, "POST", "/api-keys", csrf, url.Values{
		"name": {"ci"}, "resource": {"everything"}, "action": {"read"}, "expires": {"90d"},
	})
	if code != 200 {
		t.Fatalf("create key = %d\n%s", code, body)
	}
	m := regexp.MustCompile(`value="(mn_[A-Za-z2-7]{52})"`).FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("the answer does not show the key:\n%s", body)
	}
	key := m[1]
	_, page, _ := hc.do("GET", "/api-keys", nil, nil)
	if strings.Contains(page, key) {
		t.Fatal("the list shows the key")
	}
	if !strings.Contains(page, "never used") {
		t.Fatalf("before the first use the list should say so:\n%s", page)
	}

	before := time.Now().UTC().Truncate(time.Minute)
	if code, body, _ := hc.do("POST", "/api/v1/ops/workers_list", map[string]any{}, map[string]string{"Authorization": "Bearer " + key}); code != 200 {
		t.Fatalf("bearer call = %d %s", code, body)
	}
	after := time.Now().UTC()
	_, page, _ = hc.do("GET", "/api-keys", nil, nil)
	if strings.Contains(page, "never used") {
		t.Fatalf("after a call with the key the list still says never used:\n%s", page)
	}
	var found bool
	for at := before; !at.After(after); at = at.Add(time.Minute) {
		if strings.Contains(page, at.Format("2006-01-02 15:04")+" UTC") {
			found = true
		}
	}
	if !found {
		t.Errorf("the list does not show a last use between %s and %s:\n%s", before, after, page)
	}
}
