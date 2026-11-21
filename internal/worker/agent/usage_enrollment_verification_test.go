package agent

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/ops"
)

func TestUsageEnrollmentWaitsForAcceptanceBeforeServingLink(t *testing.T) {
	f := newConnFixture(t)
	notices := &logBuf{}
	f.conn = f.newConnector(func(c *ConnectorConfig) {
		c.UsageConnectionAllowed = f.st.UsageConnectionAllowed
		c.Out = notices
	})
	var links atomic.Int32
	f.conn.OnLink("usage-verification", func(*Link) { links.Add(1) })
	first, second, accepted := newFakeSession(), newFakeSession(), newFakeSession()
	w := okWelcome()
	w.UsagePolicy = ops.UsageVersion
	f.dial.script(f.welcomeStep(first, w), f.welcomeStep(second, w), f.welcomeStep(accepted, w))
	f.run()
	f.waitState(ConnBackoff)
	if !first.closed.Load() || links.Load() != 0 {
		t.Fatal("the connector admitted a link without local monitoring acceptance")
	}
	if msg := f.conn.Status().LastError; !strings.Contains(msg, "minion worker config usage") {
		t.Fatalf("the pending enrollment status does not explain how to proceed: %q", msg)
	}
	if notice := strings.Join(strings.Fields(notices.String()), " "); !strings.Contains(notice, ops.UsageDisclosure) || !strings.Contains(notice, "minion worker config usage") {
		t.Fatalf("the enrollment announcement is missing its disclosure or local command: %q", notice)
	}
	f.step(time.Minute)
	eventually(t, func() bool { return second.closed.Load() })
	f.waitState(ConnBackoff)
	if links.Load() != 0 || strings.Count(strings.Join(strings.Fields(notices.String()), " "), ops.UsageDisclosure) != 1 {
		t.Fatal("a retry admitted the link or repeated the same monitoring announcement")
	}

	ctx := context.Background()
	en, err := f.st.LoadEnrollment(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.st.AcceptUsageRegistration(ctx, en.HubPub, en.WorkerID, en.Owner); err != nil {
		t.Fatal(err)
	}
	f.step(time.Minute)
	f.waitState(ConnConnected)
	eventually(t, func() bool { return links.Load() == 1 })
	if accepted.closed.Load() {
		t.Fatal("the accepted link was closed")
	}
}

func TestUsageEnrollmentKeepsLegacyHubLinkWithoutOptIn(t *testing.T) {
	f := newConnFixture(t)
	f.conn = f.newConnector(func(c *ConnectorConfig) {
		c.UsageConnectionAllowed = f.st.UsageConnectionAllowed
	})
	var links atomic.Int32
	f.conn.OnLink("legacy-verification", func(*Link) { links.Add(1) })
	f.dial.script(f.welcomeStep(newFakeSession(), okWelcome()))
	f.run()
	f.waitState(ConnConnected)
	eventually(t, func() bool { return links.Load() == 1 })
	consented, err := f.st.UsageRegistration(context.Background(), okWelcome().WorkerID)
	if err != nil || consented {
		t.Fatalf("legacy connection changed telemetry consent: %t, %v", consented, err)
	}
}

func TestUsageEnrollmentRejectsUnknownNewerPolicy(t *testing.T) {
	f := newConnFixture(t)
	f.conn = f.newConnector(func(c *ConnectorConfig) {
		c.UsageConnectionAllowed = func(context.Context, string) (bool, error) { return true, nil }
	})
	w := okWelcome()
	w.UsagePolicy = ops.UsageVersion + 1
	sess := newFakeSession()
	f.dial.script(f.welcomeStep(sess, w))
	f.run()
	f.waitState(ConnBackoff)
	if !sess.closed.Load() || !strings.Contains(f.conn.Status().LastError, "newer usage registration policy") {
		t.Fatalf("unknown monitoring policy was not refused: %+v", f.conn.Status())
	}
}
