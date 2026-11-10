//go:build windows

package conpty

import (
	"os/exec"
	"sync"
	"time"

	pty "github.com/aymanbagabas/go-pty"
)

// New returns the ConPTY session host. It is the Windows implementation of
// term.SessionHost (AD-13): each session has one pane, hosted inside the
// Worker process. Sessions do not survive a Worker restart.
func New() *Host {
	return newHost(startConPTY, exec.LookPath, time.Now)
}

// winProc is a program attached to a go-pty pseudo console.
type winProc struct {
	pty pty.Pty
	cmd *pty.Cmd

	// go-pty closes the pseudo console on every Close call without clearing
	// its handle, so a second call is a use-after-free.
	closeOnce sync.Once
	closeErr  error
}

func (p *winProc) Read(b []byte) (int, error)  { return p.pty.Read(b) }
func (p *winProc) Write(b []byte) (int, error) { return p.pty.Write(b) }
func (p *winProc) Pid() int                    { return p.cmd.Process.Pid }
func (p *winProc) Wait() error                 { return p.cmd.Wait() }
func (p *winProc) Kill() error                 { return p.cmd.Process.Kill() }
func (p *winProc) Close() error {
	p.closeOnce.Do(func() { p.closeErr = p.pty.Close() })
	return p.closeErr
}

// startConPTY creates a pseudo console of the requested size and starts the
// program in it. The command is not bound to any request context: a session
// outlives the call that created it.
func startConPTY(spec startSpec) (proc, error) {
	p, err := pty.New()
	if err != nil {
		return nil, err
	}
	if err := p.Resize(spec.cols, spec.rows); err != nil {
		_ = p.Close()
		return nil, err
	}
	cmd := p.Command(spec.path, spec.args...)
	cmd.Dir = spec.dir
	if err := cmd.Start(); err != nil {
		_ = p.Close()
		return nil, err
	}
	return &winProc{pty: p, cmd: cmd}, nil
}
