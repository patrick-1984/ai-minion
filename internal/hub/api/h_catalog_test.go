package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/proto"
)

// catalogRig mounts the catalog route on the mux of the dispatcher rig, next
// to the operation routes and the not_found fallback, as the Hub does.
func catalogRig(t *testing.T) *rig {
	t.Helper()
	r := newRig(t)
	h, err := NewCatalogHandler(r.authn, r.d.Catalog)
	if err != nil {
		t.Fatal(err)
	}
	r.mux.Handle(CatalogPath, h)
	return r
}

func TestCatalogServesTheGeneratedDocument(t *testing.T) {
	r := catalogRig(t)
	w := r.do(http.MethodGet, "/api/v1/ops", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	if got := w.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q", got)
	}
	if got := w.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q", got)
	}
	if got := w.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q", got)
	}
	want, err := r.d.Catalog.OpenAPI()
	if err != nil {
		t.Fatal(err)
	}
	if w.Body.String() != string(want) {
		t.Error("the body is not the document of ops.Registry.OpenAPI")
	}
	var doc struct {
		OpenAPI string                    `json:"openapi"`
		Paths   map[string]map[string]any `json:"paths"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.OpenAPI != "3.1.0" {
		t.Errorf("openapi = %q", doc.OpenAPI)
	}
	for _, op := range r.d.Catalog.All() {
		item, ok := doc.Paths[op.RESTPath()]
		if !ok {
			t.Errorf("op %s is missing", op.Name)
			continue
		}
		if _, hasGet := item["get"]; hasGet == op.ChangesState() {
			t.Errorf("op %s: GET present = %v", op.Name, hasGet)
		}
	}
	// The second request is answered from the kept document.
	if w2 := r.do(http.MethodGet, "/api/v1/ops", ""); w2.Body.String() != string(want) {
		t.Error("the second answer differs")
	}
	if r.decide.count() != 0 {
		t.Errorf("authz was asked %d times; the route needs authentication only", r.decide.count())
	}
}

func TestCatalogNeedsAuthentication(t *testing.T) {
	r := catalogRig(t)
	r.authn.setErr(NewError(proto.CodeUnauthenticated, "sign in"))
	w := r.do(http.MethodGet, "/api/v1/ops", "")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	if !strings.Contains(w.Body.String(), `"code":"unauthenticated"`) || strings.Contains(w.Body.String(), "openapi") {
		t.Errorf("body = %s", w.Body)
	}
	if got := w.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q", got)
	}
}

func TestCatalogIsGetOnlyAndKeepsTheOtherRoutes(t *testing.T) {
	r := catalogRig(t)
	w := r.do(http.MethodPost, "/api/v1/ops", "{}")
	if w.Code != http.StatusMethodNotAllowed || w.Header().Get("Allow") != http.MethodGet {
		t.Errorf("POST: status %d, Allow %q", w.Code, w.Header().Get("Allow"))
	}
	if r.authn.calls() != 0 {
		t.Errorf("a refused method was authenticated %d times", r.authn.calls())
	}
	// An unknown operation still answers through the dispatcher, and an
	// unknown path through the fallback.
	if w := r.do(http.MethodGet, "/api/v1/ops/no_such_op", ""); !strings.Contains(w.Body.String(), `"error"`) || w.Code == http.StatusOK {
		t.Errorf("unknown op: %d %s", w.Code, w.Body)
	}
	if w := r.do(http.MethodGet, "/api/v1/opsx", ""); w.Code != http.StatusNotFound {
		t.Errorf("unknown path: %d", w.Code)
	}
}

func TestNewCatalogHandlerChecksItsConfiguration(t *testing.T) {
	r := newRig(t)
	if _, err := NewCatalogHandler(nil, r.d.Catalog); err == nil {
		t.Error("no authenticator: want an error")
	}
	if _, err := NewCatalogHandler(r.authn, nil); err == nil {
		t.Error("no catalog: want an error")
	}
}
