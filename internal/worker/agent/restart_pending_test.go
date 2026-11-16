package agent

import (
	"github.com/patrick-1984/ai-minion/internal/worker/localapi"

	"context"
	"testing"
	"time"
)

// A restart request stops the running runtime and leaves the binary for the
// command to start in the same console (Windows foreground, v0.9.3).
func TestRequestRestartStopsTheRuntimeAndRecordsTheBinary(t *testing.T) {
	t.Cleanup(func() { pendingRestart.Store(nil) })
	if requestRestart("x") {
		t.Fatal("a restart was requested without a running runtime")
	}
	rt := NewRuntime(NewRegistry(), Options{StopTimeout: time.Second})
	done := make(chan error, 1)
	go func() { done <- rt.Run(context.Background(), Settings{}) }()
	deadline := time.Now().Add(5 * time.Second)
	for currentStop.Load() == nil && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if !requestRestart(`C:\minion.exe`) {
		t.Fatal("no runtime took the request")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the runtime did not stop")
	}
	if RestartPending() != `C:\minion.exe` {
		t.Fatalf("pending = %q", RestartPending())
	}
}

// OpLocalStop answers first and then asks the runtime to stop.
func TestLocalStopOpAsksTheRuntimeToStop(t *testing.T) {
	stopped := make(chan struct{})
	l := localExecutor{stop: func() { close(stopped) }}
	raw, err := l.Execute(context.Background(), localapi.Request{Op: OpLocalStop, Actions: localapi.FullActions()}, nil)
	if err != nil || string(raw) != `{"stopping":true}` {
		t.Fatalf("raw=%s err=%v", raw, err)
	}
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("no stop")
	}
}
