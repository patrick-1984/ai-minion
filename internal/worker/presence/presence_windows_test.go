//go:build windows

package presence

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func winLayout(t *testing.T) Layout {
	t.Helper()
	state := t.TempDir()
	return Layout{Scope: ScopeUser, UID: -1, StateDir: state, PrivateDir: filepath.Join(state, "presence")}
}

func TestWindowsPrivateRecordAndRootIsRefused(t *testing.T) {
	l := winLayout(t)
	p, err := New(Options{Layout: l, Instance: "w1", Version: "v1", Interval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = p.Stop() }()
	res := Best(Reader{UID: -1, PrivateDirs: []string{l.PrivateDir}}.Scan())
	if res.Status != StatusRunning {
		t.Fatalf("%+v %v", res, res.Err)
	}
	if _, err := New(Options{Layout: Layout{Scope: ScopeRoot, PublicRoot: `C:\x`}, Instance: "w2", Version: "v1"}); err == nil {
		t.Fatal("a root layout was accepted on Windows")
	}
}

// A reader that holds the record open (without FILE_SHARE_DELETE) blocks the
// rename; the bounded retry lets the replacement through once it lets go.
func TestWindowsReplacementWaitsForAReaderThatHoldsTheFile(t *testing.T) {
	l := winLayout(t)
	p, err := New(Options{Layout: l, Instance: "w3", Version: "v1", Interval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = p.Stop() }()
	f, err := os.Open(filepath.Join(l.PrivateDir, "w3.json"))
	if err != nil {
		t.Fatal(err)
	}
	go func() { time.Sleep(200 * time.Millisecond); _ = f.Close() }()
	if err := p.SetUsage([]byte(usageDoc(1))); err != nil {
		t.Fatal(err)
	}
	p.heartbeat()
	if res := Best(Reader{UID: -1, PrivateDirs: []string{l.PrivateDir}}.Scan()); res.Status != StatusRunning {
		t.Fatalf("%+v %v", res, res.Err)
	}
}
