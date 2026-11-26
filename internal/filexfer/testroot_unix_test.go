//go:build linux || darwin

package filexfer

import (
	"path/filepath"
	"testing"
)

// engineMode is the resolver the engine tests (transfer_unix_test.go) open
// their roots with: the strongest one of this host. TestEngineWalkResolver
// runs a round trip with ResolveWalk too.
func engineMode(t *testing.T) ResolveMode {
	t.Helper()
	modes := testModes(t)
	return modes[len(modes)-1]
}

// newTestRoot opens dir (inside t.TempDir()) as a real confined Root without
// a floor, closed at the end of the test.
func newTestRoot(t *testing.T, dir string) *Root {
	t.Helper()
	return newTestRootMode(t, dir, engineMode(t))
}

func newTestRootMode(t *testing.T, dir string, mode ResolveMode) *Root {
	t.Helper()
	canon, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	r, err := OpenDirRoot(canon, RootOptions{Mode: mode})
	if err != nil {
		t.Fatalf("open root %s: %v", mode, err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return r
}
