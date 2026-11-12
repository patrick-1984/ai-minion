package server

import (
	"bytes"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/hubid/hubidtest"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
)

// lockedBuffer is a log sink that tests read while the timers write.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// RC1 release gate, finding 4: when the expiry check at not_after fails (here
// the identity row cannot be read), the timer is not stopped until the daily
// tick: the check is retried with a bounded backoff and closes the sessions as
// soon as it succeeds.
func TestAFailedExpiryCheckIsRetriedUntilTheSessionsClose(t *testing.T) {
	oldEvery, oldMax, oldDaily := identityRetryEvery, identityRetryMax, identityTickEvery
	identityRetryEvery, identityRetryMax, identityTickEvery = 50*time.Millisecond, 200*time.Millisecond, time.Hour
	t.Cleanup(func() { identityRetryEvery, identityRetryMax, identityTickEvery = oldEvery, oldMax, oldDaily })

	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	var mu sync.Mutex
	var off time.Duration
	now := func() time.Time { mu.Lock(); defer mu.Unlock(); return time.Now().Add(off) }
	mu.Lock()
	off = -180*24*time.Hour + 1500*time.Millisecond // the certificate expires 1.5 s from now
	mu.Unlock()
	svc := hubidtest.Open(t, st, t.TempDir(), hubidtest.WithNow(now))
	mu.Lock()
	off = 0
	mu.Unlock()
	var closes atomic.Int32
	svc.SetSessionCloser(func() { closes.Add(1) })
	base := closes.Load()

	logs := &lockedBuffer{}
	timers := newExpiryTimers(svc, slog.New(slog.NewTextHandler(logs, nil)), time.Now)
	timers.start(t.Context())
	t.Cleanup(timers.stop)

	// The status read fails from before not_after on.
	if _, err := st.DB().Exec(`ALTER TABLE hub_identity RENAME TO hub_identity_away`); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(logs.String(), "the expiry check failed") {
		if time.Now().After(deadline) {
			t.Fatalf("the expiry check never ran; logs:\n%s", logs)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if closes.Load() != base {
		t.Fatal("the sessions were closed although the check failed")
	}
	if _, err := st.DB().Exec(`ALTER TABLE hub_identity_away RENAME TO hub_identity`); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(5 * time.Second)
	for closes.Load() == base {
		if time.Now().After(deadline) {
			t.Fatalf("the sessions were not closed after the failed check; logs:\n%s", logs)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestExpiryRetryBackoffIsBounded(t *testing.T) {
	want := []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute, 4 * time.Minute, 5 * time.Minute, 5 * time.Minute}
	for i, w := range want {
		if got := retryAfter(tickFailed, i+1); got != w {
			t.Errorf("failure %d: wait %v, want %v", i+1, got, w)
		}
	}
	if got := retryAfter(tickBusy, 7); got != identityRetryEvery {
		t.Errorf("busy: wait %v", got)
	}
}
