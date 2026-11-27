package agent

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/transport"
)

// liveFakeSession is a fakeSession with the liveness view of a real session:
// the test sets when the Hub was last heard and whether a probe is answered.
type liveFakeSession struct {
	*fakeSession
	heard  atomic.Int64 // unix nanoseconds
	answer atomic.Bool
	probes atomic.Int32
}

func (s *liveFakeSession) LastRead() time.Time { return time.Unix(0, s.heard.Load()).UTC() }

func (s *liveFakeSession) Probe(context.Context, time.Duration) error {
	s.probes.Add(1)
	if s.answer.Load() {
		return nil
	}
	s.drop(transport.ErrKeepaliveTimeout)
	return transport.ErrKeepaliveTimeout
}

// The network came back while the connector waits out a long backoff: a
// Reconnect dials at once, without advancing the clock.
func TestReconnectEndsTheBackoffWaitAtOnce(t *testing.T) {
	f := newConnFixture(t)
	steps := make([]dialStep, 0, 8)
	for i := 0; i < 7; i++ {
		steps = append(steps, failStep("network is unreachable"))
	}
	sess := newFakeSession()
	steps = append(steps, f.welcomeStep(sess, okWelcome()))
	f.dial.script(steps...)
	f.run()
	for i := 0; i < 6; i++ {
		f.step(time.Minute)
	}
	eventually(t, func() bool { return f.dial.count() == 7 && f.conn.Status().State == ConnBackoff && f.clk.pending() > 0 })
	if got := f.conn.Reconnect("network-available"); got != ReconnectWoken {
		t.Fatalf("Reconnect = %q", got)
	}
	f.waitState(ConnConnected)
	if f.dial.count() != 8 {
		t.Fatalf("attempts = %d", f.dial.count())
	}
}

// A connected link that is half-open (the Hub restarted while the device's
// network was down): a Reconnect probes it, the probe fails and the
// connector redials at once.
func TestReconnectProbesAHalfOpenLinkAndRedials(t *testing.T) {
	f := newConnFixture(t)
	dead := &liveFakeSession{fakeSession: newFakeSession()}
	dead.heard.Store(f.clk.Now().Add(-10 * time.Minute).UnixNano())
	next := newFakeSession()
	f.dial.script(
		func(p ConnectParams) (*Attempt, error) {
			a, err := f.welcomeStep(nil, okWelcome())(p)
			a.Session = dead
			return a, err
		},
		f.welcomeStep(next, okWelcome()),
	)
	f.run()
	f.waitState(ConnConnected)
	st := f.conn.Status()
	if st.HeardAgo < 10*time.Minute || st.LastHeard.IsZero() {
		t.Fatalf("status of a silent link = %+v", st)
	}
	if got := f.conn.Reconnect("network-available"); got != ReconnectProbing {
		t.Fatalf("Reconnect = %q", got)
	}
	// The redial needs no clock step: the probe's failure nudges the backoff.
	eventually(t, func() bool { return f.dial.count() == 2 && f.conn.Status().State == ConnConnected })
	if dead.probes.Load() != 1 {
		t.Fatalf("probes = %d", dead.probes.Load())
	}
}

// A Reconnect on a link that answers keeps it.
func TestReconnectKeepsALiveLink(t *testing.T) {
	f := newConnFixture(t)
	ok := &liveFakeSession{fakeSession: newFakeSession()}
	ok.heard.Store(f.clk.Now().UnixNano())
	ok.answer.Store(true)
	f.dial.script(func(p ConnectParams) (*Attempt, error) {
		a, err := f.welcomeStep(nil, okWelcome())(p)
		a.Session = ok
		return a, err
	})
	f.run()
	f.waitState(ConnConnected)
	f.conn.Reconnect("network-available")
	eventually(t, func() bool { return ok.probes.Load() == 1 })
	time.Sleep(50 * time.Millisecond)
	if f.dial.count() != 1 || f.conn.Status().State != ConnConnected || ok.closed.Load() {
		t.Fatalf("a live link was dropped: attempts %d, status %+v", f.dial.count(), f.conn.Status())
	}
}

func TestReconnectWhileIdleSaysSo(t *testing.T) {
	f := newConnFixture(t, func(c *ConnectorConfig) { c.HubURL = "" })
	f.run()
	f.waitIdle(ReasonNoHubConfigured)
	if got := f.conn.Reconnect("x"); got != ReconnectIdle {
		t.Fatalf("Reconnect = %q", got)
	}
}

// The live status carries the silence, and the report says a connected but
// silent link is not verified.
func TestStatusReportsALinkThatIsNotVerified(t *testing.T) {
	for _, tc := range []struct {
		ago  time.Duration
		want bool
	}{{5 * time.Second, true}, {LinkFreshWithin + time.Second, false}} {
		live := LiveStatus{Connector: Status{State: ConnConnected, ConnectedSince: time.Now(), LastHeard: time.Now().Add(-tc.ago), HeardAgo: tc.ago}}
		reply := NewLiveStatusReply("/s", live)
		back, err := reply.LiveStatus()
		if err != nil {
			t.Fatal(err)
		}
		if back.Connector.HeardAgo != tc.ago.Truncate(time.Millisecond) || back.Connector.LastHeard.IsZero() {
			t.Fatalf("round trip = %+v", back.Connector)
		}
		r := BuildStatus(context.Background(), StatusInputs{HubURL: "https://hub.example", Live: staticLive{back}})
		c := r.Connection
		if c.Verified == nil || *c.Verified != tc.want || c.LastHeardAgeS == nil || *c.LastHeardAgeS != int64(tc.ago/time.Second) {
			t.Fatalf("ago %v: connection = %+v", tc.ago, c)
		}
	}
}

type staticLive struct{ ls LiveStatus }

func (s staticLive) LiveStatus(context.Context) (LiveStatus, error) { return s.ls, nil }

// The local op of the Android app's network callback reaches the connector.
func TestLocalReconnectOp(t *testing.T) {
	f := newConnFixture(t, func(c *ConnectorConfig) { c.HubURL = "" })
	f.run()
	f.waitIdle(ReasonNoHubConfigured)
	l := localExecutor{conn: f.conn}
	raw, err := l.Execute(context.Background(), localapiRequest(OpLocalReconnect, mustJSON(t, map[string]string{"reason": "network-available"})), func(json.RawMessage) {})
	if err != nil || string(raw) != `{"result":"idle"}` {
		t.Fatalf("reply %s, err %v", raw, err)
	}
	if _, err := (localExecutor{}).Execute(context.Background(), localapiRequest(OpLocalReconnect, nil), func(json.RawMessage) {}); proto.CodeOf(err) != proto.CodeUnsupportedOp {
		t.Fatalf("without a connector: %v", err)
	}
}
