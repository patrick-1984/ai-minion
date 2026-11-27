//go:build linux && !android

package agent

import (
	"os"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/ops"
)

func TestMasterFileCallerMustRunBelowTheHarness(t *testing.T) {
	if !descendsFrom(os.Getpid(), os.Getppid()) || !descendsFrom(os.Getpid(), os.Getpid()) {
		t.Fatal("the test process does not run below its parent")
	}
	if descendsFrom(os.Getppid(), os.Getpid()) || descendsFrom(0, os.Getpid()) {
		t.Fatal("a parent runs below its child")
	}
}

func TestMasterFileCallWithoutLeaseIsNotHandled(t *testing.T) {
	policy := NewPolicy(nil)
	policy.SetWorkerID("worker")
	policy.SetOwnerID("owner")
	b := newMasterBroker(nil, &Executor{policy: policy}, nil)
	var out ops.FileTransferResult
	for _, pid := range []int{0, os.Getpid()} {
		handled, err := b.fileCall(t.Context(), "boss", pid, ops.OpFilePull, ops.FilePullArgs{}, &out)
		if handled || err != nil {
			t.Fatalf("pid %d without an attachment: handled %v, %v", pid, handled, err)
		}
	}
}
