package client

import (
	"context"
	"net/http"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/ops"
)

// The monitoring header is informational: the call goes through, the notice is
// shown once, and "off" or an unknown value shows nothing.
func TestHubMonitoringNoticeIsInformationalAndOnce(t *testing.T) {
	for _, tc := range []struct {
		header string
		want   int
	}{{"full", 1}, {"actions", 1}, {"off", 0}, {"", 0}, {"FULL", 0}} {
		f := newHubFixture(t, func(_ *hubFixture, w http.ResponseWriter, _ *http.Request) {
			if tc.header != "" {
				w.Header().Set(MonitoringHeader, tc.header)
			}
			jsonReply(w, 200, `{"sessions":[]}`)
		})
		var notices []string
		b := f.hub(t, WithHubNotice(func(s string) { notices = append(notices, s) }))
		for i := 0; i < 3; i++ {
			if _, err := b.Call(context.Background(), ops.OpSessionsList, nil); err != nil {
				t.Fatalf("%q: %v", tc.header, err)
			}
		}
		if len(notices) != tc.want {
			t.Errorf("header %q: %d notices, want %d", tc.header, len(notices), tc.want)
		}
	}
}
