//go:build darwin

package filexfer

import (
	"context"
	"testing"
)

func openat2Works() bool { return false }

func TestProbeDarwin(t *testing.T) {
	r := Probe(context.Background(), ProbeCommand{Path: "/nonexistent"}, "")
	if r.Mode != ResolveWalk {
		t.Fatalf("probe on macOS = %+v, want the walk", r)
	}
}
