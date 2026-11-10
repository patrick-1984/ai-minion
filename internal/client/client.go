package client

import (
	"context"
	"encoding/json"
	"errors"
)

// Backend runs ops of the catalog (internal/ops) on a Worker or a Hub. The
// op names, the arguments and the results are the ones of the catalog, in
// their JSON wire form.
//
// A refusal by the other side comes back as a *proto.Error (use
// errors.As or proto.CodeOf to see the code). A write whose connection broke
// after the request was sent comes back as [ErrConnectionLost]: the caller
// must treat the outcome as unknown (AD-15) and never retry blindly; a retry
// with the same request id is safe because the Worker answers a duplicate
// from its cache.
type Backend interface {
	// Call runs op with args and returns its JSON result. args is encoded as
	// JSON (a struct, a map, a json.RawMessage or nil). For a streaming op
	// Call discards the progress elements and returns the final result.
	Call(ctx context.Context, op string, args any) (json.RawMessage, error)

	// Stream runs op like Call and hands every `op.progress` element to
	// onProgress, in order, as it arrives. This is how pipe_wait and other
	// streaming ops deliver their elements. An error from onProgress stops the
	// stream and is returned. A nil onProgress discards the elements. The
	// final result is returned like Call does.
	Stream(ctx context.Context, op string, args any, onProgress func(json.RawMessage) error) (json.RawMessage, error)
}

var (
	// ErrWorkerNotRunning means nothing listens on the local control socket:
	// no Worker runs for this user.
	ErrWorkerNotRunning = errors.New("no Minion Worker is running for this user; start one with `minion worker run`")

	// ErrPermissionDenied means the local control socket exists but is not
	// private to this OS user: it or its directory belongs to another user, or
	// the directory is open to others. The client never connects to it.
	ErrPermissionDenied = errors.New("permission denied on the Minion control socket; it does not belong exclusively to this user")

	// ErrConnectionLost means the connection ended before the final result
	// arrived. For a write the outcome is unknown.
	ErrConnectionLost = errors.New("the connection ended before the result arrived; the outcome is unknown")

	// ErrWorkerTooOld means the Worker daemon on the local socket predates a
	// safeguard that the request relies on (a reviewed answer, an atomic submit,
	// a verbatim text) and refused the request before anything happened: the
	// client is newer than the running Worker (an upgrade that did not restart
	// it). It is a definite "not sent" and may be repeated after the Worker was
	// updated and restarted.
	ErrWorkerTooOld = errors.New("the running Worker is too old for this request")

	// ErrProtocol means the other side sent something that is not an op frame.
	ErrProtocol = errors.New("unexpected frame from the Worker")
)
