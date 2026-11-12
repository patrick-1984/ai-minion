package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"time"
)

// The management listener (docs/architecture/proposals/hub-identity-v2.md,
// section 8.1 item 5): an explicitly configured plain-HTTP listener bound to a
// loopback address, not the listener to which a reverse proxy forwards. A
// request that arrives on it counts as HTTPS for the identity operations; no
// header, peer address or other listener can claim that.

type managementKey struct{}

// onManagementListener reports whether r arrived on the management listener.
// The marker is a context value set by the listener's own handler, so no
// request field can set it.
func onManagementListener(r *http.Request) bool {
	v, _ := r.Context().Value(managementKey{}).(bool)
	return v
}

// checkManagementListen validates MINION_MANAGEMENT_LISTEN: a loopback IP (or
// localhost) with a port other than the main listener's.
func checkManagementListen(addr, main string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("MINION_MANAGEMENT_LISTEN %q is not a valid address; use a loopback host:port such as 127.0.0.1:8444", addr)
	}
	if p, err := strconv.Atoi(port); err != nil || p < 0 || p > 65535 {
		return fmt.Errorf("MINION_MANAGEMENT_LISTEN %q has an invalid port", addr)
	}
	if !strings.EqualFold(host, "localhost") {
		ip, err := netip.ParseAddr(host)
		if err != nil || !ip.Unmap().IsLoopback() {
			return fmt.Errorf("MINION_MANAGEMENT_LISTEN %q must be a loopback address (127.0.0.1, ::1 or localhost): the management listener serves plain HTTP", addr)
		}
	}
	if _, mp, err := net.SplitHostPort(main); err == nil && mp == port && port != "0" {
		return errors.New("MINION_MANAGEMENT_LISTEN must not use the port of MINION_LISTEN")
	}
	return nil
}

type managementServer struct {
	srv  *http.Server
	ln   net.Listener
	addr string
	done chan struct{}
}

// startManagement binds addr and serves app on it as plain HTTP, marking every
// request as a management request.
func startManagement(addr string, app http.Handler, log *slog.Logger, bodyTimeout time.Duration) (*managementServer, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("cannot listen on %s for the management listener: %w", addr, err)
	}
	marked := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		app.ServeHTTP(w, r.WithContext(contextWithManagement(r.Context())))
	})
	m := &managementServer{ln: ln, addr: ln.Addr().String(), done: make(chan struct{})}
	m.srv = newHTTPServerBody(marked, log, http2WriteByteTimeout, bodyTimeout)
	go func() {
		defer close(m.done)
		if err := m.srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("management listener stopped", "error", err)
		}
	}()
	return m, nil
}

func contextWithManagement(ctx context.Context) context.Context {
	return context.WithValue(ctx, managementKey{}, true)
}

func (m *managementServer) shutdown(ctx context.Context) {
	if err := m.srv.Shutdown(ctx); err != nil {
		_ = m.srv.Close()
	}
	<-m.done
}

func (m *managementServer) close() { _ = m.srv.Close() }
