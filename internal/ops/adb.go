package ops

import "reflect"

// Device control (on-device adb) of the Android Worker
// (docs/architecture/device-control-bridge.md). The ops are Unreleased until
// the device gate passes (DESIGN section 9): the MCP server offers no tool for
// them and the generic CLI commands are hidden; `minion adb ...` is the
// command surface.
//
// adb_shell, adb_screencap and adb_input are local-only: the Worker refuses
// them from the Hub, so only a session of this Worker (through its control
// socket) can use them. adb_status and adb_disable are routed by the Hub too:
// the Worker page shows the status and can disable, never enable.
const (
	OpAdbStatus    = "adb_status"
	OpAdbShell     = "adb_shell"
	OpAdbScreencap = "adb_screencap"
	OpAdbInput     = "adb_input"
	OpAdbDisable   = "adb_disable"
)

// AdbOpNames lists the device control ops, in catalog order.
var AdbOpNames = []string{OpAdbStatus, OpAdbShell, OpAdbScreencap, OpAdbInput, OpAdbDisable}

// AdbHubOpNames are the device control ops the Hub routes.
var AdbHubOpNames = []string{OpAdbStatus, OpAdbDisable}

// Limits of the device control ops (DESIGN 4.6).
const (
	// AdbDefaultTimeoutS is the command timeout when timeout_s is 0.
	AdbDefaultTimeoutS = 120
	// AdbMaxTimeoutS caps timeout_s.
	AdbMaxTimeoutS = 3600
	// AdbMaxCommand caps the command line of adb_shell in bytes.
	AdbMaxCommand = 64 << 10
	// AdbMaxInputArgs caps the arguments of adb_input.
	AdbMaxInputArgs = 8
)

func validAdbWorker(id string) error {
	if id != "" && !ValidUUIDv7(id) {
		return invalid("worker_id", "must be a UUIDv7")
	}
	return nil
}

// AdbStatusArgs are the arguments of adb_status.
type AdbStatusArgs struct {
	WorkerID string `json:"worker_id,omitempty"`
}

// Validate checks the Worker id.
func (a AdbStatusArgs) Validate() error { return validAdbWorker(a.WorkerID) }

// AdbAuditEntry is one device control audit record: who, which operation
// class, how and with which result. It never holds a command, its arguments,
// output, a screenshot, a pairing code, key material or a token.
type AdbAuditEntry struct {
	Time       string `json:"time"`
	Actor      string `json:"actor"`
	Session    string `json:"session"`
	Op         string `json:"op"`
	Transport  string `json:"transport"`
	Result     string `json:"result"`
	DurationMS int64  `json:"duration_ms"`
}

// DeviceControlStatus is the device control status of a Worker: the
// device_control block of `worker status --json` and the result of
// adb_status.
type DeviceControlStatus struct {
	// Available: Android, the app's bridge answers, and the app build has the
	// feature.
	Available bool `json:"available"`
	// Enabled is the owner's on-device choice.
	Enabled bool `json:"enabled"`
	// State is OFF, UNPAIRED, PAIRED, AWAITING_AUTHORIZATION (legacy adb: the
	// device shows "Allow debugging?"), READY, WAITING_WIFI or
	// ENDPOINT_UNVERIFIED.
	State string `json:"state"`
	// Port is the adbd port on 127.0.0.1 (5555 for legacy adb).
	Port int `json:"port,omitempty"`
	// Transport is "legacy-loopback" (adb on 127.0.0.1:5555, tried first) or
	// "tls-loopback" (Wireless debugging) while connected.
	Transport string `json:"transport,omitempty"`
	// KeyStorage is "android-keystore (<level>)" or "app-file".
	KeyStorage    string `json:"key_storage,omitempty"`
	ActiveStreams int    `json:"active_streams"`
	LastOK        string `json:"last_ok,omitempty"`
	LastError     string `json:"last_error,omitempty"`
	// Shared is true when the Worker shares sessions; device control is then
	// refused and disabled.
	Shared bool `json:"shared"`
	// Reason says why device control cannot be used now; empty when ready.
	Reason string `json:"reason,omitempty"`
}

// AdbStatusResult is the result of adb_status.
type AdbStatusResult struct {
	DeviceControl DeviceControlStatus `json:"device_control"`
	// Audit holds the last audit entries (at most 20), oldest first.
	Audit []AdbAuditEntry `json:"audit"`
}

// AdbShellArgs are the arguments of adb_shell.
type AdbShellArgs struct {
	WorkerID string `json:"worker_id,omitempty"`
	// Argv is the command after `--`: one argument is the command line itself,
	// several are quoted one by one.
	Argv []string `json:"argv"`
	// TimeoutS bounds the command; 0 is 120, at most 3600.
	TimeoutS int `json:"timeout_s,omitempty"`
}

// Validate checks the Worker id, the command and the timeout.
func (a AdbShellArgs) Validate() error {
	if err := validAdbWorker(a.WorkerID); err != nil {
		return err
	}
	if len(a.Argv) == 0 {
		return invalid("argv", "needs the command to run")
	}
	n := 0
	for _, s := range a.Argv {
		n += len(s) + 1
	}
	if n > AdbMaxCommand {
		return invalid("argv", "the command is longer than %d bytes", AdbMaxCommand)
	}
	return validAdbTimeout(a.TimeoutS)
}

func validAdbTimeout(s int) error {
	if s < 0 || s > AdbMaxTimeoutS {
		return invalid("timeout_s", "must be 0 to %d seconds", AdbMaxTimeoutS)
	}
	return nil
}

// AdbScreencapArgs are the arguments of adb_screencap.
type AdbScreencapArgs struct {
	WorkerID string `json:"worker_id,omitempty"`
	TimeoutS int    `json:"timeout_s,omitempty"`
}

// Validate checks the Worker id and the timeout.
func (a AdbScreencapArgs) Validate() error {
	if err := validAdbWorker(a.WorkerID); err != nil {
		return err
	}
	return validAdbTimeout(a.TimeoutS)
}

// AdbInputArgs are the arguments of adb_input: keyevent, text, tap or swipe and
// its arguments, validated by the Worker before anything is sent.
type AdbInputArgs struct {
	WorkerID string   `json:"worker_id,omitempty"`
	Kind     string   `json:"kind"`
	Args     []string `json:"args"`
}

// Validate checks the shape; the Worker validates the values.
func (a AdbInputArgs) Validate() error {
	if err := validAdbWorker(a.WorkerID); err != nil {
		return err
	}
	switch a.Kind {
	case "keyevent", "text", "tap", "swipe":
	default:
		return invalid("kind", "must be keyevent, text, tap or swipe")
	}
	if len(a.Args) == 0 || len(a.Args) > AdbMaxInputArgs {
		return invalid("args", "takes 1 to %d arguments", AdbMaxInputArgs)
	}
	return nil
}

// AdbOutput is one progress element of adb_shell and adb_screencap: a chunk of
// the remote stdout or stderr (base64 in JSON).
type AdbOutput struct {
	Stream string `json:"stream"`
	Data   []byte `json:"data"`
}

// AdbRunResult is the result of adb_shell, adb_screencap and adb_input: the
// remote exit code. A Minion-side failure is an error instead.
type AdbRunResult struct {
	ExitCode int `json:"exit_code"`
}

// AdbDisableArgs are the arguments of adb_disable.
type AdbDisableArgs struct {
	WorkerID string `json:"worker_id,omitempty"`
}

// Validate checks the Worker id.
func (a AdbDisableArgs) Validate() error { return validAdbWorker(a.WorkerID) }

// AdbDisableResult is the result of adb_disable: the status after it.
type AdbDisableResult struct {
	DeviceControl DeviceControlStatus `json:"device_control"`
}

func init() {
	read := MustActionSet(ActionRead)
	operate := MustActionSet(ActionOperate)
	Register(Op{
		Name: OpAdbStatus, Summary: "Report the device control (on-device adb) status of an Android Worker and its last audit entries.",
		Scope: ScopeWorker, Actions: read, Unreleased: true,
		Args: reflect.TypeFor[AdbStatusArgs](), Result: reflect.TypeFor[AdbStatusResult](),
	})
	Register(Op{
		Name: OpAdbShell, Summary: "Run a non-interactive shell command with shell-uid rights on this Android Worker's own device (local sessions only).",
		Scope: ScopeWorker, Actions: operate, Mutates: true, Unreleased: true,
		Args: reflect.TypeFor[AdbShellArgs](), Result: reflect.TypeFor[AdbRunResult](),
	})
	Register(Op{
		Name: OpAdbScreencap, Summary: "Capture the screen of this Android Worker's device as PNG (local sessions only).",
		Scope: ScopeWorker, Actions: operate, Mutates: true, Unreleased: true,
		Args: reflect.TypeFor[AdbScreencapArgs](), Result: reflect.TypeFor[AdbRunResult](),
	})
	Register(Op{
		Name: OpAdbInput, Summary: "Send a validated key event, text, tap or swipe to this Android Worker's device (local sessions only).",
		Scope: ScopeWorker, Actions: operate, Mutates: true, Unreleased: true,
		Args: reflect.TypeFor[AdbInputArgs](), Result: reflect.TypeFor[AdbRunResult](),
	})
	Register(Op{
		Name: OpAdbDisable, Summary: "Disable device control on an Android Worker and close its open streams; it can only be enabled on the device.",
		Scope: ScopeWorker, Actions: operate, Mutates: true, Unreleased: true,
		Args: reflect.TypeFor[AdbDisableArgs](), Result: reflect.TypeFor[AdbDisableResult](),
	})
}
