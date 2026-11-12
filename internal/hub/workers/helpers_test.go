package workers

import (
	"github.com/patrick-1984/ai-minion/internal/testutil/hubstore"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/hub/store"
)

// syncBuf is a log sink that tests may read while the Hub writes.
type syncBuf struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func textLogger(w *syncBuf) *slog.Logger {
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

func openStore(t *testing.T, dir string) *store.Store {
	t.Helper()
	st, err := hubstore.Open(t, dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}
