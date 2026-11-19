package compliance

import "github.com/patrick-1984/ai-minion/internal/ops"

// Path says through which front door a call came (design §5.3).
type Path uint8

const (
	// PathREST is the REST dispatcher: the browser (in process), the command
	// line, MCP and API keys.
	PathREST Path = 1 << iota
	// PathCall is the Worker call-stream server: pipe ops of Worker-bound
	// keys and the session-control grant ops.
	PathCall
)

// OutputKind is the content a read returns to the caller.
type OutputKind uint8

const (
	OutNone OutputKind = iota
	// OutScreen is the text of a snapshot (style spans dropped).
	OutScreen
	// OutCapture is the text of a capture.
	OutCapture
	// OutTurns is the text of pipe turns, once per turn id.
	OutTurns
)

// InputKind is the content a write asks the Hub to send into a session.
type InputKind uint8

const (
	InNone InputKind = iota
	InText
	InKeys
	InChoice
	InTask
	InMessage
	InCreate
)

// Row is the capture rule of one op: the paths on which it is captured, what
// the action record carries (documented here, produced by describe), the
// output and the input class.
type Row struct {
	Paths  Path
	Action string
	Output OutputKind
	Input  InputKind
}

const both = PathREST | PathCall

// Matrix is the typed boundary matrix of design §5.3, keyed by op name. It
// fails closed: an op that is not in it gets action records only, and a test
// fails when a Worker-scope op of the catalog has no row.
var Matrix = map[string]Row{
	ops.OpSessionsList:       {Paths: both, Action: "worker, session count"},
	ops.OpSessionSnapshot:    {Paths: both, Action: "target, pane id, rows x cols, payload sha256", Output: OutScreen},
	ops.OpSessionCapture:     {Paths: both, Action: "target, pane id, lines requested and returned, sha256", Output: OutCapture},
	ops.OpSessionCreate:      {Paths: PathREST, Action: "worker, session name, outcome", Input: InCreate},
	ops.OpSendText:           {Paths: both, Action: "target, request id, length, submit, outcome, replayed", Input: InText},
	ops.OpSendKeys:           {Paths: both, Action: "target, request id, key count, outcome, replayed", Input: InKeys},
	ops.OpAnswer:             {Paths: both, Action: "target, request id, outcome, replayed", Input: InChoice},
	ops.OpSetTask:            {Paths: both, Action: "target, request id, length, outcome, replayed", Input: InTask},
	ops.OpPipeSend:           {Paths: both, Action: "peer target, request id, length, outcome, replayed", Input: InMessage},
	ops.OpPipeRead:           {Paths: both, Action: "target, turn ids returned, withheld count", Output: OutTurns},
	ops.OpPipeWait:           {Paths: both, Action: "one record per element: turn id", Output: OutTurns},
	ops.OpShareGet:           {Paths: both, Action: "op, target, outcome"},
	ops.OpShareSet:           {Paths: both, Action: "op, target, outcome"},
	ops.OpWorkerDebug:        {Paths: both, Action: "op, target, outcome"},
	ops.OpWorkerLogs:         {Paths: both, Action: "op, target, outcome"},
	ops.OpWorkerUpdate:       {Paths: both, Action: "op, target, outcome"},
	// A Worker retires its own enrollment on its call stream (served by the
	// connect handler, not the call server): actions only.
	ops.OpWorkerRetire:       {Paths: PathCall, Action: "op, Worker, outcome"},
	ops.OpWorkerUpdateStatus: {Paths: both, Action: "op, target, outcome"},
}

// RowOf returns the row of op; an op without one has actions only (zero
// Row with both paths).
func RowOf(op string) Row {
	if r, ok := Matrix[op]; ok {
		return r
	}
	return Row{Paths: both, Action: "op, target, outcome"}
}

// ContentBearing reports whether op carries session content in either
// direction at the strictest level: the ops a stricter policy is refused for
// (the live notice, design §5.7).
func ContentBearing(op string) bool {
	r, ok := Matrix[op]
	return ok && (r.Output != OutNone || r.Input != InNone)
}
