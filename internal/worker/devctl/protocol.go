// Package devctl is the Go side of on-device adb ("Device control") of the
// Android Worker (docs/architecture/device-control-bridge.md).
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
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// ProtocolVersion is the version of the bridge protocol this Worker speaks.
// It travels in the request; a bridge ignores it today.
const ProtocolVersion = 1

// Frame layout (the app's BridgeFrames): a uint32 big-endian length n
// (1..MaxPayload+1), one channel byte, then n-1 payload bytes. Channel 0
// carries one UTF-8 JSON object with a "type"; channels 1 and 2 carry raw
// stdout and stderr bytes of a running operation.
const (
	ChannelControl = 0
	ChannelStdout  = 1
	ChannelStderr  = 2
	// MaxPayload bounds the payload of one frame in both directions.
	MaxPayload = 1 << 20
	// MaxRequest bounds the request frame (the bridge reads at most this).
	MaxRequest = 256 << 10
)

// SocketName is the file name of the bridge socket, next to worker.sock in the
// Worker runtime directory (r/minion/adb-bridge.sock in the app).
const SocketName = "adb-bridge.sock"

// TokenEnvPrefix starts the one line the app writes to the Worker's stdin at
// launch: "MINION_ADB_BRIDGE_TOKEN=<64 hex>\n".
const TokenEnvPrefix = "MINION_ADB_BRIDGE_TOKEN="

// Control frame types. The Worker sends exactly one request per connection
// (no type member needed; the bridge reads op). The bridge answers status with
// one status frame, disable with one ok frame, and shell, screencap and input
// with one started frame, any number of stdout and stderr frames and exactly
// one exit frame; any of them can instead end with one error frame. The
// Worker closing the connection cancels the operation: the bridge closes the
// adb stream at once.
const (
	TypeStatus  = "status"
	TypeOK      = "ok"
	TypeStarted = "started"
	TypeExit    = "exit"
	TypeError   = "error"
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

// Error codes of an error frame. Each maps to one honest text (Reason).
const (
	ErrNotAvailable       = "not_available"        // build without the feature flag, or API too old
	ErrDisabled           = "disabled"             // the owner has not enabled it, or it was disabled
	ErrNotPaired          = "not_paired"           // no legacy adb and no pairing, or Android ended the authorization
	ErrNeedsAuth          = "needs_authorization"  // legacy adb: "Allow debugging?" waits on the device
	ErrNeedsWiFi          = "needs_wifi"           // Wireless debugging is off (no Wi-Fi)
	ErrAmbiguous          = "ambiguous"            // several local adb endpoints, none chosen by the owner
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
	Version int `json:"version"`
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

// Status is the status frame and the device_control block of the Worker
// status (without the Worker-side fields the Service adds).
type Status struct {
	Type string `json:"type,omitempty"`
	// FeatureEnabled is the build flag of the app (-PminionDeviceControl).
	FeatureEnabled bool `json:"feature_enabled"`
	// Enabled is the owner's on-device choice.
	Enabled bool `json:"enabled"`
	// State is UNAVAILABLE, OFF, UNPAIRED, PAIRED (connects on demand: paired
	// or legacy adb seen, not connected now), AWAITING_AUTHORIZATION (legacy
	// adb: the device shows "Allow debugging?"), READY (connected and
	// verified), WAITING_WIFI or ENDPOINT_UNVERIFIED (DESIGN 4.3, 4.4).
	State string `json:"state"`
	// Port is the adbd port on 127.0.0.1 (5555 for legacy adb, the Wireless
	// debugging port for TLS), 0 when not connected.
	Port int `json:"port,omitempty"`
	// Transport is TransportLegacyLoopback or TransportTLSLoopback while
	// connected (legacy also while the device asks), "" otherwise.
	Transport string `json:"transport,omitempty"`
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
	StateUnavailable        = "UNAVAILABLE"
	StateOff                = "OFF"
	StateUnpaired           = "UNPAIRED"
	StatePaired             = "PAIRED"
	StateAwaitingAuth       = "AWAITING_AUTHORIZATION"
	StateReady              = "READY"
	StateWaitingWiFi        = "WAITING_WIFI"
	StateEndpointUnverified = "ENDPOINT_UNVERIFIED"
)

// Exit is the exit frame: the remote exit code of shell, screencap or
// input.
type Exit struct {
	Type string `json:"type"`
	Code int    `json:"code"`
}

// ErrorFrame is the error frame.
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
		return "not paired: no adb on 127.0.0.1:5555 and Wireless debugging is not paired (or Android ended the authorization); pair in the Minion app"
	case ErrNeedsAuth:
		return "first connection: confirm \"Allow debugging?\" on the device and tick \"Always allow\", then retry"
	case ErrNeedsWiFi:
		return "needs Wi-Fi: Wireless debugging is off (Android turns it off without Wi-Fi)"
	case ErrAmbiguous:
		return "several adb endpoints on this device: enter the connection port in the Minion app"
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

// WriteFrame writes one frame on channel ch. A control payload is v encoded
// as JSON; a data payload is v as []byte.
func WriteFrame(w io.Writer, ch byte, v any) error {
	var payload []byte
	if ch == ChannelControl {
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		payload = b
	} else {
		b, ok := v.([]byte)
		if !ok {
			return fmt.Errorf("devctl: data frame needs bytes")
		}
		payload = b
	}
	if len(payload) > MaxPayload {
		return fmt.Errorf("devctl: frame of %d bytes is too large", len(payload))
	}
	buf := make([]byte, 5+len(payload))
	binary.BigEndian.PutUint32(buf, uint32(len(payload)+1))
	buf[4] = ch
	copy(buf[5:], payload)
	_, err := w.Write(buf)
	return err
}

// ReadFrame reads one frame of at most limit payload bytes.
func ReadFrame(r io.Reader, limit int) (byte, []byte, error) {
	var hdr [5]byte
	if _, err := io.ReadFull(r, hdr[:4]); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(hdr[:4])
	if n < 1 || uint64(n-1) > uint64(limit) {
		return 0, nil, fmt.Errorf("devctl: bad frame size %d", n)
	}
	if _, err := io.ReadFull(r, hdr[4:]); err != nil {
		return 0, nil, unexpected(err)
	}
	if hdr[4] > ChannelStderr {
		return 0, nil, fmt.Errorf("devctl: bad channel %d", hdr[4])
	}
	payload := make([]byte, n-1)
	if _, err := io.ReadFull(r, payload); err != nil {
		return 0, nil, unexpected(err)
	}
	return hdr[4], payload, nil
}

func unexpected(err error) error {
	if errors.Is(err, io.EOF) {
		return io.ErrUnexpectedEOF
	}
	return err
}

// ControlType returns the type member of a control payload.
func ControlType(payload []byte) (string, error) {
	var t struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(payload, &t); err != nil || t.Type == "" {
		return "", fmt.Errorf("devctl: control frame without type")
	}
	return t.Type, nil
}

// ReadRequest reads the request frame (for bridges written in Go: the fake of
// the tests).
func ReadRequest(r io.Reader) (Request, error) {
	ch, body, err := ReadFrame(r, MaxRequest)
	if err != nil {
		return Request{}, err
	}
	if ch != ChannelControl {
		return Request{}, fmt.Errorf("devctl: the request is not a control frame")
	}
	var rq Request
	if err := json.Unmarshal(body, &rq); err != nil {
		return Request{}, fmt.Errorf("devctl: request: %w", err)
	}
	return rq, nil
}
