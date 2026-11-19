package agent

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/logx"
)

// TestPairingCodeNeverReachesARemoteSink: the pairing code line is a human
// notice. The local log keeps it at info as before; a remote sink built with
// logx.RemoteSafe gets nothing of it.
func TestPairingCodeNeverReachesARemoteSink(t *testing.T) {
	f := newPairFixture(t, pairWaitStep(nil))
	f.codeSeq("K7Q4XY12")
	remote := &logBuf{}
	r, cancel := f.run(f.pairer(func(c *PairerConfig) {
		c.Logger = slog.New(logx.Tee(
			slog.NewTextHandler(f.logs, &slog.HandlerOptions{Level: slog.LevelDebug}),
			logx.RemoteSafe(slog.NewJSONHandler(remote, &slog.HandlerOptions{Level: slog.LevelDebug})),
		))
	}))
	eventually(t, func() bool { return strings.Contains(f.logs.String(), "MN-K7Q4-XY12") })
	if !strings.Contains(f.logs.String(), `level=INFO msg="pairing code"`) {
		t.Fatalf("the local log lost the info line:\n%s", f.logs.String())
	}
	cancel()
	r.wait(t)
	if !errors.Is(r.err, context.Canceled) {
		t.Fatalf("err = %v", r.err)
	}
	for _, leak := range []string{"K7Q4", "k7q4", "XY12", `"pairing code"`} {
		if strings.Contains(remote.String(), leak) {
			t.Fatalf("the remote sink received %q:\n%s", leak, remote.String())
		}
	}
}
