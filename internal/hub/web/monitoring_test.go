package web

import (
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestMonitoringViewTextAndSource(t *testing.T) {
	if currentMonitoring().Version != 0 || currentMonitoring().Notice != "" {
		t.Fatal("no source: nothing monitored")
	}
	release := SetMonitoring(func() MonitoringView { return MonitoringView{Version: 7, Notice: "n"} })
	if v := currentMonitoring(); v.VersionText() != "7" || v.Notice != "n" {
		t.Fatalf("view = %+v", v)
	}
	release()
	if currentMonitoring().Version != 0 {
		t.Fatal("released source still answers")
	}
}

// The version a page was rendered with reaches the API; the browser's other
// headers are not copied.
func TestSeenVersionIsForwardedToTheAPI(t *testing.T) {
	dst, src := http.Header{}, http.Header{}
	src.Set("Minion-Monitoring-Seen", "12")
	src.Set("X-Other", "x")
	copySeen(dst, src)
	if dst.Get("Minion-Monitoring-Seen") != "12" || dst.Get("X-Other") != "" {
		t.Fatalf("dst = %v", dst)
	}
	copySeen(dst, http.Header{})
	if dst.Get("Minion-Monitoring-Seen") != "12" {
		t.Fatal("an absent header must not erase")
	}
}

func TestLayoutShowsTheMonitoringBannerOnlyWhenThereIsAPolicy(t *testing.T) {
	for _, c := range []struct {
		name string
		view MonitoringView
		want string
		not  string
	}{
		{"never on", MonitoringView{}, "", "monitor-banner"},
		{"on", MonitoringView{Version: 3, Notice: "Sessions are monitored: output, input on (policy v3)"}, `data-monitor-version="3"`, "hidden"},
		{"off after being on", MonitoringView{Version: 4}, `data-monitor-version="4" hidden`, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			release := SetMonitoring(func() MonitoringView { return c.view })
			defer release()
			body := renderSignedInPage(t)
			if c.want != "" && !strings.Contains(body, c.want) {
				t.Fatalf("missing %q in\n%s", c.want, body)
			}
			if c.name == "never on" && strings.Contains(body, c.not) {
				t.Fatal("a Hub that never monitored renders a banner")
			}
			if c.name == "on" && !strings.Contains(body, "Sessions are monitored: output, input on (policy v3)") {
				t.Fatal("notice text missing")
			}
		})
	}
}

func TestMonitoringScript(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	script, _ := filepath.Abs(filepath.Join("testdata", "monitoring_test.js"))
	src, _ := filepath.Abs(filepath.Join("static", "monitoring.js"))
	out, err := exec.Command(node, script, src).CombinedOutput()
	if err != nil {
		t.Fatalf("monitoring.js: %v\n%s", err, out)
	}
}

// renderSignedInPage renders a plain signed-in page of a fresh server.
func renderSignedInPage(t *testing.T) string {
	t.Helper()
	reg := NewRegistry()
	if err := reg.RegisterPage(Page{Pattern: "/sessions", Title: "Sessions", Template: "probe.html"}); err != nil {
		t.Fatal(err)
	}
	s := newServer(t, reg)
	req := httptest.NewRequest("GET", "/sessions", nil)
	req.Header.Set("X-Test-Session", "root:admin")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("GET /sessions = %d", rec.Code)
	}
	return rec.Body.String()
}
