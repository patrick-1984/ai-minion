package web

import (
	"testing"

	"github.com/patrick-1984/ai-minion/internal/version"
)

// Everyone sees "Minion vX"; only an administrator sees "vX → vY", and only
// when the check found a newer release. Pages never call the source more
// than once per render and never reach the network themselves.
func TestRailVersionBlock(t *testing.T) {
	old := version.Version
	version.Version = "v0.9.0"
	t.Cleanup(func() { version.Version = old })
	newer := true
	SetUpdateSource(func() (string, string, bool) { return "v0.9.3", "https://example.invalid/r/v0.9.3", newer })
	t.Cleanup(func() { SetUpdateSource(nil) })
	connect(t, newFakeOps())
	s := adminServer(t)

	body := get(t, s, "/account", "X-Test-Session", adminHeader).Body.String()
	mustHave(t, body, `<span class="muted">Minion v0.9.0</span>`, `v0.9.0 → v0.9.3</a>`, `href="https://example.invalid/r/v0.9.3"`)
	body = get(t, s, "/account", "X-Test-Session", userHeader).Body.String()
	mustHave(t, body, `Minion v0.9.0`)
	mustNotHave(t, body, `→ v0.9.3`)
	newer = false
	mustNotHave(t, get(t, s, "/account", "X-Test-Session", adminHeader).Body.String(), `→ v0.9.3`)
}
