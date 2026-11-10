package agent

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestLocksExcludeAndRelease(t *testing.T) {
	l := NewLocks()
	ctx := context.Background()
	unlock, err := l.Lock(ctx, "a", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	// Same key: times out. Other key: free.
	if _, err := l.Lock(ctx, "a", 20*time.Millisecond); !errors.Is(err, ErrLockTimeout) {
		t.Fatalf("second lock err = %v", err)
	}
	if _, err := l.Lock(ctx, "a", 0); !errors.Is(err, ErrLockTimeout) {
		t.Fatalf("try-lock err = %v", err)
	}
	other, err := l.Lock(ctx, "b", 0)
	if err != nil {
		t.Fatalf("other key: %v", err)
	}
	other()
	unlock()
	unlock() // releasing twice is harmless
	again, err := l.Lock(ctx, "a", 0)
	if err != nil {
		t.Fatalf("after unlock: %v", err)
	}
	again()
	if n := l.held(); n != 0 {
		t.Fatalf("%d entries leaked", n)
	}
}

func TestLockWaiterGetsTheLockOnRelease(t *testing.T) {
	l := NewLocks()
	ctx := context.Background()
	unlock, _ := l.Lock(ctx, "a", 0)
	got := make(chan error, 1)
	go func() {
		u, err := l.Lock(ctx, "a", 5*time.Second)
		if err == nil {
			u()
		}
		got <- err
	}()
	time.Sleep(30 * time.Millisecond)
	unlock()
	if err := <-got; err != nil {
		t.Fatalf("waiter: %v", err)
	}
	if n := l.held(); n != 0 {
		t.Fatalf("%d entries leaked", n)
	}
}

func TestLockHonoursTheContext(t *testing.T) {
	l := NewLocks()
	unlock, _ := l.Lock(context.Background(), "a", 0)
	defer unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := l.Lock(ctx, "a", 5*time.Second); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
	done, stop := context.WithCancel(context.Background())
	stop()
	if _, err := l.Lock(done, "free", time.Second); !errors.Is(err, context.Canceled) {
		t.Fatalf("a canceled context took a free lock: %v", err)
	}
}

func TestLocksAreMutuallyExclusiveUnderLoad(t *testing.T) {
	l := NewLocks()
	var inside, bad atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			u, err := l.Lock(context.Background(), PaneKey("%1"), 10*time.Second)
			if err != nil {
				bad.Add(1)
				return
			}
			if inside.Add(1) != 1 {
				bad.Add(1)
			}
			time.Sleep(time.Millisecond)
			inside.Add(-1)
			u()
		}()
	}
	wg.Wait()
	if bad.Load() != 0 || l.held() != 0 {
		t.Fatalf("violations %d, leaked entries %d", bad.Load(), l.held())
	}
}
