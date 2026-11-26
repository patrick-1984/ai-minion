package devctl

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"
)

// Audit ops (DESIGN 4.8). Only these classes are ever recorded.
const (
	AuditStatus     = "status"
	AuditShell      = "shell"
	AuditScreencap  = "screencap"
	AuditInput      = "input"
	AuditEnable     = "enable"
	AuditDisable    = "disable"
	AuditRevoked    = "revoked"
	AuditProvenance = "provenance"
)

// TransportTLSLoopback is the only transport of v1.
const TransportTLSLoopback = "tls-loopback"

// Actors and sessions that are not a verified Hub user or a mapped session.
const (
	ActorLocal     = "local"
	SessionUnknown = "unknown"
)

// Entry is one audit record. It has no field for a command, its arguments,
// stdin, stdout, stderr, a screenshot, a pairing code, key material or the
// bridge token, so none can be recorded by mistake.
type Entry struct {
	Time       string `json:"time"`
	Actor      string `json:"actor"`
	Session    string `json:"session"`
	Op         string `json:"op"`
	Transport  string `json:"transport"`
	Result     string `json:"result"`
	DurationMS int64  `json:"duration_ms"`
}

// Results.
const (
	ResultOK = "ok"
)

// ResultExit is the result of a remote command that ended with code n.
func ResultExit(n int) string {
	if n == 0 {
		return ResultOK
	}
	return "exit:" + strconv.Itoa(n)
}

// ResultDenied is a refusal by policy.
func ResultDenied(reason string) string { return "denied:" + reason }

// ResultError is a failure of class code.
func ResultError(code string) string { return "error:" + code }

// Audit rotation (DESIGN 4.8): 1 MB per file, the current file and 3 old ones.
const (
	AuditMaxBytes = 1 << 20
	AuditKeep     = 3
	// AuditRecent is how many entries the status keeps for the Hub panel.
	AuditRecent = 20
)

// AuditLog is the local append-only audit file (JSON lines, 0600) with the
// most recent entries in memory. Mirror, when set, gets every entry too (the
// Worker logs it, which carries it to the remote log sinks and OTEL).
type AuditLog struct {
	path   string
	mirror func(Entry)

	mu     sync.Mutex
	recent []Entry
}

// NewAuditLog returns the audit log at path (state/adb/audit.log). An empty
// path keeps entries in memory only.
func NewAuditLog(path string, mirror func(Entry)) *AuditLog {
	return &AuditLog{path: path, mirror: mirror}
}

// Record appends e. A write failure is returned, but the entry is still kept
// in memory and mirrored: the audit must not silently lose a row.
func (a *AuditLog) Record(e Entry) error {
	if e.Time == "" {
		e.Time = time.Now().UTC().Format(time.RFC3339Nano)
	}
	if e.Actor == "" {
		e.Actor = ActorLocal
	}
	if e.Session == "" {
		e.Session = SessionUnknown
	}
	if e.Transport == "" {
		e.Transport = TransportTLSLoopback
	}
	a.mu.Lock()
	a.recent = append(a.recent, e)
	if len(a.recent) > AuditRecent {
		a.recent = a.recent[len(a.recent)-AuditRecent:]
	}
	err := a.append(e)
	a.mu.Unlock()
	if a.mirror != nil {
		a.mirror(e)
	}
	return err
}

// Recent returns the last entries, oldest first.
func (a *AuditLog) Recent() []Entry {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]Entry(nil), a.recent...)
}

func (a *AuditLog) append(e Entry) error {
	if a.path == "" {
		return nil
	}
	line, err := json.Marshal(e)
	if err != nil {
		return err
	}
	line = append(line, '\n')
	if err := os.MkdirAll(filepath.Dir(a.path), 0o700); err != nil {
		return err
	}
	if fi, err := os.Stat(a.path); err == nil && fi.Size()+int64(len(line)) > AuditMaxBytes {
		a.rotate()
	}
	f, err := os.OpenFile(a.path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	_, werr := f.Write(line)
	cerr := f.Close()
	if werr != nil {
		return werr
	}
	return cerr
}

// rotate moves audit.log to audit.log.1, .1 to .2 and so on; the oldest goes.
func (a *AuditLog) rotate() {
	_ = os.Remove(fmt.Sprintf("%s.%d", a.path, AuditKeep))
	for i := AuditKeep - 1; i >= 1; i-- {
		_ = os.Rename(fmt.Sprintf("%s.%d", a.path, i), fmt.Sprintf("%s.%d", a.path, i+1))
	}
	_ = os.Rename(a.path, a.path+".1")
}
