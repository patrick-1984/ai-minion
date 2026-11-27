//go:build linux && !android

package agent

import (
	"os"
	"sync"
	"syscall"
	"testing"
)

// A signal that interrupts the liveness poll is not a dead process. The Go
// runtime preempts goroutines with SIGURG, so under load a poll of a live
// process can return EINTR; that made TestMasterBrokerUsesTheSocketProcess
// flaky ("cannot pin the local process").
func TestMasterProcessLivenessSurvivesSignals(t *testing.T) {
	p, err := masterPinPID(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	defer p.close()
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					_ = syscall.Kill(os.Getpid(), syscall.SIGURG) // ignored by the runtime, interrupts syscalls
				}
			}
		}()
	}
	defer func() { close(stop); wg.Wait() }()
	for i := 0; i < 20000; i++ {
		if !p.live() {
			t.Fatalf("a live process was reported dead after %d checks", i)
		}
	}
}
