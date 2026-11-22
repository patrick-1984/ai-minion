package proto

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
)

// MaxReadbackDocBytes bounds the usage document of one read-back frame. It is
// the size limit of the document itself (minion.usage-meters/1).
const MaxReadbackDocBytes = 32 << 10

// ReadbackApply is the Hub's private control frame that hands a Worker the
// usage document it publishes in its local presence directory (A79/A80). It is
// the first frame of an op stream the Hub opened on the authenticated link; it
// is not an op: it has no entry in the op catalog, no actions, no origin and no
// caller, so no MCP tool, REST route, local socket call, Worker call or master
// call can produce it.
//
// Every field is a binding that the Worker compares with the connection the
// frame arrived on, immediately before it replaces the local document. A frame
// that does not match is refused and nothing is published.
type ReadbackApply struct {
	Type string `json:"type"`
	// Revision orders the frames of one connection: it starts at 1 on every
	// connection and grows with every new document. A repeat of the revision
	// last applied with the same document (a lost acknowledgement) is
	// acknowledged again; a lower one, or the same one with another document,
	// is refused as stale.
	Revision uint64 `json:"revision"`
	// OwnerID is the stable Hub id of the Worker's owner the document was
	// built for; WorkerID is the Worker it is addressed to; HubKey is the
	// fingerprint of the Hub root key the Worker pinned.
	OwnerID  string `json:"owner_id"`
	WorkerID string `json:"worker_id"`
	HubKey   string `json:"hub_key"`
	// Doc is the minion.usage-meters/1 document, at most MaxReadbackDocBytes.
	Doc json.RawMessage `json:"doc,omitempty"`
	// Clear asks the Worker to remove the published document instead of
	// replacing it: the Hub sends it when the recipient is no longer entitled
	// to it (its owner was deactivated or lost access, or it is not approved).
	// A clear carries no Doc; it is bound and ordered like any other frame.
	Clear bool `json:"clear,omitempty"`
}

// UnmarshalJSON reads a frame; a JSON null document (what an older Hub wrote for
// a clear) is no document.
func (f *ReadbackApply) UnmarshalJSON(b []byte) error {
	type plain ReadbackApply
	if err := json.Unmarshal(b, (*plain)(f)); err != nil {
		return err
	}
	if string(bytes.TrimSpace(f.Doc)) == "null" {
		f.Doc = nil
	}
	return nil
}

// Check validates the bounds that do not depend on the receiving connection.
func (f ReadbackApply) Check() *Error {
	switch {
	case f.Type != TypeReadbackApply:
		return NewError(CodeInvalidArgument, "not a readback.apply frame")
	case f.Revision == 0:
		return NewError(CodeInvalidArgument, "the read-back revision must be positive")
	case f.OwnerID == "" || f.WorkerID == "" || f.HubKey == "":
		return NewError(CodeInvalidArgument, "the read-back frame lacks its owner, Worker or Hub key binding")
	case f.Clear && len(f.Doc) != 0:
		return NewError(CodeInvalidArgument, "a read-back clear carries no document")
	case f.Clear:
		return nil
	case len(f.Doc) == 0 || len(f.Doc) > MaxReadbackDocBytes:
		return NewError(CodeInvalidArgument, fmt.Sprintf("the read-back document must be 1 to %d bytes", MaxReadbackDocBytes))
	}
	return nil
}

// ReadbackAck is the result of an applied ReadbackApply: the revision that is
// now published locally. The Worker sends it only after the local publication
// succeeded.
type ReadbackAck struct {
	Revision uint64 `json:"revision"`
}

// Reasons in the details of a refused ReadbackApply (ErrorFrame.Details
// {"reason": ...}); the Hub reads them to decide whether to retry.
const (
	// ReadbackStale: the revision is not newer than the one applied.
	ReadbackStale = "stale_revision"
	// ReadbackFenced: a binding (owner, Worker, Hub key, enrollment or link)
	// does not match the connection; the frame was built for another one.
	ReadbackFenced = "binding_mismatch"
)

// ReadbackRefusal returns the error that refuses a ReadbackApply, carrying one
// of the Readback* reasons in its details.
func ReadbackRefusal(code Code, reason, msg string) *Error {
	e := NewError(code, msg)
	e.Details, _ = json.Marshal(map[string]string{"reason": reason})
	return e
}

// ReadbackReason returns the reason of a refused ReadbackApply, "" when err
// carries none.
func ReadbackReason(err error) string {
	var e *Error
	if !errors.As(err, &e) || len(e.Details) == 0 {
		return ""
	}
	var d struct {
		Reason string `json:"reason"`
	}
	if json.Unmarshal(e.Details, &d) != nil {
		return ""
	}
	return d.Reason
}
