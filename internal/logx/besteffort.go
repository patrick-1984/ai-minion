package logx

import (
	"context"
	"log/slog"
	"time"
)

// BestEffortWait is how long [BestEffort] waits for one record.
const BestEffortWait = 200 * time.Millisecond

// BestEffort writes one record without letting a stalled output (a standard
// error that nobody reads, which holds the handler's lock) delay the caller,
// which is a stop sequence: the write goes on in the background and the
// caller waits for it at most [BestEffortWait].
func BestEffort(l *slog.Logger, level slog.Level, msg string, args ...any) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		l.Log(context.Background(), level, msg, args...)
	}()
	t := time.NewTimer(BestEffortWait)
	defer t.Stop()
	select {
	case <-done:
	case <-t.C:
	}
}
