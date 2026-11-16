package version

import (
	"strings"
	"testing"
)

func TestManifestImageIsValidated(t *testing.T) {
	base := `{"schema":1,"version":"v0.9.0","protocol_range":{"min":"3.0","max":"3.0"},"artifacts":[]%s}`
	good := `,"image":{"repository":"ghcr.io/patrick-1984/minion","digest":"sha256:` + strings.Repeat("a", 64) + `"}`
	m, err := ParseManifest([]byte(strings.Replace(base, "%s", good, 1)), "v0.9.0")
	if err != nil || m.Image == nil || m.Image.Ref() != "ghcr.io/patrick-1984/minion@sha256:"+strings.Repeat("a", 64) {
		t.Fatalf("%+v %v", m.Image, err)
	}
	if m, err := ParseManifest([]byte(strings.Replace(base, "%s", "", 1)), "v0.9.0"); err != nil || m.Image != nil {
		t.Fatalf("a manifest without image: %+v %v", m.Image, err)
	}
	for _, bad := range []string{
		`,"image":{"repository":"ghcr.io/patrick-1984/minion","digest":"sha256:short"}`,
		`,"image":{"repository":"ghcr.io/patrick-1984/minion","digest":"latest"}`,
		`,"image":{"repository":"Ghcr.io/X","digest":"sha256:` + strings.Repeat("a", 64) + `"}`,
		`,"image":{"repository":"","digest":"sha256:` + strings.Repeat("a", 64) + `"}`,
	} {
		if _, err := ParseManifest([]byte(strings.Replace(base, "%s", bad, 1)), "v0.9.0"); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
}
