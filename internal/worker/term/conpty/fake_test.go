package conpty

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"sync"
	"time"
)

// fakeProc is an in-memory pseudo console: the test plays the program by
// writing to out and reads what the host typed from input().
type fakeProc struct {
	pid   int
	outR  *io.PipeReader
	outW  *io.PipeWriter
	exit  chan struct{}
	mu    sync.Mutex
	in    bytes.Buffer
	kills int
	// closes counts every Close call, including repeated ones: closing a real
	// pseudo console twice is a use-after-free.
	closes int
	// afterWrite, when set, runs after each successful write with the number of
	// writes so far, outside the fake's lock.
	afterWrite func(n int)
	writes     int

	exitOnce, closeOnce sync.Once
	writeErr            error
}

func newFakeProc(pid int) *fakeProc {
	r, w := io.Pipe()
	return &fakeProc{pid: pid, outR: r, outW: w, exit: make(chan struct{})}
}

func (f *fakeProc) Read(b []byte) (int, error) { return f.outR.Read(b) }
func (f *fakeProc) Write(b []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.writeErr != nil {
		return 0, f.writeErr
	}
	n, err := f.in.Write(b)
	f.writes++
	hook, count := f.afterWrite, f.writes
	f.mu.Unlock()
	if hook != nil {
		hook(count)
	}
	f.mu.Lock()
	return n, err
}
func (f *fakeProc) Pid() int    { return f.pid }
func (f *fakeProc) Wait() error { <-f.exit; return nil }
func (f *fakeProc) Kill() error { f.mu.Lock(); f.kills++; f.mu.Unlock(); f.exitNow(); return nil }
func (f *fakeProc) Close() error {
	f.mu.Lock()
	f.closes++
	f.mu.Unlock()
	f.closeOnce.Do(func() { _ = f.outW.Close() })
	return nil
}

// closeCount returns how often Close was called.
func (f *fakeProc) closeCount() int { f.mu.Lock(); defer f.mu.Unlock(); return f.closes }

func (f *fakeProc) setAfterWrite(fn func(n int)) { f.mu.Lock(); f.afterWrite = fn; f.mu.Unlock() }

func (f *fakeProc) exitNow() { f.exitOnce.Do(func() { close(f.exit) }) }

// show makes the program print s.
func (f *fakeProc) show(s string) { _, _ = f.outW.Write([]byte(s)) }

// input returns what the host has written to the program so far.
func (f *fakeProc) input() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.in.String()
}

func (f *fakeProc) setWriteErr(err error) { f.mu.Lock(); f.writeErr = err; f.mu.Unlock() }

// fakeStarter hands out fakeProcs and records the specs it was asked for.
type fakeStarter struct {
	mu    sync.Mutex
	specs []startSpec
	procs []*fakeProc
	fail  error
}

func (s *fakeStarter) start(spec startSpec) (proc, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail != nil {
		return nil, s.fail
	}
	p := newFakeProc(1000 + len(s.procs))
	s.specs = append(s.specs, spec)
	s.procs = append(s.procs, p)
	return p, nil
}

func (s *fakeStarter) proc(i int) *fakeProc {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.procs[i]
}

var errNoLook = errors.New("not found")

// newTestHost builds a host over a fakeStarter with fast drain timing. pwsh is
// found only when withPwsh is set.
func newTestHost(withPwsh bool) (*Host, *fakeStarter) {
	fs := &fakeStarter{}
	look := func(f string) (string, error) {
		switch {
		case f == "pwsh.exe": // the name the default shell lookup uses
			if withPwsh {
				return `C:\Program Files\PowerShell\7\pwsh.exe`, nil
			}
			return "", errNoLook
		case strings.HasPrefix(f, "missing"):
			return "", errNoLook
		}
		if !strings.Contains(f, ".") {
			f += ".exe"
		}
		return `C:\fakebin\` + f, nil
	}
	h := newHost(fs.start, look, time.Now)
	h.drainQuiet, h.drainMax, h.drainPoll = 20*time.Millisecond, 500*time.Millisecond, 5*time.Millisecond
	return h, fs
}
