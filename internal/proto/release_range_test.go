package proto

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// The signed release manifest must declare exactly the protocol this build
// speaks: 3.0..3.0 (design hub-identity-v2, section 11.1).
func TestReleaseWorkflowDeclaresTheCurrentProtocol(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", "release.yml"))
	if err != nil {
		t.Skipf("release workflow not available: %v", err)
	}
	for _, name := range []string{"MINION_PROTOCOL_MIN", "MINION_PROTOCOL_MAX"} {
		m := regexp.MustCompile(name + `: "([0-9]+\.[0-9]+)"`).FindSubmatch(raw)
		if m == nil {
			t.Fatalf("release.yml has no %s", name)
		}
		if string(m[1]) != Current.String() {
			t.Errorf("%s = %s, want %s", name, m[1], Current)
		}
	}
}
