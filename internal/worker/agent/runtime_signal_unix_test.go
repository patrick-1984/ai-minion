//go:build unix

package agent

import (
	"context"
	"os"
	"reflect"
	"syscall"
	"testing"
	"time"
)

// TestRunStopsOnSignals sends the real signals to the test process. Run
// registers its handler before the modules start, so the signal never reaches
// the default action.
func TestRunStopsOnSignals(t *testing.T) {
	for name, sig := range map[string]syscall.Signal{"SIGTERM": syscall.SIGTERM, "SIGINT": syscall.SIGINT} {
		t.Run(name, func(t *testing.T) {
			l := &eventLog{}
			r := NewRegistry()
			fakeModule(t, r, l, "a", 1)
			fakeModule(t, r, l, "b", 2)
			rt := NewRuntime(r, Options{})
			done := make(chan error, 1)
			go func() { done <- rt.Run(context.Background(), testSettings()) }()
			waitFor(t, func() bool { return len(l.get()) == 2 })
			if err := syscall.Kill(os.Getpid(), sig); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("Run did not return after the signal")
			}
			want := []string{"start a", "start b", "stop b", "stop a"}
			if got := l.get(); !reflect.DeepEqual(got, want) {
				t.Fatalf("events = %v, want %v", got, want)
			}
		})
	}
}
