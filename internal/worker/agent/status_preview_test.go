package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// AC (Story 8.2): `minion worker status` carries the preview label on Windows.
func TestStatusLabelsWindowsAsPreview(t *testing.T) {
	r := BuildStatus(context.Background(), StatusInputs{Version: "v0.1.0", Name: "pc", Platform: "windows"})
	if !r.Preview || r.Platform != "windows" {
		t.Fatalf("report = %+v", r)
	}
	var b bytes.Buffer
	if err := r.WriteText(&b); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(b.String(), "\n")
	if !strings.HasPrefix(lines[0], "Version:") || !strings.Contains(lines[1], "Platform:") || !strings.Contains(lines[1], "Windows (preview)") {
		t.Errorf("the platform line is not right after the version:\n%s", b.String())
	}
	var js bytes.Buffer
	if err := r.WriteJSON(&js); err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(js.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	if m["platform"] != "windows" || m["preview"] != true {
		t.Errorf("JSON = %s", js.String())
	}
}

// Platforms that are not a preview add nothing to the report, so that the
// output of Linux and macOS is unchanged.
func TestStatusSaysNothingAboutThePlatformWhereItIsNotAPreview(t *testing.T) {
	for _, goos := range []string{"", "linux", "darwin"} {
		r := BuildStatus(context.Background(), StatusInputs{Platform: goos})
		var b, js bytes.Buffer
		_ = r.WriteText(&b)
		_ = r.WriteJSON(&js)
		if r.Preview || r.Platform != "" || strings.Contains(b.String(), "Platform") || strings.Contains(js.String(), "platform") || strings.Contains(js.String(), "preview\"") {
			t.Errorf("%q: report mentions the platform:\n%s\n%s", goos, b.String(), js.String())
		}
	}
}
