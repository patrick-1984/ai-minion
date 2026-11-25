package api

import (
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// CatalogPath is the route of the generated API description: the bare
// /api/v1/ops, next to the operations at /api/v1/ops/<name>.
const CatalogPath = ops.OpenAPIPath

// CatalogHandler serves GET /api/v1/ops: the OpenAPI 3.1 description that
// [ops.Registry.OpenAPI] generates from the catalog. Any authenticated
// principal may read it: it says which operations exist and what they take,
// which every signed-in page and every API key already learns from the
// product, and nothing about users, Workers or sessions.
type CatalogHandler struct {
	// Auth authenticates the caller. Required.
	Auth Authn
	// Catalog is the catalog that is described. Required.
	Catalog *ops.Registry
	// BodyTimeout bounds the time a client may take to send a request body
	// that it announced; zero means [DefaultBodyTimeout].
	BodyTimeout time.Duration

	once sync.Once
	doc  []byte
	err  error
}

// NewCatalogHandler checks the configuration and returns the handler.
func NewCatalogHandler(auth Authn, catalog *ops.Registry) (*CatalogHandler, error) {
	if auth == nil || catalog == nil {
		return nil, fmt.Errorf("api: the catalog handler needs an authenticator and a catalog")
	}
	return &CatalogHandler{Auth: auth, Catalog: catalog}, nil
}

// ServeHTTP implements http.Handler. The document is generated on the first
// request and kept: the catalog does not change once the Hub serves.
func (h *CatalogHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if err := absorbBody(w, r, h.BodyTimeout); err != nil {
		WriteError(w, err)
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		WriteError(w, NewError(proto.CodeMethodNotAllowed, "use GET"))
		return
	}
	if _, err := h.Auth.Authenticate(r); err != nil {
		WriteError(w, err)
		return
	}
	h.once.Do(func() { h.doc, h.err = h.Catalog.OpenAPI() })
	if h.err != nil {
		WriteError(w, internalError())
		return
	}
	hd := w.Header()
	hd.Set("Content-Type", "application/json")
	hd.Set("Cache-Control", "no-store")
	hd.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(h.doc)
}
