package ops

import (
	"os"
	"strings"
	"testing"
)

// The exporter disclosure must read the same in the Go constant, both installers
// and the Android string.
func TestUsageExporterNoticeMirrors(t *testing.T) {
	if !strings.HasSuffix(UsageDisclosure, " "+UsageExporterNotice) || !strings.Contains(UsageExporterNotice, "usage_claude_exporter=off") {
		t.Fatalf("UsageDisclosure must end with the exporter notice: %q", UsageDisclosure)
	}
	for _, f := range []string{"../../deploy/install.sh", "../../deploy/install.ps1", "../../android/app/src/main/res/values/styles.xml"} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(b), UsageExporterNotice) {
			t.Errorf("%s does not carry the exporter notice verbatim", f)
		}
	}
}
