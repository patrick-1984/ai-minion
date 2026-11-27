package files

import (
	"context"
	"net/http"
	"strings"

	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/transport"
)

// Authenticate authenticates a request to the Hub's HTTP endpoint: the
// browser session cookie (with its CSRF header for every method but GET) or
// an API key. api.Authenticator.Authenticate implements it through a small
// adapter.
type Authenticate func(r *http.Request) (authz.Principal, error)

// Handler returns the Hub's public data-leg endpoint for the web UI and
// REST callers: GET (download, Range relayed to the Worker) and PUT (upload
// of one file) of proto.FilesLegPath(<transfer id>), after an admission
// through the ordinary REST op (file_pull or file_push). The caller is
// authenticated by authn; the leg is bound to that identity and to the leg
// token of the admission. Mount it at proto.FilesLegPathPrefix.
func (s *Service) Handler(authn Authenticate) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, proto.FilesLegPathPrefix) {
			legError(w, proto.CodeNotFound, "not found")
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodPut {
			w.Header().Set("Allow", "GET, PUT")
			legError(w, proto.CodeMethodNotAllowed, "use GET or PUT for a data leg")
			return
		}
		p, err := authn(r)
		if err != nil {
			code := proto.CodeOf(err)
			if code == "" || code == proto.CodeInternal {
				code = proto.CodeUnauthenticated
			}
			legError(w, code, "the request is not authenticated")
			return
		}
		if p.Kind == authz.KindWorker {
			// A Worker's legs come on its own connection (ServeWorkerLeg).
			legError(w, proto.CodeForbidden, "not allowed")
			return
		}
		s.ServeLeg(w, r, CallerLeg{Identity: identityOf(p)})
	})
}

// ServeWorkerLeg serves a caller leg that a Worker opened on its connection
// (an inbound fx stream): workerID and conn (the handshake transcript of that
// connection) are what the connection layer authenticated. It returns when
// the leg has ended.
func (s *Service) ServeWorkerLeg(ctx context.Context, workerID string, conn []byte, leg *transport.LegConn) error {
	caller := CallerLeg{Identity: WorkerIdentity(workerID), Conn: conn}
	srv := transport.NewLegServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.ServeLeg(w, r, caller)
	}))
	return transport.ServeLeg(ctx, srv, leg)
}
