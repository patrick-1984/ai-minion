package server

import (
	"net"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/workers"
	"github.com/patrick-1984/ai-minion/internal/obs/traffic"
	"github.com/patrick-1984/ai-minion/internal/transport"
)

// hubTelemetry connects the Worker connections (internal/hub/workers) to the
// traffic log and the traffic metrics (internal/obs/traffic).
type hubTelemetry struct {
	log *traffic.Log
}

var _ workers.Telemetry = hubTelemetry{}

func (t hubTelemetry) FrameTap(workerID, name string) transport.FrameTap {
	if t.log == nil {
		return nil
	}
	return t.log.Tap(traffic.Peer{WorkerID: workerID, Name: name})
}

func (t hubTelemetry) Conn(c net.Conn) (net.Conn, workers.ConnTelemetry) { return c, noConn{} }
func (t hubTelemetry) OpRouted(string, string, string, time.Duration)    {}
func (t hubTelemetry) CallServed(string, string, string)                 {}
func (t hubTelemetry) Admission(string, bool)                            {}
func (t hubTelemetry) TurnIngest(string)                                 {}

type noConn struct{}

func (noConn) Authenticated(string) {}
func (noConn) Closed(bool)          {}

// installTelemetry gives the Worker service the traffic log of the obs
// module, when the Hub runs one.
func installTelemetry(env *Env, svc *workers.Service) {
	o, err := Require[*ObsService](env)
	if err != nil || o.Traffic == nil {
		return
	}
	svc.SetTelemetry(hubTelemetry{log: o.Traffic})
}
