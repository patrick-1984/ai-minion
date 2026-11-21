package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestMaintenanceGateRejectsWrites(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	t.Setenv("MINION_MAINTENANCE", "readonly")
	h := maintenanceGate(ok)
	for _, tc := range []struct {
		method, upgrade string
		want            int
	}{
		{"GET", "", 200}, {"HEAD", "", 200}, {"OPTIONS", "", 200},
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
