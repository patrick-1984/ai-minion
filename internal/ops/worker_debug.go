package ops

import "reflect"

// Names of the remote debug operations of a Worker (v0.10.0).
const (
	// OpWorkerDebug switches debug logging of a Worker on for a while, or off.
	// The Worker refuses it unless it was started with allow_debug_remote.
	OpWorkerDebug = "worker_debug"
	// OpWorkerLogs returns the most recent lines of the Worker's log, from the
	// Worker's memory. The Worker refuses it unless allow_debug_remote is on.
	OpWorkerLogs = "worker_logs"
)

// Bounds of the remote debug operations.
const (
	// DefaultDebugMinutes is how long debug logging stays on when the request
	// names no duration; MaxDebugMinutes is the longest a request may ask.
	DefaultDebugMinutes = 60
	MaxDebugMinutes     = 1440
	// DefaultLogLines is how many lines worker_logs returns when the request
	// names no number; MaxLogLines is the most it returns.
	DefaultLogLines = 500
	MaxLogLines     = 2000
)

// WorkerDebugArgs are the arguments of worker_debug.
type WorkerDebugArgs struct {
	// WorkerID names the Worker; required through the Hub.
	WorkerID string `json:"worker_id"`
	// Enabled switches debug logging on (true) or back to the configured level
	// (false).
	Enabled bool `json:"enabled"`
	// Minutes is how long debug logging stays on before it switches itself
	// off: 1 to MaxDebugMinutes, 0 means DefaultDebugMinutes. Ignored when
	// Enabled is false.
	Minutes int `json:"minutes,omitempty"`
}

// WorkerDebugState is the debug state of a Worker: the result of worker_debug
// and part of the result of worker_logs.
type WorkerDebugState struct {
	// Debug is true while the Worker logs at debug level, for any reason.
	Debug bool `json:"debug"`
	// Remote is true while debug logging that was switched on through the Hub
	// runs; Until is when it switches itself off.
	Remote bool       `json:"remote"`
	Until  *Timestamp `json:"until,omitempty"`
	// RemainingSeconds is the time left until Until, rounded down.
	RemainingSeconds int `json:"remaining_seconds,omitempty"`
	// Level is the level the Worker logs at now (debug, info, warn, error).
	Level string `json:"level"`
}

// WorkerLogsArgs are the arguments of worker_logs.
type WorkerLogsArgs struct {
	WorkerID string `json:"worker_id"`
	// Lines is how many of the most recent lines to return: 1 to MaxLogLines,
	// 0 means DefaultLogLines.
	Lines int `json:"lines,omitempty"`
}

// WorkerLogsResult is the tail of the Worker's in-memory log.
type WorkerLogsResult struct {
	WorkerDebugState
	// Lines are the most recent log lines, oldest first, as key=value text.
	// The log never holds terminal content, message bodies or secrets.
	Lines []string `json:"lines"`
	// Kept is how many lines the Worker holds now; Dropped how many older
	// lines were pushed out since it started.
	Kept    int    `json:"kept"`
	Dropped uint64 `json:"dropped"`
}

func init() {
	// The owner of the Worker (operate) or an administrator (manage). Both are
	// Mutates: POST-only and audited by the Hub, also worker_logs, which hands
	// out the Worker's log.
	owner := MustActionSet(ActionOperate, ActionManage)
	Register(Op{
		Name:    OpWorkerDebug,
		Summary: "Switch debug logging of a Worker on for a while (default 60 minutes, at most 1440) or off; the Worker must allow it (allow_debug_remote).",
		Scope:   ScopeWorker, Actions: owner, Mutates: true,
		Args: reflect.TypeFor[WorkerDebugArgs](), Result: reflect.TypeFor[WorkerDebugState](),
	})
	Register(Op{
		Name:    OpWorkerLogs,
		Summary: "Read the most recent lines of a Worker's log from its memory (default 500, at most 2000); the Worker must allow it (allow_debug_remote).",
		Scope:   ScopeWorker, Actions: owner, Mutates: true,
		Args: reflect.TypeFor[WorkerLogsArgs](), Result: reflect.TypeFor[WorkerLogsResult](),
	})
}
