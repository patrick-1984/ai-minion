//go:build windows

package service

import "testing"

// The CI runner and a developer's account are not LocalSystem.
func TestIsSystemAccountIsFalseForAUser(t *testing.T) {
	if IsSystemAccount() {
		t.Error("the test process runs as LocalSystem")
	}
}

func TestSupervisorEventRoundTrip(t *testing.T) {
	if SupervisorRunning() {
		t.Skip("a supervisor of this session runs")
	}
	if found, err := SignalSupervisorStop(); err != nil || found {
		t.Fatalf("found=%v err=%v without a supervisor", found, err)
	}
	stop, closeFn, err := CreateSupervisorStopEvent()
	if err != nil {
		t.Fatal(err)
	}
	defer closeFn()
	if found, err := SignalSupervisorStop(); err != nil || !found {
		t.Fatalf("found=%v err=%v", found, err)
	}
	<-stop
}
