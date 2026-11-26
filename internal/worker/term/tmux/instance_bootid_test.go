package tmux

import (
	"os"
	"runtime"
	"strings"
	"testing"
)

// TestOnlyLinuxHasABootIdentity: the instance id needs the kernel boot id,
// which only Linux exposes. On every other system (macOS included) the tmux
// host reports no instance ids and does not claim to, which is what the
// release note promises: instance ids on Linux tmux hosts with a readable boot
// id only.
func TestOnlyLinuxHasABootIdentity(t *testing.T) {
	got := strings.TrimSpace(readBootID())
	if runtime.GOOS != "linux" {
		if got != "" {
			t.Fatalf("readBootID on %s = %q, want none", runtime.GOOS, got)
		}
		if h := newFakeHost(t, &fake{}); h.ReportsInstances() {
			t.Fatalf("a tmux host on %s claims instance ids", runtime.GOOS)
		}
		return
	}
	if _, err := os.Stat("/proc/sys/kernel/random/boot_id"); err != nil {
		t.Skipf("no readable boot id here: %v", err)
	}
	if !validBootID(got) {
		t.Fatalf("the Linux boot id %q is not taken as a boot id", got)
	}
}
