package setup

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"
)

// The wizard must take exactly the lines it asks for: whatever follows stays
// unread in the shared stream for the next consumer (--invite-key -).
func TestLineReaderLeavesRestOfStreamUnread(t *testing.T) {
	in := strings.NewReader("first\r\nSECRET-LINE\nmore\n")
	rd := &lineReader{in: in}
	got, err := rd.next(context.Background())
	if err != nil || got != "first" {
		t.Fatalf("got %q, %v", got, err)
	}
	time.Sleep(20 * time.Millisecond) // a background reader would have consumed it by now
	rest, _ := io.ReadAll(in)
	if string(rest) != "SECRET-LINE\nmore\n" {
		t.Fatalf("wizard read ahead; remaining %q", rest)
	}
}

func TestLineReaderEOFAndPartialLine(t *testing.T) {
	rd := &lineReader{in: strings.NewReader("tail")}
	if got, err := rd.next(context.Background()); err != nil || got != "tail" {
		t.Fatalf("got %q, %v", got, err)
	}
	if _, err := rd.next(context.Background()); err != io.EOF {
		t.Fatalf("want EOF, got %v", err)
	}
}

func TestLineReaderCancelDoesNotHangAndHandsOverPending(t *testing.T) {
	pr, pw := io.Pipe()
	defer pw.Close()
	rd := &lineReader{in: pr}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := rd.next(ctx); err != context.DeadlineExceeded {
		t.Fatalf("want deadline, got %v", err)
	}
	go func() { _, _ = pw.Write([]byte("late\n")) }()
	got, err := rd.next(context.Background())
	if err != nil || got != "late" {
		t.Fatalf("pending read lost: %q, %v", got, err)
	}
}
