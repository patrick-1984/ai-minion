//go:build unix

package cli

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Review finding 6: MINION_JOIN_FILE is read once. A FIFO that is written
// once would block a second reader forever.
func TestWorkerUpReadsTheJoinFileOnce(t *testing.T) {
	text, _ := testJoinToken(t, "https://hub.example")
	fifo := filepath.Join(t.TempDir(), "join")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skip("no FIFO:", err)
	}
	go func() {
		w, err := os.OpenFile(fifo, os.O_WRONLY, 0)
		if err != nil {
			return
		}
		_, _ = w.WriteString(text + "\n")
		_ = w.Close()
	}()
	f := newUpFixture(t, false)
	var seen joinSeen
	joinHook(t, f, &seen)
	f.env["MINION_JOIN_FILE"] = fifo
	done := make(chan error, 1)
	go func() { _, err := f.run("--share-existing", "later"); done <- err }()
	select {
	case err := <-done:
		if err != nil || !seen.invite.Set {
			t.Fatalf("worker up: %v", err)
		}
	case <-time.After(10 * time.Second):
		// Unblock the second reader so that the test can end.
		if w, err := os.OpenFile(fifo, os.O_WRONLY, 0); err == nil {
			_ = w.Close()
		}
		t.Fatal("worker up read MINION_JOIN_FILE twice and blocked")
	}
	if strings.Contains(f.stderr.String(), text) {
		t.Error("the token reached stderr")
	}
}
