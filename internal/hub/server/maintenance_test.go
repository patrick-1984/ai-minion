package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestMaintenanceBootSkipsApplicationModulesAndManagement(t *testing.T) {
	t.Setenv("MINION_MAINTENANCE", "readonly")
	reg := NewRegistry()
	if err := reg.Register("must-not-start", 50, func(context.Context, *Env) error {
		t.Error("application/background module ran during trial")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	r := startHubPristine(t, &Config{DataDir: t.TempDir(), TLSOff: true, PublicURL: "http://localhost", ManagementListen: "127.0.0.1:0"}, reg)
	if r.srv.ManagementAddr() != "" {
		t.Fatal("management listener enabled in maintenance")
	}
	for _, path := range []string{"/readyz", "/healthz", "/auth/oidc/callback", "/api/worker", "/"} {
		res, err := http.Get("http://" + r.srv.Addr() + path)
		if err != nil {
			t.Fatal(err)
		}
		_ = res.Body.Close()
		want := http.StatusServiceUnavailable
		if path == "/readyz" || path == "/healthz" {
			want = http.StatusOK
		}
		if res.StatusCode != want {
			t.Errorf("%s: %d, want %d", path, res.StatusCode, want)
		}
	}
}

func TestMaintenanceGateRejectsWrites(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	t.Setenv("MINION_MAINTENANCE", "readonly")
	h := maintenanceGate(ok)
	for _, tc := range []struct {
		method, upgrade string
		want            int
	}{
		{"GET", "", 503}, {"HEAD", "", 503}, {"OPTIONS", "", 503},
		{"POST", "", 503}, {"PUT", "", 503}, {"DELETE", "", 503}, {"PATCH", "", 503}, {"GET", "websocket", 503},
	} {
		req := httptest.NewRequest(tc.method, "/x", nil)
		if tc.upgrade != "" {
			req.Header.Set("Upgrade", tc.upgrade)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != tc.want {
			t.Errorf("%s upgrade=%q: %d, want %d", tc.method, tc.upgrade, rec.Code, tc.want)
		}
	}
	t.Setenv("MINION_MAINTENANCE", "")
	rec := httptest.NewRecorder()
	maintenanceGate(ok).ServeHTTP(rec, httptest.NewRequest("POST", "/x", nil))
	if rec.Code != 200 {
		t.Fatalf("gate active without the variable: %d", rec.Code)
	}
}
