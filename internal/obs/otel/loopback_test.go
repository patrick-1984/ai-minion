package otel

import (
	"net/http"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/config"
)

func TestLoopbackHostAnyCaseAndNoProxy(t *testing.T) {
	for h, want := range map[string]bool{"localhost": true, "LOCALHOST": true, "LocalHost": true, "127.0.0.1": true, "::1": true, "collector.example": false, "10.0.0.1": false} {
		if config.LoopbackHost(h) != want {
			t.Errorf("LoopbackHost(%q) = %v", h, !want)
		}
	}
	c, _ := exportClient(nil)
	if c.Transport.(responseFilter).rt.(*http.Transport).Proxy == nil {
		t.Fatal("the client has no proxy to remove; the test proves nothing")
	}
	noProxy(c)
	if c.Transport.(responseFilter).rt.(*http.Transport).Proxy != nil {
		t.Fatal("a loopback collector is reached through the environment's proxy")
	}
}
