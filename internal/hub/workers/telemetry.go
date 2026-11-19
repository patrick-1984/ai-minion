package workers

import (
	"net"
	"sync/atomic"
	"time"

	"github.com/patrick-1984/ai-minion/internal/transport"
)

// Telemetry observes the Worker connections for the traffic log and the
// traffic metrics (internal/obs/traffic). Every method is called on the
// path of the traffic it observes: it must be cheap and must never block.
type Telemetry interface {
	// FrameTap returns the frame tap of an admitted Worker's session, or
	// nil. It is called once per connection, after the handshake.
	FrameTap(workerID, name string) transport.FrameTap
	// Conn wraps the connection returned by the WebSocket upgrade to count
	// its payload bytes (layer L1), and returns how to report the
	// connection's Worker and its end.
	Conn(c net.Conn) (net.Conn, ConnTelemetry)
	// OpRouted counts an op the Hub sent to a Worker (Router.Call/Stream).
	OpRouted(workerID, op, outcome string, d time.Duration)
	// CallServed counts a Worker->Hub call stream request.
	CallServed(workerID, op, outcome string)
	// Admission counts a stream admitted to or refused from a lane.
	Admission(lane string, admitted bool)
	// TurnIngest counts a pipe turn the Hub accepted, found busy or
	// rejected.
	TurnIngest(result string)
}

// ConnTelemetry follows one accepted connection.
type ConnTelemetry interface {
	// Authenticated is called once the handshake identified the Worker.
	Authenticated(workerID string)
	// Closed is called once when the connection ends; handshakeOK says
	// whether the handshake completed.
	Closed(handshakeOK bool)
}

// telemetryBox holds the Telemetry of a Handler, swapped without locks.
type telemetryBox struct{ p atomic.Pointer[Telemetry] }

func (b *telemetryBox) get() Telemetry {
	if p := b.p.Load(); p != nil {
		return *p
	}
	return nil
}

// Telemetry returns the installed telemetry, or nil.
func (s *Service) Telemetry() Telemetry { return s.handler.telemetry.get() }

// SetTelemetry installs t (nil removes it). Connections accepted before keep
// the telemetry they started with.
func (s *Service) SetTelemetry(t Telemetry) {
	if t == nil {
		s.handler.telemetry.p.Store(nil)
		return
	}
	s.handler.telemetry.p.Store(&t)
}
