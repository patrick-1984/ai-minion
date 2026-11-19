//go:build windows

package service

import (
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// The CI runner and a developer's account are not LocalSystem.
func TestWindowsIsSystemAccountIsFalseForAUser(t *testing.T) {
	if IsSystemAccount() {
		t.Error("the test process runs as LocalSystem")
	}
}

// One supervisor per user machine-wide: a second acquire finds the first, a
// stop signal reaches a supervisor that only holds the mutex (still starting)
// once its event exists, and the role is free again after release.
func TestWindowsSupervisorOwnershipAndStartingRace(t *testing.T) {
	if SupervisorRunning() {
		t.Skip("a supervisor of this user runs")
	}
	release, already, err := AcquireSupervisor()
	if err != nil || already {
		t.Fatalf("already=%v err=%v", already, err)
	}
	if _, again, err := AcquireSupervisor(); err != nil || !again {
		t.Fatalf("a second supervisor was allowed: again=%v err=%v", again, err)
	}
	// The event appears a little after the mutex, as in the launcher.
	type res struct {
		found bool
		err   error
	}
	got := make(chan res, 1)
	go func() { f, e := SignalSupervisorStop(); got <- res{f, e} }()
	time.Sleep(300 * time.Millisecond)
	stop, closeFn, err := CreateSupervisorStopEvent()
	if err != nil {
		t.Fatal(err)
	}
	if r := <-got; r.err != nil || !r.found {
		t.Fatalf("signal: %+v", r)
	}
	<-stop
	closeFn()
	release()
	if SupervisorRunning() {
		t.Error("the supervisor role was not released")
	}
	// A new supervisor does not inherit the signaled event.
	release, _, _ = AcquireSupervisor()
	stop2, closeFn2, err := CreateSupervisorStopEvent()
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-stop2:
		t.Error("the new supervisor saw the old signal")
	case <-time.After(500 * time.Millisecond):
	}
	closeFn2()
	release()
}

// An object owned by another account is not a supervisor of this user.
func TestWindowsOwnerAllowed(t *testing.T) {
	user, _ := windows.CreateWellKnownSid(windows.WinBuiltinUsersSid)
	system, _ := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	everyone, _ := windows.CreateWellKnownSid(windows.WinWorldSid)
	if !ownerAllowed(user, user, system, nil) || !ownerAllowed(system, user, system, nil) {
		t.Error("the user and SYSTEM may own the objects")
	}
	if ownerAllowed(everyone, user, system, nil) || ownerAllowed(nil, user, system, nil) {
		t.Error("another owner was accepted")
	}
	if !ownerAllowed(everyone, user, system, everyone) {
		t.Error("the default owner of the token may own the objects")
	}
}

func TestWindowsAcquireSupervisorRefusesAnObjectOfAnotherOwner(t *testing.T) {
	if SupervisorRunning() {
		t.Skip("a supervisor of this user runs")
	}
	// An object we create is ours, never ErrSupervisorNameTaken.
	release, already, err := AcquireSupervisor()
	if err != nil || already {
		t.Fatalf("already=%v err=%v", already, err)
	}
	defer release()
	if _, again, err := AcquireSupervisor(); err != nil || !again {
		t.Fatalf("our own mutex was refused: again=%v err=%v", again, err)
	}
}
