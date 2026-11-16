package version

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// RestrictToHost refuses a redirect to any other host or to plain HTTP.
func TestRestrictToHostRefusesRedirectsElsewhere(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("[]")) }))
	defer target.Close()
	src := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/elsewhere", http.StatusFound)
	}))
	defer src.Close()
	g, err := NewGitHubSource(src.URL, "test")
	if err != nil {
		t.Fatal(err)
	}
	g.RestrictToHost("api.github.com")
	if _, err := g.Releases(context.Background()); err == nil || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("redirect followed: %v", err)
	}
}
