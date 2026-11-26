// Package devctl is the Go side of on-device adb ("Device control") of the
// Android Worker (docs/minion-android-adb/bridge-protocol.md).
//
// The adb client itself lives in the Minion app process (the Kotlin
// AdbBridge): it holds the key, pairs, dials adbd over TLS on loopback and
// checks the endpoint. The Worker reaches it over a local Unix socket with the
// small framed protocol of this file, decides the policy (only on Android, only
// when the app reports the feature enabled, only on an unshared Worker), maps
// the caller to its session and writes the audit. It never sees a pairing
// code, a key or a TLS secret, and it never records a command, its output, a
// code, a key or a token.
//
// The feature ships compiled in but off: the app reports feature_enabled only
// in a build made with -PminionDeviceControl=true, and the ops are Unreleased
// in the catalog until the device gate (DESIGN section 9) passes.
package devctl

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/patrick-1984/ai-minion/internal/proto"
)

// ProtocolVersion is the version of the bridge protocol this Worker speaks.
const ProtocolVersion = 1

// FrameLimit bounds one frame body on the bridge socket in both directions.
// It is the frame limit of the local control socket, so the Kotlin side reads
// both with the same code.
const FrameLimit = proto.StreamFrameLimit

// MaxChunk is the largest raw data chunk the bridge sends in one stdout or
// stderr frame (base64 inflates it by a third, well inside FrameLimit).
const MaxChunk = 256 << 10

// SocketName is the file name of the bridge socket, next to worker.sock in the
// Worker runtime directory (r/minion/adb-bridge.sock in the app).
const SocketName = "adb-bridge.sock"

// Frame types. The Worker sends exactly one request frame per connection; the
// bridge answers with zero or more data frames and exactly one final frame
// (status, exit, ok or error), then closes. The Worker closing the connection
// cancels the operation: the bridge closes the adb stream at once.
const (
	TypeRequest = "adb.request"
	TypeStatus  = "adb.status"
	TypeStdout  = "adb.stdout"
	TypeStderr  = "adb.stderr"
	TypeExit    = "adb.exit"
	TypeOK      = "adb.ok"
	TypeError   = "adb.error"
)

// Request ops: the four operation classes of DESIGN 4.6 plus disable. The
// bridge has no generic "open any service" entry.
const (
	OpStatus    = "status"
	OpShell     = "shell"
	OpScreencap = "screencap"
	OpInput     = "input"
	OpDisable   = "disable"
)

// Error codes of an adb.error frame. Each maps to one honest text (Reason).
const (
	ErrNotAvailable       = "not_available"        // build without the feature flag, or API too old
	ErrDisabled           = "disabled"             // the owner has not enabled it, or it was disabled
	ErrNotPaired          = "not_paired"           // no pairing, or Android ended the authorization
	ErrNeedsWiFi          = "needs_wifi"           // Wireless debugging is off (no Wi-Fi)
	ErrEndpointUnverified = "endpoint_unverified"  // the provenance check failed (DESIGN 4.4)
	ErrTimeout            = "timeout"              // the command ran past its timeout
	ErrBadToken           = "bad_token"            // the bridge token did not match
	ErrBadRequest         = "bad_request"          // a request the bridge does not accept
	ErrBusy               = "busy"                 // too many streams
	ErrShared             = "shared"               // Worker side only: the Worker is shared
	ErrInternal           = "internal"             // anything else
	ErrBridgeDown         = "bridge_unavailable"   // Worker side only: no bridge socket
	ErrUnsupportedOS      = "unsupported_platform" // Worker side only: not Android
)

// Request is the one frame the Worker sends.
type Request struct {
	Type    string `json:"type"`
	Version int    `json:"version"`
	// Token is the per-start bridge token the app handed the Worker on its
	// stdin. It prevents accidental use by other same-uid code; it is not a
	// security boundary (DESIGN 5).
	Token string `json:"token"`
	Op    string `json:"op"`
	// Command is the shell command line of shell and input, built and quoted
	// by the Worker (see [ShellCommand], [InputCommand]). Empty otherwise.
	Command string `json:"command,omitempty"`
	// TimeoutMS bounds shell, screencap and input; the bridge ends the stream
	// and answers timeout when it passes.
	TimeoutMS int64 `json:"timeout_ms,omitempty"`
	// Reason is the disable reason class (app, hub, share, local); never text
	// of a user.
	Reason string `json:"reason,omitempty"`
}

// Status is the adb.status frame and the device_control block of the Worker
// status (without the Worker-side fields the Service adds).
type Status struct {
	Type string `json:"type,omitempty"`
	// FeatureEnabled is the build flag of the app (-PminionDeviceControl).
	FeatureEnabled bool `json:"feature_enabled"`
	// Enabled is the owner's on-device choice.
	Enabled bool `json:"enabled"`
	// State is OFF, UNPAIRED, PAIRED, READY, WAITING_WIFI or
	// ENDPOINT_UNVERIFIED (DESIGN 4.3, 4.4).
	State string `json:"state"`
	// Port is the adbd TLS connection port on 127.0.0.1, 0 when none.
	Port int `json:"port,omitempty"`
	// KeyStorage is "android-keystore (TEE|STRONGBOX|SOFTWARE)" or "app-file".
	KeyStorage    string `json:"key_storage,omitempty"`
	ActiveStreams int    `json:"active_streams"`
	// LastOK and LastError: RFC 3339 time of the last successful operation, and
	// the class of the last failure (an error code above), never output.
	LastOK    string `json:"last_ok,omitempty"`
	LastError string `json:"last_error,omitempty"`
}

// Bridge states.
const (
	StateOff                = "OFF"
	StateUnpaired           = "UNPAIRED"
	StatePaired             = "PAIRED"
	StateReady              = "READY"
	StateWaitingWiFi        = "WAITING_WIFI"
	StateEndpointUnverified = "ENDPOINT_UNVERIFIED"
)

// Data is an adb.stdout or adb.stderr frame. encoding/json writes Data as
// base64.
type Data struct {
	Type string `json:"type"`
	Data []byte `json:"data"`
}

// Exit is the adb.exit frame: the remote exit code of shell, screencap or
// input.
type Exit struct {
	Type string `json:"type"`
	Code int    `json:"code"`
}

// ErrorFrame is the adb.error frame.
type ErrorFrame struct {
	Type    string `json:"type"`
	Code    string `json:"code"`
	Message string `json:"message,omitempty"`
}

// Error is a refusal or failure with one of the codes above.
type Error struct {
	Code string
	// Message is the bridge's text, if any; Reason is used when it is empty.
	Message string
}

func (e *Error) Error() string {
	if e.Message != "" {
		return e.Message
	}
	return Reason(e.Code)
}

// Reason is the honest text of an error code (DESIGN 4.6). It never holds
// a command, output or secret.
func Reason(code string) string {
	switch code {
	case ErrNotAvailable:
		return "device control is not available in this build"
	case ErrDisabled:
		return "device control is disabled; enable it in the Minion app on the device"
	case ErrNotPaired:
		return "not paired: authorization ended in Android; pair again in the Minion app"
	case ErrNeedsWiFi:
		return "needs Wi-Fi: Wireless debugging is off (Android turns it off without Wi-Fi)"
	case ErrEndpointUnverified:
		return "endpoint unverified: the adb endpoint did not prove it is the device's adbd; nothing was sent"
	case ErrTimeout:
		return "timeout"
	case ErrBadToken:
		return "the device control bridge refused this Worker (token mismatch); restart the Minion app"
	case ErrBadRequest:
		return "the device control bridge refused the request"
	case ErrBusy:
		return "device control is busy; retry shortly"
	case ErrShared:
		return "Worker is shared: device control needs a dedicated, unshared Worker (worker config share none)"
	case ErrBridgeDown:
		return "device control is not running: the Minion app bridge is not reachable"
	case ErrUnsupportedOS:
		return "device control is not available on this system (Android only)"
	}
	return "device control failed"
}

// CodeOf returns the code of a *Error in err, or "".
func CodeOf(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}

// writeFrame writes one frame.
func writeFrame(w io.Writer, v any) error { return proto.WriteFrame(w, FrameLimit, v) }

// readFrame reads one frame and returns its type and body.
func readFrame(r io.Reader) (string, json.RawMessage, error) {
	body, err := proto.ReadFrame(r, FrameLimit)
	if err != nil {
		return "", nil, err
	}
	typ, err := proto.FrameType(body)
	if err != nil {
		return "", nil, fmt.Errorf("devctl: bridge frame: %w", err)
	}
	return typ, body, nil
}

// ReadRequest reads the request frame (for bridges written in Go: the fake of
// the tests).
func ReadRequest(r io.Reader) (Request, error) {
	typ, body, err := readFrame(r)
	if err != nil {
		return Request{}, err
	}
	if typ != TypeRequest {
		return Request{}, fmt.Errorf("devctl: expected %s, got %q", TypeRequest, typ)
	}
	var rq Request
	if err := json.Unmarshal(body, &rq); err != nil {
		return Request{}, fmt.Errorf("devctl: request: %w", err)
	}
	return rq, nil
}

// WriteFrame writes one bridge frame (for the fake bridge of the tests).
func WriteFrame(w io.Writer, v any) error { return writeFrame(w, v) }
