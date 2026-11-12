package transport

import (
	"errors"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/identity"
)

// RC1 release gate, finding 6: the deadline passes while BeforeSigWorker runs
// (the pair_resume record write can block). Both clocks are read again right
// before sig_worker, which is then never sent.
func TestSigWorkerIsNotSentWhenTheDeadlinePassesDuringBeforeSigWorker(t *testing.T) {
	for _, tc := range []struct {
		name string
		jump func(c *fakeWM, a HubAuth)
	}{
		{"wall clock reaches not_after", func(c *fakeWM, a HubAuth) { c.set(time.UnixMilli(a.Expiry.WallMs), c.Mono()) }},
		{"monotonic clock reaches the deadline", func(c *fakeWM, a HubAuth) { c.set(c.Wall(), a.Expiry.DeadlineMono) }},
	} {
		for _, mode := range []Mode{ModePairCode, ModeConnect} {
			t.Run(tc.name+"/"+string(mode), func(t *testing.T) {
				var wc WorkerConfig
				var hc HubConfig
				if mode == ModePairCode {
					wc, hc, _ = pairConfigs(t)
				} else {
					wc, hc, _, _ = connectConfigs(t)
				}
				clk := &fakeWM{wall: time.Now(), mono: 7 * time.Second}
				wc.Clock = clk
				wc.BeforeSigWorker = func(a HubAuth) error { tc.jump(clk, a); return nil }
				o := runPair(t, wc, hc, nil)
				if !errors.Is(o.workerErr, identity.ErrCertExpired) {
					t.Fatalf("worker error %v, want the certificate expired before sig_worker", o.workerErr)
				}
				onlyHelloAndPake(t, o)
			})
		}
	}
}
