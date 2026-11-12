package server

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/hub/httpbody"
)

// OrderCore is the init order of the core module. It provides the services
// every other module needs and therefore comes first.
const OrderCore = 0

// coreModuleName is the name of the module that provides the HTTP services.
const coreModuleName = "core"

// Defaults of the HTTP server.
const (
	// DefaultShutdownTimeout is how long a graceful shutdown waits for
	// in-flight requests.
	DefaultShutdownTimeout = 15 * time.Second

	readHeaderTimeout = 10 * time.Second
	idleTimeout       = 2 * time.Minute

	// requestBodyTimeout is how long a client may take to send a request body
	// (internal/hub/httpbody). The server has no read timeout of its own: the
	// Worker channel and the event streams are long-lived, so a deadline on
	// the connection would kill them. Only a request that has a body gets one.
	requestBodyTimeout = httpbody.DefaultTimeout

	// http2WriteByteTimeout closes an HTTP/2 connection to which no byte can
	// be written for this long while data is waiting. It bounds what stream
	// deadlines cannot: a client that stops reading its TCP socket blocks the
	// connection's socket write, and net/http queues a stream's RST_STREAM
	// (the effect of SetWriteDeadline) behind that very write, so the streaming
	// handlers (the event feed, the dashboard) would never be released. The
	// timeout is extended by every byte written and does not run while nothing
	// is waiting to be written, so an idle SSE stream or Worker channel and a
	// slow but reading client are not affected.
	http2WriteByteTimeout = 30 * time.Second
)

// newHTTPServer returns the Hub's http.Server around handler. Run uses it with
// [http2WriteByteTimeout]; tests pass a shorter one.
func newHTTPServer(handler http.Handler, errLog *slog.Logger, writeByteTimeout time.Duration) *http.Server {
	return newHTTPServerBody(handler, errLog, writeByteTimeout, requestBodyTimeout)
}

// newHTTPServerBody is [newHTTPServer] with the time a client may take to send
// a request body. The body guard is the outermost handler: it runs before the
// mux, the startup gate and every route, so no route can forget it.
func newHTTPServerBody(handler http.Handler, errLog *slog.Logger, writeByteTimeout, bodyTimeout time.Duration) *http.Server {
	return &http.Server{
		Handler:           httpbody.Guard(bodyTimeout)(handler),
		ReadHeaderTimeout: readHeaderTimeout,
		IdleTimeout:       idleTimeout,
		HTTP2:             &http.HTTP2Config{WriteByteTimeout: writeByteTimeout},
		ErrorLog:          slog.NewLogLogger(errLog.Handler(), slog.LevelWarn),
	}
}

// Config is the validated runtime configuration of one Hub run. Modules get
// it as a service: Require[*Config](env).
type Config struct {
	// DataDir is the data directory, already known to be writable.
	DataDir string
	// Listen is the listen address, host:port.
	Listen string
	// TLSOff serves plain HTTP (a proxy terminates TLS).
	TLSOff bool
	// CertFile and KeyFile name an operator-supplied certificate.
	CertFile, KeyFile string
	// PublicURL is the normalised MINION_PUBLIC_URL, possibly empty.
	PublicURL string
	// TrustedProxies are the proxies whose forwarding headers are honoured.
	TrustedProxies []netip.Prefix
	// ManagementListen is the optional loopback address of the management
	// listener: plain HTTP, the one place where a passphrase is accepted
	// without HTTPS (contract section 8.1 item 5). Empty means none.
	ManagementListen string
	// Logger receives the Hub's log records; nil means [slog.Default].
	Logger *slog.Logger
}

// NewConfig validates the raw settings and builds the Config. It fails, with
// an English message that says what to set, for an invalid listen address,
// public URL or proxy list, and when TLS is off without a public URL.
func NewConfig(hs config.HubServer, logger *slog.Logger) (*Config, error) {
	cfg := &Config{
		DataDir: hs.DataDir,
		Listen:  hs.Listen,
		TLSOff:  hs.TLS == config.TLSOff,
		Logger:  logger,
	}
	if cfg.Listen == "" {
		cfg.Listen = config.DefaultListen
	}
	if _, port, err := net.SplitHostPort(cfg.Listen); err != nil {
		return nil, fmt.Errorf("MINION_LISTEN %q is not a valid address; use host:port such as :8443", cfg.Listen)
	} else if p, err := strconv.Atoi(port); err != nil || p < 0 || p > 65535 {
		return nil, fmt.Errorf("MINION_LISTEN %q has an invalid port", cfg.Listen)
	}
	var err error
	if cfg.PublicURL, err = NormalizePublicURL(hs.PublicURL); err != nil {
		return nil, err
	}
	if cfg.TrustedProxies, err = ParseTrustedProxies(hs.TrustedProxies); err != nil {
		return nil, err
	}
	if hs.ManagementListen != "" {
		if err := checkManagementListen(hs.ManagementListen, cfg.Listen); err != nil {
			return nil, err
		}
		cfg.ManagementListen = hs.ManagementListen
	}
	if cfg.TLSOff {
		if cfg.PublicURL == "" {
			return nil, errors.New("MINION_TLS=off needs MINION_PUBLIC_URL (the external URL browsers use, for example https://hub.example.com): it decides the cookie Secure flag, the OIDC redirect URI and absolute links")
		}
		if hs.CertFile != "" {
			return nil, errors.New("MINION_TLS=off conflicts with MINION_TLS_CERT_FILE and MINION_TLS_KEY_FILE; unset one side")
		}
	}
	if (hs.CertFile == "") != (hs.KeyFile == "") {
		return nil, errors.New("set both MINION_TLS_CERT_FILE and MINION_TLS_KEY_FILE, or neither")
	}
	cfg.CertFile, cfg.KeyFile = hs.CertFile, hs.KeyFile
	return cfg, nil
}

func (c *Config) logger() *slog.Logger {
	if c.Logger != nil {
		return c.Logger
	}
	return slog.Default()
}

// runState carries the objects of one run to the core module through the
// context that [Kernel.Start] passes to every init function.
type runState struct {
	cfg    *Config
	net    *Network
	health *Health
	mux    *http.ServeMux
	root   *Root
}

type runStateKey struct{}

func init() {
	if err := registerCore(DefaultRegistry); err != nil {
		panic(err)
	}
}

// registerCore adds the core module to r. It provides the services that the
// HTTP server owns: *Config, *Network, *Health and the root *http.ServeMux.
// Modules register their routes on the mux during init; /healthz and /readyz
// are already there.
func registerCore(r *Registry) error {
	return r.Register(coreModuleName, OrderCore, initCore,
		DeclaresService[*Config](),
		DeclaresService[*Network](),
		DeclaresService[*Health](),
		DeclaresService[*http.ServeMux](),
		DeclaresService[*Root](),
	)
}

func initCore(ctx context.Context, env *Env) error {
	rs, ok := ctx.Value(runStateKey{}).(*runState)
	if !ok {
		return errors.New("the core module can only start through Server.Run")
	}
	rs.health.Register(rs.mux)
	if err := Provide(env, rs.cfg); err != nil {
		return err
	}
	if err := Provide(env, rs.net); err != nil {
		return err
	}
	if err := Provide(env, rs.health); err != nil {
		return err
	}
	if err := Provide(env, rs.mux); err != nil {
		return err
	}
	return Provide(env, rs.root)
}

// Root lets a module wrap the root handler. A *http.ServeMux answers some
// requests itself, before any route runs (the redirects for a path that is
// not clean or lacks its trailing slash), so only a wrapper sees them all.
// Modules call [Root.Wrap] during init; the wrappers apply once the Hub has
// started, in the order of the calls (the first call is the outermost).
type Root struct {
	mu    sync.Mutex
	wraps []func(http.Handler) http.Handler
	built http.Handler
}

// Wrap adds a wrapper. It fails once the Hub has started.
func (r *Root) Wrap(w func(http.Handler) http.Handler) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.built != nil {
		return errors.New("the root handler is already built; wrap it during init")
	}
	r.wraps = append(r.wraps, w)
	return nil
}

// build composes the wrappers around mux. It is called once, after the
// modules have started and before the Hub reports ready.
func (r *Root) build(mux http.Handler) {
	r.mu.Lock()
	defer r.mu.Unlock()
	h := mux
	for i := len(r.wraps) - 1; i >= 0; i-- {
		h = r.wraps[i](h)
	}
	r.built = h
}

// ServeHTTP serves through the composed handler.
func (r *Root) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	h := r.built
	r.mu.Unlock()
	h.ServeHTTP(w, req)
}

// Server is one Hub run: TLS, the module kernel and the HTTP listener.
type Server struct {
	cfg     *Config
	reg     *Registry
	reload  <-chan struct{}
	timeout time.Duration
	// bodyTimeout bounds the time a client takes to send a request body.
	bodyTimeout time.Duration

	started chan struct{}
	addr    atomic.Pointer[string]
	// mgmtAddr is the address of the management listener, when there is one.
	mgmtAddr atomic.Pointer[string]
}

// Option changes how [New] builds a Server. The defaults are what the Hub
// uses.
type Option func(*Server)

// WithReloadSignal replaces the SIGHUP handler: every receive reloads the
// certificate. It exists for tests.
func WithReloadSignal(ch <-chan struct{}) Option { return func(s *Server) { s.reload = ch } }

// WithBodyTimeout sets how long a client may take to send a request body. It
// exists for tests; the Hub uses [requestBodyTimeout].
func WithBodyTimeout(d time.Duration) Option { return func(s *Server) { s.bodyTimeout = d } }

// WithShutdownTimeout sets how long a graceful shutdown waits.
func WithShutdownTimeout(d time.Duration) Option { return func(s *Server) { s.timeout = d } }

// New returns a Server for cfg that runs the modules registered in reg.
func New(cfg *Config, reg *Registry, opts ...Option) *Server {
	s := &Server{cfg: cfg, reg: reg, timeout: DefaultShutdownTimeout, bodyTimeout: requestBodyTimeout, started: make(chan struct{})}
	for _, o := range opts {
		o(s)
	}
	return s
}

// Started is closed once the Hub accepts connections.
func (s *Server) Started() <-chan struct{} { return s.started }

// Addr returns the address the Hub listens on (with the real port when the
// configured port was 0), or "" before [Server.Started] is closed.
func (s *Server) Addr() string {
	if p := s.addr.Load(); p != nil {
		return *p
	}
	return ""
}

// ManagementAddr returns the address of the management listener (with the real
// port when the configured port was 0), or "" when there is none or the Hub has
// not started.
func (s *Server) ManagementAddr() string {
	if p := s.mgmtAddr.Load(); p != nil {
		return *p
	}
	return ""
}

// Run starts the Hub and blocks until ctx is cancelled, then shuts down
// gracefully. It returns nil after a clean shutdown. In order it
//
//  1. checks that the data dir is writable (before it opens any file there);
//  2. loads or generates the TLS certificate, logs its fingerprint once and
//     starts watching SIGHUP, so a reload signal never meets the default
//     action that ends the process;
//  3. binds the listen address and installs the package-level [PublicURL]
//     and [ClientIP], so that module init functions and start hooks can use
//     them;
//  4. starts serving: /healthz answers 200 and /readyz answers 503 right
//     away, also while the modules initialise (a long migration must not
//     look like a dead process to a liveness probe), and every other request
//     gets 503 until startup has succeeded;
//  5. starts the modules (the store opens the database and applies the
//     migrations) and marks the Hub ready;
//  6. serves until ctx ends, reloading the certificate on SIGHUP;
//  7. fails /readyz, drains requests, then stops the modules in reverse.
func (s *Server) Run(ctx context.Context) error {
	cfg := s.cfg
	log := cfg.logger()

	if err := CheckDataDir(cfg.DataDir); err != nil {
		return err
	}

	nw, err := NewNetwork(cfg.PublicURL, !cfg.TLSOff, cfg.TrustedProxies)
	if err != nil {
		return err
	}

	var certs *CertSource
	if !cfg.TLSOff {
		certs, err = LoadCertSource(CertOptions{
			DataDir:  cfg.DataDir,
			CertFile: cfg.CertFile,
			KeyFile:  cfg.KeyFile,
			Names:    certNames(cfg.PublicURL, cfg.Listen),
		})
		if err != nil {
			return err
		}
		log.Info("tls certificate",
			"fingerprint_sha256", certs.Fingerprint(),
			"source", certSource(certs),
			"generated", certs.Generated)
	}
	stopReload := s.watchReload(log, certs)
	defer stopReload()

	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return fmt.Errorf("cannot listen on %s: %w; set MINION_LISTEN to a free address", cfg.Listen, err)
	}
	defer ln.Close() //nolint:errcheck // closed again by Serve on the normal path

	setDefaultNetwork(nw)
	defer clearDefaultNetwork(nw)

	rs := &runState{cfg: cfg, net: nw, health: NewHealth(), mux: http.NewServeMux(), root: &Root{}}
	srv := newHTTPServerBody(startupGate(rs.health, rs.root), log, http2WriteByteTimeout, s.bodyTimeout)
	serveLn := ln
	if certs != nil {
		srv.TLSConfig = certs.TLSConfig()
		serveLn = tlsListener(ln, srv.TLSConfig)
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(serveLn) }()

	var mgmt *managementServer
	if cfg.ManagementListen != "" {
		mgmt, err = startManagement(cfg.ManagementListen, startupGate(rs.health, rs.root), log, s.bodyTimeout)
		if err != nil {
			_ = srv.Close()
			return err
		}
		defer mgmt.close()
		s.mgmtAddr.Store(&mgmt.addr)
	}

	kernel := NewKernel(s.reg)
	if err := kernel.Start(context.WithValue(ctx, runStateKey{}, rs)); err != nil {
		_ = srv.Close() // the kernel has already stopped the modules it started
		return err
	}

	rs.root.build(rs.mux)
	rs.health.MarkStarted()
	addr := ln.Addr().String()
	s.addr.Store(&addr)
	scheme := "https"
	if cfg.TLSOff {
		scheme = "http"
	}
	log.Info("hub listening", "addr", addr, "scheme", scheme, "public_url", cfg.PublicURL, "modules", kernel.Modules())
	if mgmt != nil {
		log.Info("management listener", "addr", mgmt.addr, "scheme", "http")
	}
	close(s.started)

	var runErr error
	select {
	case <-ctx.Done():
	case err := <-serveErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			runErr = fmt.Errorf("http server stopped: %w", err)
		}
	}

	rs.health.MarkStopping()
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.timeout)
	defer cancel()
	if mgmt != nil {
		mgmt.shutdown(sctx)
	}
	if err := srv.Shutdown(sctx); err != nil {
		_ = srv.Close()
		log.Warn("graceful shutdown timed out; connections were closed", "error", err)
	}
	if err := kernel.Stop(sctx); err != nil {
		runErr = errors.Join(runErr, err)
	}
	log.Info("hub stopped")
	return runErr
}

// startupGate is the root handler. The health endpoints are always served by
// h. Everything else goes to app, but only once the Hub has finished starting:
// before that the modules may still be registering their routes, so the
// answer is 503.
func startupGate(h *Health, app http.Handler) http.Handler {
	probes := http.NewServeMux()
	h.Register(probes)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/healthz", "/readyz":
			probes.ServeHTTP(w, r)
			return
		}
		if !h.Started() {
			w.Header().Set("Retry-After", "1")
			writeStatus(w, http.StatusServiceUnavailable, map[string]string{"status": "unavailable", "reason": "starting"})
			return
		}
		app.ServeHTTP(w, r)
	})
}

func certSource(c *CertSource) string {
	if c.SelfSigned {
		return "self-signed"
	}
	return "files"
}

// watchReload reloads the certificate on every reload signal until the
// returned stop function is called. Without the signal override it listens
// for SIGHUP.
func (s *Server) watchReload(log *slog.Logger, certs *CertSource) (stop func()) {
	ch := s.reload
	stopSignals := func() {}
	if ch == nil {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGHUP)
		hup := make(chan struct{}, 1)
		done := make(chan struct{})
		go func() {
			for {
				select {
				case <-sig:
					select {
					case hup <- struct{}{}:
					default:
					}
				case <-done:
					return
				}
			}
		}()
		ch = hup
		stopSignals = func() { signal.Stop(sig); close(done) }
	}
	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-ch:
				reloadCert(log, certs)
			case <-done:
				return
			}
		}
	}()
	return func() { stopSignals(); close(done) }
}

func reloadCert(log *slog.Logger, certs *CertSource) {
	if certs == nil {
		log.Info("reload requested; TLS is off, so there is no certificate to reload")
		return
	}
	if err := certs.Reload(); err != nil {
		log.Error("certificate reload failed; the previous certificate stays in service", "error", err)
		return
	}
	log.Info("tls certificate reloaded", "fingerprint_sha256", certs.Fingerprint())
}

// CheckDataDir proves that the Hub can write to its data directory before
// anything else touches the disk (A-14). A missing directory is created (mode
// 0700). When the directory cannot be created or written, the error says in
// English what to do, for example
//
//	data dir /data is not writable by uid 65532; run: chown 65532:65532 <host dir>
//
// The message names only the data dir; it never prints another path or a
// secret.
func CheckDataDir(dir string) error {
	return checkDataDir(dir, os.Geteuid(), os.Getegid())
}

// checkDataDir is CheckDataDir with the identity of the running user as
// arguments, so that tests can check the message for any uid.
func checkDataDir(dir string, uid, gid int) error {
	if dir == "" {
		return errors.New("the data dir is not set; set MINION_DATA_DIR to a directory the Hub may write to")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return fmt.Errorf("data dir %s is not a usable path: %w", dir, errors.Unwrap(err))
	}
	dir = abs

	info, err := os.Stat(dir)
	switch {
	case err == nil && !info.IsDir():
		return fmt.Errorf("data dir %s is not a directory; set MINION_DATA_DIR to a directory", dir)
	case errors.Is(err, fs.ErrNotExist), errors.Is(err, syscall.ENOTDIR):
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return uncreatable(dir, uid, gid, err)
		}
	case err != nil:
		return unwritable(dir, uid, gid, err)
	}

	f, err := os.CreateTemp(dir, ".minion-write-test-*")
	if err != nil {
		return unwritable(dir, uid, gid, err)
	}
	name := f.Name()
	_ = f.Close()
	if err := os.Remove(name); err != nil {
		return unwritable(dir, uid, gid, err)
	}
	return nil
}

// errnoText returns the reason of a file system error without any path: the
// data dir is already named in the message and nothing else may be printed.
func errnoText(cause error) string {
	var pe *fs.PathError
	if errors.As(cause, &pe) {
		return pe.Err.Error()
	}
	return cause.Error()
}

// uncreatable builds the actionable error for a data dir that does not exist
// and cannot be created, for example /data when no volume is mounted in
// Docker. A chown cannot fix that: the directory has to be created first.
func uncreatable(dir string, uid, gid int, cause error) error {
	reason := errnoText(cause)
	if runtime.GOOS == "windows" || uid < 0 {
		return fmt.Errorf("data dir %s does not exist and the account that runs the Hub cannot create it (%s); create it and give that account write access, or set MINION_DATA_DIR to another directory",
			dir, reason)
	}
	return fmt.Errorf("data dir %s does not exist and uid %d cannot create it; run: mkdir -p <host dir> && chown %d:%d <host dir> (or mount a host directory at %s, or use an init container) (%s)",
		dir, uid, uid, gid, dir, reason)
}

// unwritable builds the actionable error for an existing directory that
// cannot be read or written.
func unwritable(dir string, uid, gid int, cause error) error {
	reason := errnoText(cause)
	if runtime.GOOS == "windows" || uid < 0 {
		return fmt.Errorf("data dir %s is not writable (%s); give the account that runs the Hub write access to this directory, or set MINION_DATA_DIR to another directory",
			dir, reason)
	}
	return fmt.Errorf("data dir %s is not writable by uid %d; run: chown %d:%d <host dir> (%s; in Docker or Compose run it on the host directory mounted at %s, or use an init container that does the same)",
		dir, uid, uid, gid, reason, dir)
}
