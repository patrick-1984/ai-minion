package adminsock

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/api"
	"github.com/patrick-1984/ai-minion/internal/hub/hubid"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// SocketName is the file name of the admin socket in the data directory.
const SocketName = "admin.sock"

// Paths of the protocol. The command names are those of the CLI.
const (
	PathStatus           = "/v1/identity/status"
	PathSetPassphrase    = "/v1/identity/set-passphrase"
	PathReissue          = "/v1/identity/reissue"
	PathRotateMaster     = "/v1/identity/rotate-master"
	PathChangePassphrase = "/v1/identity/change-passphrase"
)

const (
	maxBody     = 16 << 10
	bodyTimeout = 15 * time.Second
)

// Path returns the admin socket path for a data directory.
func Path(dataDir string) string { return filepath.Join(dataDir, SocketName) }

// ActorForUID is the audit actor of a local command: "local:<uid>".
func ActorForUID(uid uint32) string { return "local:" + strconv.FormatUint(uint64(uid), 10) }

type ctxKey struct{}

// peerConn carries the kernel credentials read at accept time.
type peerConn struct {
	net.Conn
	uid    uint32
	credOK bool
}

type credListener struct{ net.Listener }

func (l credListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	pc := &peerConn{Conn: c}
	if uc, ok := c.(*net.UnixConn); ok {
		if uid, err := peerUID(uc); err == nil {
			pc.uid, pc.credOK = uid, true
		}
	}
	return pc, nil
}

// Server serves the admin socket.
type Server struct {
	ops  api.IdentityOps
	log  *slog.Logger
	http *http.Server
	// allowed is the user id of the Hub; root is always allowed too.
	allowed uint32
}

// NewServer returns the handler side of the socket. svc is the identity
// service; log receives internal failures (never a passphrase).
func NewServer(svc api.IdentityService, log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	s := &Server{ops: api.IdentityOps{Svc: svc, Log: log}, log: log, allowed: uint32(os.Geteuid())} //nolint:gosec // euid is non-negative
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+PathStatus, s.status)
	mux.HandleFunc("POST "+PathSetPassphrase, func(w http.ResponseWriter, r *http.Request) {
		run(s, w, r, func(ctx context.Context, actor hubid.Actor, a ops.HubIdentitySetPassphraseArgs) (ops.HubIdentityResult, error) {
			return s.ops.SetPassphrase(ctx, actor, a)
		})
	})
	mux.HandleFunc("POST "+PathReissue, func(w http.ResponseWriter, r *http.Request) {
		run(s, w, r, func(ctx context.Context, actor hubid.Actor, a ops.HubIdentityReissueArgs) (ops.HubIdentityResult, error) {
			return s.ops.Reissue(ctx, actor, a)
		})
	})
	mux.HandleFunc("POST "+PathRotateMaster, func(w http.ResponseWriter, r *http.Request) {
		run(s, w, r, func(ctx context.Context, actor hubid.Actor, a ops.HubIdentityRotateArgs) (ops.HubIdentityResult, error) {
			return s.ops.RotateMaster(ctx, actor, a)
		})
	})
	mux.HandleFunc("POST "+PathChangePassphrase, func(w http.ResponseWriter, r *http.Request) {
		run(s, w, r, func(ctx context.Context, actor hubid.Actor, a ops.HubIdentityChangePassphraseArgs) (ops.HubIdentityResult, error) {
			return s.ops.ChangePassphrase(ctx, actor, a)
		})
	})
	s.http = &http.Server{
		Handler:           http.MaxBytesHandler(mux, maxBody),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       30 * time.Second,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
		ConnContext: func(ctx context.Context, c net.Conn) context.Context {
			if pc, ok := c.(*peerConn); ok {
				return context.WithValue(ctx, ctxKey{}, pc)
			}
			return ctx
		},
	}
	return s
}

// Serve serves ln until Close. It returns nil after Close.
func (s *Server) Serve(ln net.Listener) error {
	err := s.http.Serve(credListener{ln})
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// Close stops the server and closes the connections.
func (s *Server) Close() error { return s.http.Close() }

// Shutdown stops the server gracefully.
func (s *Server) Shutdown(ctx context.Context) error { return s.http.Shutdown(ctx) }

// actor returns the audit actor of the peer, or writes the refusal. Only the
// Hub's own user and root are served; the uid comes from the kernel.
func (s *Server) actor(w http.ResponseWriter, r *http.Request) (hubid.Actor, bool) {
	pc, _ := r.Context().Value(ctxKey{}).(*peerConn)
	if pc == nil || !pc.credOK {
		api.WriteError(w, api.NewError(proto.CodeForbidden, "the identity of the caller could not be established"))
		return hubid.Actor{}, false
	}
	if pc.uid != 0 && pc.uid != s.allowed {
		api.WriteError(w, api.NewError(proto.CodeForbidden, "only the user that runs the Hub, or root, may use the admin socket"))
		return hubid.Actor{}, false
	}
	return hubid.Actor{ID: ActorForUID(pc.uid), Channel: hubid.ChannelCLI}, true
}

func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.actor(w, r); !ok {
		return
	}
	res, err := s.ops.Status(r.Context())
	reply(s, w, res, err)
}

func reply(s *Server, w http.ResponseWriter, res any, err error) {
	if err != nil {
		api.WriteError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if err := json.NewEncoder(w).Encode(res); err != nil {
		s.log.Error("admin socket: write the answer", "error", err)
	}
}

// run decodes the arguments of one command, validates them and calls fn as the
// peer. A body that is not application/json, has unknown members or trailing
// data is refused.
func run[A any](s *Server, w http.ResponseWriter, r *http.Request, fn func(context.Context, hubid.Actor, A) (ops.HubIdentityResult, error)) {
	actor, ok := s.actor(w, r)
	if !ok {
		return
	}
	_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(bodyTimeout))
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
		api.WriteError(w, api.NewError(proto.CodeInvalidArgument, "send the request as application/json"))
		return
	}
	var a A
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&a); err != nil {
		api.WriteError(w, api.NewError(proto.CodeInvalidArgument, "the request body is not valid"))
		return
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		api.WriteError(w, api.NewError(proto.CodeInvalidArgument, "the request body is not valid"))
		return
	}
	if v, ok := any(a).(ops.Validator); ok {
		if err := v.Validate(); err != nil {
			api.WriteError(w, err)
			return
		}
	}
	res, err := fn(r.Context(), actor, a)
	reply(s, w, res, err)
}
