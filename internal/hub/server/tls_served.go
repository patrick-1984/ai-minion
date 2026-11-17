package server

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"sync"
	"sync/atomic"

	"github.com/patrick-1984/ai-minion/internal/hub/workers"
)

// servedCerts records, per TLS connection, the key of the certificate the
// Hub's TLS listener actually served on it (design 4.6, addendum C5): after a
// reload (SIGHUP) a connection that started earlier still reports the
// certificate it got, not the one in service now.
//
// The http.Server's ConnContext runs for every accepted connection before its
// handshake: it registers an empty slot under the raw connection (the
// *tls.Conn's NetConn, which is what ClientHelloInfo.Conn carries) and puts
// the slot into the connection's context. The wrapped GetCertificate takes the
// slot out of the map and fills it. ConnState removes a slot whose connection
// ended without a handshake. The handler reads the slot after the handshake.
type servedCerts struct {
	mu    sync.Mutex
	slots map[net.Conn]*servedSlot
}

// servedSlot is the key hash served on one connection, nil until the
// handshake chose the certificate.
type servedSlot struct{ hash atomic.Pointer[[32]byte] }

func (s *servedSlot) key() []byte {
	if h := s.hash.Load(); h != nil {
		return h[:]
	}
	return nil
}

func newServedCerts() *servedCerts { return &servedCerts{slots: map[net.Conn]*servedSlot{}} }

// take removes and returns the slot of the raw connection c.
func (s *servedCerts) take(c net.Conn) *servedSlot {
	s.mu.Lock()
	defer s.mu.Unlock()
	slot := s.slots[c]
	delete(s.slots, c)
	return slot
}

// getCertificate wraps get: it records the key of the certificate it returns
// in the slot of the connection.
func (s *servedCerts) getCertificate(get func(*tls.ClientHelloInfo) (*tls.Certificate, error)) func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	return func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
		cert, err := get(hello)
		if err != nil || cert == nil || cert.Leaf == nil {
			return cert, err
		}
		if slot := s.take(hello.Conn); slot != nil {
			h := SPKIHash(cert.Leaf)
			slot.hash.Store(&h)
		}
		return cert, nil
	}
}

// connContext is the http.Server's ConnContext.
func (s *servedCerts) connContext(ctx context.Context, c net.Conn) context.Context {
	tc, ok := c.(*tls.Conn)
	if !ok {
		return ctx
	}
	slot := &servedSlot{}
	s.mu.Lock()
	s.slots[tc.NetConn()] = slot
	s.mu.Unlock()
	return workers.WithServedTLSKey(ctx, slot.key)
}

// connState is the http.Server's ConnState: it drops the slot of a
// connection that ended (or was taken over) without a handshake.
func (s *servedCerts) connState(c net.Conn, st http.ConnState) {
	if st != http.StateClosed && st != http.StateHijacked {
		return
	}
	if tc, ok := c.(*tls.Conn); ok {
		s.take(tc.NetConn())
	}
}

// install makes srv record the served certificate per connection; cfg is the
// TLS configuration of its listener.
func (s *servedCerts) install(srv *http.Server, cfg *tls.Config) {
	cfg.GetCertificate = s.getCertificate(cfg.GetCertificate)
	srv.ConnContext = s.connContext
	srv.ConnState = s.connState
}
