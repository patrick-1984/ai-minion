package proto

import (
	"net/http"
)

// Code is a snake_case error or reason code. Codes are declared only in this
// file (AD-2); later stories append a constant and a row to codeTable. The
// same codes appear in wire "error" frames and in AD-18 API error envelopes.
type Code string

// The codes. Keep one line per code, in the order of codeTable.
const (
	CodeUnsupportedOp           Code = "unsupported_op"
	CodeProtoIncompatible       Code = "proto_incompatible"
	CodeFrameTooLarge           Code = "frame_too_large"
	CodeNotEnrolled             Code = "not_enrolled"
	CodeRevoked                 Code = "revoked"
	CodeUnauthenticated         Code = "unauthenticated"
	CodeForbidden               Code = "forbidden"
	CodeNotFound                Code = "not_found"
	CodeConflict                Code = "conflict"
	CodeRateLimited             Code = "rate_limited"
	CodeInvalidArgument         Code = "invalid_argument"
	CodeInviteInvalid           Code = "invite_invalid"
	CodeInviteExpired           Code = "invite_expired"
	CodeInviteExhausted         Code = "invite_exhausted"
	CodePairingCodeInvalid      Code = "pairing_code_invalid"
	CodePairingIDCollision      Code = "pairing_id_collision"
	CodePairingAttemptsExceeded Code = "pairing_attempts_exceeded"
	CodeAlreadyPaired           Code = "already_paired"
	CodeWorkerOffline           Code = "worker_offline"
	CodeTargetNotFound          Code = "target_not_found"
	CodePaneChanged             Code = "pane_changed"
	CodeBusy                    Code = "busy"
	CodeShellPane               Code = "shell_pane"
	CodeDialogOpen              Code = "dialog_open"
	CodePreviewOnly             Code = "preview_only"
	CodeNotShared               Code = "not_shared"
	CodeMessageTooLarge         Code = "message_too_large"
	CodePipeRateLimited         Code = "pipe_rate_limited"
	CodePipeBudgetExceeded      Code = "pipe_budget_exceeded"
	CodeSchemaTooNew            Code = "schema_too_new"
	CodeUntrustedPeer           Code = "untrusted_peer"
	CodeInternal                Code = "internal"
	CodeMethodNotAllowed        Code = "method_not_allowed"
	CodeNoDialog                Code = "no_dialog"
	CodeDialogChanged           Code = "dialog_changed"
	CodeConnectionLost          Code = "connection_lost"
	CodeViewIncomplete          Code = "view_incomplete"
	CodePipeOriginRefused       Code = "pipe_origin_refused"
	CodeWorkerPending           Code = "worker_pending"
	CodeHubCertExpired          Code = "hub_cert_expired"
	CodeReauthRequired          Code = "reauth_required"
	CodeReauthUnavailable       Code = "reauth_unavailable"
	CodeTLSRequired             Code = "tls_required"
	CodePassphraseInvalid       Code = "passphrase_invalid"
)

type codeInfo struct {
	code   Code
	status int
	desc   string
}

// codeTable is the single registry: code, HTTP status (AD-18) and a one-line
// English description. Append new codes at the end.
var codeTable = []codeInfo{
	{CodeUnsupportedOp, 501, "The Worker does not implement the requested operation."},
	{CodeProtoIncompatible, 426, "No protocol version is supported by both peers."},
	{CodeFrameTooLarge, 413, "A frame exceeds the size limit for its channel."},
	{CodeNotEnrolled, 403, "The Worker is not enrolled with this Hub."},
	{CodeRevoked, 403, "The identity or credential was revoked."},
	{CodeUnauthenticated, 401, "The caller is not authenticated."},
	{CodeForbidden, 403, "The caller is authenticated but not allowed to do this."},
	{CodeNotFound, 404, "The requested resource does not exist."},
	{CodeConflict, 409, "The request conflicts with the current state."},
	{CodeRateLimited, 429, "Too many requests; retry later."},
	{CodeInvalidArgument, 400, "An argument is missing, malformed or out of range."},
	{CodeInviteInvalid, 400, "The invitation key is unknown or malformed."},
	{CodeInviteExpired, 410, "The invitation key has expired."},
	{CodeInviteExhausted, 410, "The invitation key has no uses left."},
	{CodePairingCodeInvalid, 400, "The pairing code is wrong or malformed."},
	{CodePairingIDCollision, 409, "The pairing identifier is already in use."},
	{CodePairingAttemptsExceeded, 429, "Too many failed pairing attempts; the code is no longer valid."},
	{CodeAlreadyPaired, 409, "The Worker is already paired with a Hub."},
	{CodeWorkerOffline, 503, "The Worker is not connected to the Hub."},
	{CodeTargetNotFound, 404, "The target Worker or session does not exist."},
	{CodePaneChanged, 409, "The terminal pane changed since the caller last looked."},
	{CodeBusy, 409, "The agent session is busy and cannot take input now."},
	{CodeShellPane, 409, "The target pane runs a plain shell, not an agent."},
	{CodeDialogOpen, 409, "A dialog is open in the target session."},
	{CodePreviewOnly, 403, "The session is shared as a preview only."},
	{CodeNotShared, 403, "The session is not shared with the Hub."},
	{CodeMessageTooLarge, 413, "The message exceeds the maximum size."},
	{CodePipeRateLimited, 429, "Too many pipe messages in a short time."},
	{CodePipeBudgetExceeded, 429, "The daily pipe message budget is used up."},
	{CodeSchemaTooNew, 409, "The stored data was written by a newer version."},
	{CodeUntrustedPeer, 403, "The peer is not trusted for this action."},
	{CodeInternal, 500, "An unexpected internal error occurred."},
	{CodeMethodNotAllowed, 405, "The HTTP method is not allowed for this route."},
	{CodeNoDialog, 409, "No dialog is open in the target session."},
	{CodeDialogChanged, 409, "The dialog changed since the caller reviewed it; nothing was sent."},
	{CodeConnectionLost, 502, "The connection ended before the result arrived; the outcome is unknown."},
	{CodeViewIncomplete, 409, "The visible screen could not be read completely; nothing was sent."},
	{CodePipeOriginRefused, 403, "The receiving Worker does not accept messages from this origin (missing or malformed origin, or pipe_accept_from)."},
	{CodeWorkerPending, 403, "The Worker is waiting for approval by its owner or an admin."},
	{CodeHubCertExpired, 503, "The Hub certificate has expired; the Hub owner must re-issue it."},
	{CodeReauthRequired, 403, "The operation needs a sign-in checked in the last five minutes; sign in again."},
	{CodeReauthUnavailable, 403, "The sign-in method cannot prove a fresh authentication; the operation is refused."},
	{CodeTLSRequired, 403, "The request did not arrive over HTTPS, so a passphrase is not accepted."},
	{CodePassphraseInvalid, 403, "The passphrase was not accepted."},
}

var codeIndex = func() map[Code]codeInfo {
	m := make(map[Code]codeInfo, len(codeTable))
	for _, ci := range codeTable {
		m[ci.code] = ci
	}
	return m
}()

// Codes returns every declared code in declaration order.
func Codes() []Code {
	out := make([]Code, len(codeTable))
	for i, ci := range codeTable {
		out[i] = ci.code
	}
	return out
}

// Known reports whether c is a declared code.
func (c Code) Known() bool { _, ok := codeIndex[c]; return ok }

// String returns the code as a string.
func (c Code) String() string { return string(c) }

// Description returns the one-line English description of c, or "" for an
// unknown code.
func (c Code) Description() string { return codeIndex[c].desc }

// HTTPStatus maps a code to the one HTTP status used for it in AD-18 error
// envelopes. An unknown code maps to 500.
func HTTPStatus(c Code) int {
	if ci, ok := codeIndex[c]; ok {
		return ci.status
	}
	return http.StatusInternalServerError
}
