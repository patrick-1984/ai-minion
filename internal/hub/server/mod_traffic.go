package server

import (
	"net"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/workers"
	otelsink "github.com/patrick-1984/ai-minion/internal/obs/otel"
	"github.com/patrick-1984/ai-minion/internal/obs/traffic"
	"github.com/patrick-1984/ai-minion/internal/transport"
)

// hubTelemetry connects the Worker connections (internal/hub/workers) to the
// traffic log and the traffic metrics (internal/obs).
type hubTelemetry struct {
	log     *traffic.Log
	metrics *otelsink.Metrics
}

var _ workers.Telemetry = hubTelemetry{}

func (t hubTelemetry) FrameTap(workerID, name string) transport.FrameTap {
	var lt, mt transport.FrameTap
	if t.log != nil {
		lt = t.log.Tap(traffic.Peer{WorkerID: workerID, Name: name})
	}
	if t.metrics != nil {
		mt = traffic.MetricsTap(t.metrics, workerID)
	}
	return traffic.Taps(lt, mt)
}

func (t hubTelemetry) Conn(c net.Conn) (net.Conn, workers.ConnTelemetry) {
	if t.metrics == nil {
		return c, noConn{}
	}
	cc := traffic.NewCountingConn(c, t.metrics)
	return cc, cc
}

func (t hubTelemetry) OpRouted(workerID, op, outcome string, d time.Duration) {
	if t.metrics != nil {
		t.metrics.Op(workerID, op, outcome, d)
	}
}

func (t hubTelemetry) CallServed(_, op, outcome string) {
	if t.metrics != nil {
		t.metrics.Call(op, outcome)
	}
}

func (t hubTelemetry) Admission(lane string, admitted bool) {
	if t.metrics != nil {
		t.metrics.Admission(lane, admitted)
	}
}

func (t hubTelemetry) TurnIngest(result string) {
	if t.metrics != nil {
		t.metrics.TurnIngest(result)
	}
}

type noConn struct{}

func (noConn) Authenticated(string) {}
func (noConn) Closed(bool)          {}

// installTelemetry gives the Worker service the traffic log and metrics of
// the obs module, when the Hub runs them, and the metrics the number of
// Workers online.
func installTelemetry(env *Env, svc *workers.Service) {
	o, err := Require[*ObsService](env)
	if err != nil || (o.Traffic == nil && o.Metrics == nil) {
		return
	}
	if o.Metrics != nil {
		o.Metrics.SetOnline(func() int64 { return int64(svc.Registry().Len()) })
	}
	svc.SetTelemetry(hubTelemetry{log: o.Traffic, metrics: o.Metrics})
}
