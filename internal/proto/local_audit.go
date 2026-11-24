package proto

// OpLocalAuditV1 is the private call op with which a Worker that is connected
// to a Hub reports a write that a local caller (the CLI, a master session or
// MCP on the Worker host, through the local control socket) made on one of its
// own sessions, so that the Hub audit trail shows it too. The report names the
// operation, the session and the outcome; it never carries message text,
// keys or terminal content.
const OpLocalAuditV1 = "local_audit_v1"

// LocalAuditArgs are the arguments of [OpLocalAuditV1].
type LocalAuditArgs struct {
	// Op is the Worker operation that ran (send_text, send_keys, answer, ...).
	Op string `json:"op"`
	// Session is the target session on the reporting Worker; may be empty.
	Session string `json:"session,omitempty"`
	// Outcome is the outcome of the write (delivered, refused, error, ...).
	Outcome string `json:"outcome"`
	// Reason is the reason code of a refusal or failure; may be empty.
	Reason string `json:"reason,omitempty"`
	// RequestID is the request id of the write (UUIDv7); may be empty.
	RequestID string `json:"request_id,omitempty"`
	// SourceSession is the session of the calling terminal on the Worker (a
	// master or agent session), when it could be told; empty otherwise.
	SourceSession string `json:"source_session,omitempty"`
}
