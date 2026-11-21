//go:build a77_native && linux

package fixtures

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/testutil/tmuxtest"
	"golang.org/x/sys/unix"
)

type catalogState struct {
	Revision uint64
	Publish  bool
	FailList bool
}

type peerRequest struct {
	Kind        string
	Revision    uint64
	ClaimedPane string
	ToolCount   int
}

type peerReply struct {
	State   catalogState
	Allowed bool
}

type terminalPin struct {
	server, harness processPin
	session, name   string
	pane            string
	image           string
}

type brokerEvidence struct {
	PeerPID, Rejected, Lists, ListErrors, ListsAfterReopen, Denied, Delivered int
	LastToolCount                                                             int
	Applied, Listed                                                           uint64
	Retired                                                                   string
}

type fixtureBroker struct {
	tmux     *tmuxtest.Server
	self     string
	path     string
	mu       sync.Mutex
	listener *net.UnixListener
	opens    int
	binding  terminalPin
	peer     processPin
	state    catalogState
	active   bool
	evidence brokerEvidence
	closed   chan struct{}
}

func newBroker(t *testing.T, tmux *tmuxtest.Server, self, path string) *fixtureBroker {
	t.Helper()
	b := &fixtureBroker{tmux: tmux, self: self, path: path, binding: terminalPin{
		server: processPin{fd: -1}, harness: processPin{fd: -1},
	}, peer: processPin{fd: -1}}
	t.Cleanup(func() {
		b.close()
		b.binding.server.close()
		b.binding.harness.close()
		b.peer.close()
	})
	return b
}

func (b *fixtureBroker) open(t *testing.T) {
	t.Helper()
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.listener != nil {
		t.Fatal("broker already open")
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: b.path, Net: "unix"})
	must(t, err)
	must(t, os.Chmod(b.path, 0o600))
	b.listener, b.closed = listener, make(chan struct{})
	b.opens++
	b.state.Revision++
	go b.serve(listener, b.closed)
}

func (b *fixtureBroker) close() {
	b.mu.Lock()
	listener, closed := b.listener, b.closed
	b.listener, b.closed = nil, nil
	b.mu.Unlock()
	if listener != nil {
		_ = listener.Close()
		<-closed
	}
}

func (b *fixtureBroker) serve(listener *net.UnixListener, done chan struct{}) {
	defer close(done)
	for {
		conn, err := listener.AcceptUnix()
		if err != nil {
			return
		}
		b.servePeer(conn)
	}
}

func (b *fixtureBroker) servePeer(conn *net.UnixConn) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
	pid, err := peerPID(conn)
	if err != nil {
		return
	}
	var request peerRequest
	if json.NewDecoder(conn).Decode(&request) != nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	var reply peerReply
	switch {
	case !b.authenticPeer(pid, request):
		b.evidence.Rejected++
	case !b.liveBinding():
		// Keep the control connection usable so the test helper can withdraw its catalog.
		reply = peerReply{State: b.state, Allowed: request.Kind != "call"}
	default:
		reply = b.handle(request)
	}
	_ = json.NewEncoder(conn).Encode(reply)
}

func (b *fixtureBroker) handle(request peerRequest) peerReply {
	switch request.Kind {
	case "applied":
		b.evidence.Applied = request.Revision
	case "list":
		b.evidence.Lists++
		b.evidence.Listed = request.Revision
		b.evidence.LastToolCount = request.ToolCount
		if b.opens > 1 {
			b.evidence.ListsAfterReopen++
		}
		if b.state.FailList {
			b.evidence.ListErrors++
		}
	case "call":
		if b.active {
			b.evidence.Delivered++
		} else {
			b.evidence.Denied++
		}
	}
	return peerReply{State: b.state, Allowed: request.Kind != "call" || b.active}
}

func peerPID(conn *net.UnixConn) (int, error) {
	raw, err := conn.SyscallConn()
	if err != nil {
		return 0, err
	}
	var cred *unix.Ucred
	var getErr error
	if err := raw.Control(func(fd uintptr) {
		cred, getErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil {
		return 0, err
	}
	if getErr != nil {
		return 0, getErr
	}
	if cred.Pid <= 0 || int(cred.Uid) != os.Getuid() {
		return 0, fmt.Errorf("unexpected peer account or process")
	}
	return int(cred.Pid), nil
}

func (b *fixtureBroker) authenticPeer(pid int, _ peerRequest) bool {
	// Deliberately ignore all claimed pane/session information supplied by the client.
	parent, _, err := processStats(pid)
	if err != nil || parent != b.binding.harness.pid || !b.binding.harness.alive() ||
		!sameExecutable(pid, b.self) {
		return false
	}
	if b.peer.fd < 0 {
		pin, err := pinProcess(pid)
		if err != nil {
			return false
		}
		b.peer = pin
		b.evidence.PeerPID = pid
	}
	return b.peer.pid == pid && b.peer.alive()
}

func (b *fixtureBroker) liveBinding() bool {
	if b.evidence.Retired != "" {
		return false
	}
	matches, err := b.rows(b.binding.pane)
	switch {
	case err != nil, !b.binding.server.alive(), !b.binding.harness.alive(),
		!sameExecutable(b.binding.harness.pid, b.binding.image):
		b.evidence.Retired = "terminal or harness unavailable"
	case len(matches) > 1:
		b.evidence.Retired = "pane belongs to multiple sessions"
	case len(matches) != 1:
		b.evidence.Retired = "terminal mapping changed"
	case matches[0].server != b.binding.server.pid || matches[0].harness != b.binding.harness.pid ||
		matches[0].session != b.binding.session || matches[0].name != b.binding.name:
		b.evidence.Retired = "terminal mapping changed"
	default:
		return true
	}
	b.active = false
	b.state = catalogState{Revision: b.state.Revision + 1}
	return false
}

func (b *fixtureBroker) observeLifecycle() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.liveBinding()
}

type tmuxRow struct {
	server, harness int
	session, name   string
}

func (b *fixtureBroker) rows(pane string) ([]tmuxRow, error) {
	raw, err := b.tmux.Tmux("list-panes", "-a", "-F", "#{pane_id} #{pid} #{pane_pid} #{session_id} #{session_name}")
	if err != nil {
		return nil, err
	}
	var found []tmuxRow
	for _, line := range strings.Split(strings.TrimSpace(raw), "\n") {
		f := strings.Fields(line)
		if len(f) != 5 || f[0] != pane {
			continue
		}
		server, serverErr := strconv.Atoi(f[1])
		harness, harnessErr := strconv.Atoi(f[2])
		if serverErr != nil || harnessErr != nil {
			return nil, fmt.Errorf("invalid private tmux row")
		}
		found = append(found, tmuxRow{server: server, harness: harness, session: f[3], name: f[4]})
	}
	return found, nil
}

func (b *fixtureBroker) pin(t *testing.T, target string) {
	t.Helper()
	raw, err := b.tmux.Tmux("display-message", "-p", "-t", target, "#{pane_id}")
	must(t, err)
	pane := strings.TrimSpace(raw)
	claude, err := filepath.EvalSymlinks(findClaude(t))
	must(t, err)
	var row tmuxRow
	eventually(t, "tmux pane launched the vendor executable", func() bool {
		matches, err := b.rows(pane)
		if err != nil || len(matches) != 1 || !sameExecutable(matches[0].harness, claude) {
			return false
		}
		row = matches[0]
		return true
	}, func() string { return pane })
	terminal, err := pinProcess(row.harness)
	must(t, err)
	server, err := pinProcess(row.server)
	must(t, err)
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.binding.harness.pid != terminal.pid {
		b.peer.close()
		b.peer = processPin{fd: -1}
	}
	b.binding.server.close()
	b.binding.harness.close()
	b.binding = terminalPin{server: server, harness: terminal, session: row.session, name: row.name, pane: pane, image: claude}
	b.evidence.Retired = ""
}

func (b *fixtureBroker) pane() terminalPin {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.binding
}

func (b *fixtureBroker) set(active, publish, failList bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.active = active
	b.state = catalogState{Revision: b.state.Revision + 1, Publish: publish, FailList: failList}
}

func (b *fixtureBroker) revision() uint64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.state.Revision
}

func (b *fixtureBroker) evidenceNow() brokerEvidence {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.evidence
}

func (b *fixtureBroker) wait(t *testing.T, label string, condition func(brokerEvidence) bool) {
	t.Helper()
	eventually(t, label, func() bool { return condition(b.evidenceNow()) },
		func() string { return fmt.Sprintf("%+v", b.evidenceNow()) })
}

func (b *fixtureBroker) waitState(t *testing.T, label string) {
	t.Helper()
	rev := b.revision()
	b.wait(t, label, func(s brokerEvidence) bool { return s.Applied == rev && s.Listed == rev })
}

type processPin struct {
	pid, fd int
	birth   string
}

func pinProcess(pid int) (processPin, error) {
	fd, err := unix.PidfdOpen(pid, 0)
	if err != nil {
		return processPin{fd: -1}, err
	}
	_, born, err := processStats(pid)
	if err != nil {
		_ = unix.Close(fd)
		return processPin{fd: -1}, err
	}
	return processPin{pid: pid, fd: fd, birth: born}, nil
}

func (p processPin) alive() bool {
	if p.fd < 0 {
		return false
	}
	n, err := unix.Poll([]unix.PollFd{{Fd: int32(p.fd), Events: unix.POLLIN}}, 0)
	if err != nil || n != 0 {
		return false
	}
	_, born, err := processStats(p.pid)
	return err == nil && born == p.birth
}

func (p processPin) close() {
	if p.fd >= 0 {
		_ = unix.Close(p.fd)
	}
}

func stopFixture(p processPin) {
	if !p.alive() {
		return
	}
	_ = unix.PidfdSendSignal(p.fd, unix.SIGTERM, nil, 0)
	events := []unix.PollFd{{Fd: int32(p.fd), Events: unix.POLLIN}}
	if n, _ := unix.Poll(events, 3000); n == 0 && p.alive() {
		_ = unix.PidfdSendSignal(p.fd, unix.SIGKILL, nil, 0)
		_, _ = unix.Poll(events, 2000)
	}
}

func processStats(pid int) (parent int, birth string, err error) {
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, "", err
	}
	end := strings.LastIndexByte(string(raw), ')')
	fields := strings.Fields(string(raw[end+1:]))
	if end < 1 || len(fields) <= 19 {
		return 0, "", fmt.Errorf("malformed process metadata")
	}
	parent, err = strconv.Atoi(fields[1])
	return parent, fields[19], err
}

func sameExecutable(pid int, path string) bool {
	running, err := os.Stat(fmt.Sprintf("/proc/%d/exe", pid))
	if err != nil {
		return false
	}
	expected, err := os.Stat(path)
	return err == nil && os.SameFile(running, expected)
}

func requestBroker(ctx context.Context, path string, req peerRequest) (peerReply, error) {
	conn, err := (&net.Dialer{Timeout: 350 * time.Millisecond}).DialContext(ctx, "unix", path)
	if err != nil {
		return peerReply{}, err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(time.Second))
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return peerReply{}, err
	}
	var reply peerReply
	err = json.NewDecoder(conn).Decode(&reply)
	return reply, err
}
