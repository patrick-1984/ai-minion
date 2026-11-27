//go:build linux && !android

package agent

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

type masterProcess struct {
	pid   int
	fd    int
	birth string
}

func masterPeerSupported() bool {
	pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return false
	}
	defer func() {
		_ = unix.Close(pair[0])
		_ = unix.Close(pair[1])
	}()
	fd, err := unix.GetsockoptInt(pair[0], unix.SOL_SOCKET, unix.SO_PEERPIDFD)
	if err != nil {
		return false
	}
	_ = unix.Close(fd)
	return true
}

// masterPeer uses the peer's kernel process descriptor, so a reused numeric
// PID cannot turn a connection from an already exited process into proof.
func masterPeer(conn *net.UnixConn) (*masterProcess, error) {
	raw, err := conn.SyscallConn()
	if err != nil {
		return nil, err
	}
	var creds *unix.Ucred
	var sockErr error
	fd := -1
	if err := raw.Control(func(socket uintptr) {
		creds, sockErr = unix.GetsockoptUcred(int(socket), unix.SOL_SOCKET, unix.SO_PEERCRED)
		if sockErr == nil && int(creds.Uid) == os.Getuid() && creds.Pid > 0 {
			fd, sockErr = unix.GetsockoptInt(int(socket), unix.SOL_SOCKET, unix.SO_PEERPIDFD)
			if sockErr == nil {
				unix.CloseOnExec(fd)
			}
		}
	}); err != nil {
		return nil, err
	}
	if sockErr != nil || fd < 0 {
		return nil, fmt.Errorf("cannot verify the local master peer")
	}
	return masterPin(int(creds.Pid), fd)
}

func masterPinPID(pid int) (*masterProcess, error) {
	fd, err := unix.PidfdOpen(pid, 0)
	if err != nil {
		return nil, err
	}
	return masterPin(pid, fd)
}

func masterPin(pid, fd int) (*masterProcess, error) {
	p := &masterProcess{pid: pid, fd: fd}
	_, birth, err := masterProcessInfo(pid)
	p.birth = birth
	if err != nil || !p.live() {
		p.close()
		return nil, fmt.Errorf("cannot pin the local process")
	}
	return p, nil
}

func (p *masterProcess) close() {
	if p != nil {
		_ = unix.Close(p.fd)
	}
}

func (p *masterProcess) live() bool {
	if p == nil {
		return false
	}
	// A pidfd is readable once its process has exited. poll is never
	// restarted after a signal handler, and the Go runtime preempts with
	// SIGURG, so EINTR says nothing about the process: ask again.
	n, err := unix.Poll([]unix.PollFd{{Fd: int32(p.fd), Events: unix.POLLIN}}, 0)
	for err == unix.EINTR {
		n, err = unix.Poll([]unix.PollFd{{Fd: int32(p.fd), Events: unix.POLLIN}}, 0)
	}
	if err != nil || n != 0 {
		return false
	}
	_, birth, err := masterProcessInfo(p.pid)
	return err == nil && p.birth == birth
}

func (p *masterProcess) same(other *masterProcess) bool {
	return p != nil && other != nil && p.pid == other.pid && p.birth == other.birth && p.live() && other.live()
}

func (p *masterProcess) imageIs(want os.FileInfo) bool {
	info, err := os.Stat("/proc/" + strconv.Itoa(p.pid) + "/exe")
	return err == nil && want != nil && os.SameFile(info, want)
}

func masterProcessInfo(pid int) (parent int, birth string, err error) {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0, "", err
	}
	s := string(data)
	// comm is in parentheses and may itself contain parentheses or spaces.
	end := strings.LastIndexByte(s, ')')
	fields := strings.Fields(s[end+1:])
	if end < 0 || len(fields) <= 19 {
		return 0, "", fmt.Errorf("invalid local process information")
	}
	parent, err = strconv.Atoi(fields[1])
	return parent, fields[19], err
}
