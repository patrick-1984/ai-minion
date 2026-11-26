package ops

import (
	"encoding/json"
	"io/fs"
	"reflect"
	"strings"
	"unicode/utf8"

	"github.com/patrick-1984/ai-minion/internal/proto"
)

// File transfer contract (files.v1, BRD minion-file-transfer v2, WP1).
//
// Five control ops; none streams and none is a Write (the bytes move on data
// legs, see internal/proto/files.go):
//
//   - file_list (Worker, read, file_read): one directory of the target root,
//     paginated by limit and by the encoded size of the result
//     (proto.MaxFileControlResult), with an opaque next_cursor.
//   - file_pull (Worker, Mutates, file_read on the target): admission of a
//     pull. The result is a FileTransferResult at stage "admitted": transfer
//     id, roots, kind, size, etag, encodings, limits, expires_at and the
//     caller's single-use leg token. Never content, never the manifest.
//   - file_push (Worker, Mutates, file_write on the target): admission of a
//     push, same result.
//   - file_receipt (Hub, Mutates): the receiver's terminal receipt of a pull,
//     accepted only from the receiving leg's identity with its leg token.
//   - file_transfers_list (Hub, read, file_read): the owner-scoped history.
//
// The same FileTransferResult is the final answer of the caller side (the
// local daemon for MCP and session callers, the REST CLI) at stage
// "completed", with the outcome and totals and without the leg token.
//
// Members the Hub sets on the request it routes to the target Worker (transfer
// id, the target's leg token, the Hub's limits) and the members the caller side
// declares for a push (kind, size, entries) are hidden members: no producer
// offers them as a flag or a tool argument.
//
// The ops are Unreleased until the executors, the Hub service and the caller
// side exist: they are in the catalog, the OpenAPI document and on the wire,
// but not MCP tools and not listed in CLI help. Until then a Worker does not
// announce them (no handler), so the Hub answers unsupported_op without
// opening a stream, and the Hub has no handler for the two Hub ops, so its
// REST dispatcher answers unsupported_op.

// Names of the file transfer operations.
const (
	OpFileList          = "file_list"
	OpFilePull          = "file_pull"
	OpFilePush          = "file_push"
	OpFileReceipt       = "file_receipt"
	OpFileTransfersList = "file_transfers_list"
)

// FileOpNames lists the five file transfer ops.
var FileOpNames = []string{OpFileList, OpFilePull, OpFilePush, OpFileReceipt, OpFileTransfersList}

// IsFileOp reports whether op is one of the Worker-scope file ops that need
// proto.FeatureFilesV1 on the Worker.
func IsFileOp(op string) bool {
	switch op {
	case OpFileList, OpFilePull, OpFilePush:
		return true
	}
	return false
}

// Directions of a transfer, named from the initiating caller.
const (
	FileDirectionPull = "pull" // the target sends, the caller receives
	FileDirectionPush = "push" // the caller sends, the target receives
)

// Effects of a transfer on one end: files leave it, or files arrive on it.
const (
	FileEffectSend    = "send"
	FileEffectReceive = "receive"
)

// FileEffects returns the effect of a transfer of direction on the target and
// on the caller. ok is false for an unknown direction.
func FileEffects(direction string) (target, caller string, ok bool) {
	switch direction {
	case FileDirectionPull:
		return FileEffectSend, FileEffectReceive, true
	case FileDirectionPush:
		return FileEffectReceive, FileEffectSend, true
	}
	return "", "", false
}

// Kinds of a transfer.
const (
	FileKindFile = "file" // one regular file, raw bytes
	FileKindDir  = "dir"  // a folder, restricted pax tar with the manifest last
)

// Stages of a FileTransferResult.
const (
	FileStageAdmitted  = "admitted"  // admission answered; the data leg is next
	FileStageCompleted = "completed" // the caller side finished; outcome set
)

// States of a transfer record (file_transfers_list). The terminal states are
// delivered, partial, failed_not_sent, served, unknown and denied; unknown can
// still move to its real outcome when a late receipt arrives.
const (
	FileStateAdmitted      = "admitted"
	FileStatePrepared      = "prepared"
	FileStateDelivered     = "delivered"
	FileStatePartial       = "partial"
	FileStateFailedNotSent = "failed_not_sent"
	FileStateServed        = "served" // a web download: bytes served, not committed
	FileStateUnknown       = "unknown"
	FileStateDenied        = "denied" // refused at admission; no bytes moved
)

// ValidFileState reports whether s is a transfer record state.
func ValidFileState(s string) bool {
	switch s {
	case FileStateAdmitted, FileStatePrepared, FileStateDelivered, FileStatePartial,
		FileStateFailedNotSent, FileStateServed, FileStateUnknown, FileStateDenied:
		return true
	}
	return false
}

// Values of the Worker's local file_transfer switch and of the Hub's
// per-Worker override.
const (
	FileModeOn          = "on"
	FileModeSendOnly    = "send_only"
	FileModeReceiveOnly = "receive_only"
	FileModeOff         = "off"
)

// Values of the Hub-wide file_transfer policy (pull and push are named from
// the initiating caller). FileModeOn and FileModeOff are shared.
const (
	FilePolicyPullOnly   = "pull_only"
	FilePolicyPushOnly   = "push_only"
	FilePolicyAdminsOnly = "admins_only"
)

// ValidWorkerFileMode reports whether m is a value of the Worker switch or the
// Hub per-Worker override.
func ValidWorkerFileMode(m string) bool {
	switch m {
	case FileModeOn, FileModeSendOnly, FileModeReceiveOnly, FileModeOff:
		return true
	}
	return false
}

// ValidHubFilePolicy reports whether p is a value of the Hub-wide policy.
func ValidHubFilePolicy(p string) bool {
	switch p {
	case FileModeOn, FilePolicyPullOnly, FilePolicyPushOnly, FileModeOff, FilePolicyAdminsOnly:
		return true
	}
	return false
}

// WorkerFileModeAllows reports whether the Worker switch (or per-Worker
// override) mode allows effect on that Worker. An unknown mode or effect
// allows nothing.
func WorkerFileModeAllows(mode, effect string) bool {
	switch effect {
	case FileEffectSend:
		return mode == FileModeOn || mode == FileModeSendOnly
	case FileEffectReceive:
		return mode == FileModeOn || mode == FileModeReceiveOnly
	}
	return false
}

// HubFilePolicyAllows reports whether the Hub-wide policy allows a transfer of
// direction by a caller; admin tells whether the caller is an administrator.
// An unknown policy or direction allows nothing.
func HubFilePolicyAllows(policy, direction string, admin bool) bool {
	if direction != FileDirectionPull && direction != FileDirectionPush {
		return false
	}
	switch policy {
	case FileModeOn:
		return true
	case FilePolicyPullOnly:
		return direction == FileDirectionPull
	case FilePolicyPushOnly:
		return direction == FileDirectionPush
	case FilePolicyAdminsOnly:
		return admin
	}
	return false
}

// CheckFilePath checks a wire path (BRD FR-5): relative, slash separated,
// valid UTF-8 without control characters or backslashes, no empty, "." or ".."
// element, no leading or trailing slash, no element with the staging prefix,
// not the reserved manifest name, at most proto.MaxPathBytes bytes,
// proto.MaxNameBytes per element and proto.MaxPathDepth elements. The empty
// path names the root and is accepted only when root is true. A refusal is a
// *proto.Error with proto.CodePathOutsideRoot whose message never contains
// the path. NFC normalization is checked by the transfer engine
// (internal/filexfer), which owns the Unicode tables.
func CheckFilePath(p string, root bool) error {
	refuse := func(why string) error {
		return proto.NewError(proto.CodePathOutsideRoot, "the path "+why)
	}
	if p == "" {
		if root {
			return nil
		}
		return refuse("is empty")
	}
	if len(p) > proto.MaxPathBytes {
		return refuse("is too long")
	}
	if !utf8.ValidString(p) {
		return refuse("is not valid UTF-8")
	}
	for _, r := range p {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) {
			return refuse("contains a control character")
		}
		if r == '\\' {
			return refuse("contains a backslash")
		}
	}
	if !fs.ValidPath(p) || p == "." {
		return refuse("must be relative, without empty, \".\" or \"..\" elements")
	}
	elems := strings.Split(p, "/")
	if len(elems) > proto.MaxPathDepth {
		return refuse("is too deep")
	}
	for _, e := range elems {
		if len(e) > proto.MaxNameBytes {
			return refuse("has an element that is too long")
		}
		if strings.HasPrefix(e, proto.StagingPrefix) {
			return refuse("names a reserved staging file")
		}
	}
	if p == proto.ManifestName {
		return refuse("is the reserved manifest name")
	}
	return nil
}

// validCursor reports whether c is an opaque cursor: unpadded base64url text of
// at most proto.MaxFileListCursor bytes.
func validCursor(c string) bool {
	if len(c) > proto.MaxFileListCursor {
		return false
	}
	for i := 0; i < len(c); i++ {
		b := c[i]
		if (b < 'A' || b > 'Z') && (b < 'a' || b > 'z') && (b < '0' || b > '9') && b != '-' && b != '_' {
			return false
		}
	}
	return true
}

// FileListArgs are the arguments of file_list.
type FileListArgs struct {
	Target Target `json:"target"`
	// Path is the directory to list, relative to the root of the session's
	// folder; empty lists the root.
	Path string `json:"path,omitempty"`
	// Cursor is the next_cursor of the previous page; empty starts at the
	// first entry.
	Cursor string `json:"cursor,omitempty"`
	// Limit caps the entries of the page; 0 means proto.FileListDefaultLimit.
	Limit int `json:"limit,omitempty"`
}

// Validate checks the target, the cursor form and the limit. The path is
// checked by the Worker with CheckFilePath, so that a hostile path is refused
// with path_outside_root on every surface.
func (a FileListArgs) Validate() error {
	if err := a.Target.Validate(); err != nil {
		return err
	}
	if !validCursor(a.Cursor) {
		return invalid("cursor", "is not a cursor of file_list")
	}
	if a.Limit < 0 || a.Limit > proto.FileListMaxLimit {
		return invalid("limit", "must be between 0 and %d", proto.FileListMaxLimit)
	}
	return nil
}

// EffectiveLimit returns Limit or proto.FileListDefaultLimit when it is 0.
func (a FileListArgs) EffectiveLimit() int {
	if a.Limit == 0 {
		return proto.FileListDefaultLimit
	}
	return a.Limit
}

// FileEntry is one entry of a listing.
type FileEntry struct {
	Name string `json:"name"`
	// Type is proto.FileTypeFile, FileTypeDir, FileTypeSymlink or FileTypeOther.
	Type string `json:"type"`
	// Size is the size in bytes of a regular file, 0 otherwise.
	Size int64 `json:"size"`
	// MTime is the modification time in seconds since the Unix epoch.
	MTime int64 `json:"mtime"`
	// SkippedReason (proto.Skip*) is set for an entry that cannot be
	// transferred.
	SkippedReason string `json:"skipped_reason,omitempty"`
}

// FileListResult is one page of a listing, sorted by the byte order of the
// names. Its encoding is at most proto.MaxFileControlResult bytes.
type FileListResult struct {
	// Root is the display root of the session's folder ("~/projects/app";
	// never an absolute path under home; "Minion files/..." on Android).
	Root string `json:"root"`
	// Path is the listed directory, relative to the root ("" for the root).
	Path    string      `json:"path"`
	Entries []FileEntry `json:"entries"`
	// NextCursor continues the listing; empty on the last page. It is bound
	// to the root's identity and cannot be used on another folder.
	NextCursor string `json:"next_cursor,omitempty"`
}

// MarshalJSON writes entries as an empty array instead of null.
func (r FileListResult) MarshalJSON() ([]byte, error) {
	type plain FileListResult
	if r.Entries == nil {
		r.Entries = []FileEntry{}
	}
	return json.Marshal(plain(r))
}

// FileLegArgs are the hidden members the Hub sets on the file_pull or
// file_push request it routes to the target Worker. A caller never sets them:
// the Hub overwrites them on every routed request.
type FileLegArgs struct {
	// TransferID is the UUIDv7 of the admitted transfer.
	TransferID string `json:"transfer_id,omitempty"`
	// LegToken is the target leg's single-use token (proto.NewLegToken).
	LegToken string `json:"leg_token,omitempty"`
	// MaxBytes and MaxEntries are the Hub's limits of this transfer; the
	// Worker applies the lower of them and its own (0: the Worker's own).
	MaxBytes   int64 `json:"max_bytes,omitempty"`
	MaxEntries int64 `json:"max_entries,omitempty"`
}

// Validate checks the forms of the Hub members: both or neither of transfer
// id and leg token, and limits that are not negative.
func (a FileLegArgs) Validate() error {
	if (a.TransferID == "") != (a.LegToken == "") {
		return invalid("leg_token", "transfer_id and leg_token go together")
	}
	if a.TransferID != "" && !ValidUUIDv7(a.TransferID) {
		return invalid("transfer_id", "must be a UUIDv7")
	}
	if a.LegToken != "" && !proto.ValidLegToken(a.LegToken) {
		return invalid("leg_token", "is not a leg token")
	}
	if a.MaxBytes < 0 || a.MaxEntries < 0 {
		return invalid("max_bytes", "limits must not be negative")
	}
	return nil
}

// FilePullArgs are the arguments of file_pull: copy Path of the target
// session's folder into the caller's folder.
type FilePullArgs struct {
	Target Target `json:"target"`
	// Path is the file or folder to pull, relative to the target root.
	Path string `json:"path"`
	// To is the destination relative to the caller's root (caller side
	// only); empty means the last element of Path in the caller's root.
	To string `json:"to,omitempty"`
	// Overwrite replaces an existing regular file at the destination;
	// without it an existing name is a conflict.
	Overwrite bool `json:"overwrite,omitempty"`
	FileLegArgs
}

// Validate checks the target and the Hub members. The paths are checked with
// CheckFilePath by the end that resolves them.
func (a FilePullArgs) Validate() error {
	if err := a.Target.Validate(); err != nil {
		return err
	}
	return a.FileLegArgs.Validate()
}

// FilePushArgs are the arguments of file_push: copy Path of the caller's
// folder into the target session's folder.
type FilePushArgs struct {
	Target Target `json:"target"`
	// Path is the local file or folder to push, relative to the caller's
	// root (caller side only).
	Path string `json:"path"`
	// As is the destination relative to the target root; empty means the last
	// element of Path in the target root. The caller side always sets it on
	// the request it sends to the Hub.
	As string `json:"as,omitempty"`
	// Overwrite replaces an existing regular file at the destination.
	Overwrite bool `json:"overwrite,omitempty"`
	// Kind, Size and Entries are declared by the caller side (hidden): the
	// kind of the source, its logical bytes and its entry count, which the
	// target checks against its limits and free space at admission.
	Kind    string `json:"kind,omitempty"`
	Size    int64  `json:"size,omitempty"`
	Entries int64  `json:"entries,omitempty"`
	FileLegArgs
}

// Validate checks the target, the declared kind and sizes and the Hub
// members.
func (a FilePushArgs) Validate() error {
	if err := a.Target.Validate(); err != nil {
		return err
	}
	if a.Kind != "" && a.Kind != FileKindFile && a.Kind != FileKindDir {
		return invalid("kind", "must be file or dir")
	}
	if a.Size < 0 || a.Entries < 0 {
		return invalid("size", "must not be negative")
	}
	return a.FileLegArgs.Validate()
}

// FileTransferLimits are the effective limits of one transfer (the lower of
// the Hub's and the Worker's).
type FileTransferLimits struct {
	MaxBytes     int64 `json:"max_bytes"`
	MaxEntries   int64 `json:"max_entries"`
	MaxDepth     int   `json:"max_depth"`
	MaxPathBytes int   `json:"max_path_bytes"`
}

// DefaultFileTransferLimits returns the files.v1 defaults.
func DefaultFileTransferLimits() FileTransferLimits {
	return FileTransferLimits{
		MaxBytes: proto.MaxTransferBytes, MaxEntries: proto.MaxTransferEntries,
		MaxDepth: proto.MaxPathDepth, MaxPathBytes: proto.MaxPathBytes,
	}
}

// Lower returns the member-wise lower of l and o; a zero member of o leaves
// the member of l.
func (l FileTransferLimits) Lower(o FileTransferLimits) FileTransferLimits {
	low := func(a, b int64) int64 {
		if b > 0 && b < a {
			return b
		}
		return a
	}
	return FileTransferLimits{
		MaxBytes:     low(l.MaxBytes, o.MaxBytes),
		MaxEntries:   low(l.MaxEntries, o.MaxEntries),
		MaxDepth:     int(low(int64(l.MaxDepth), int64(o.MaxDepth))),
		MaxPathBytes: int(low(int64(l.MaxPathBytes), int64(o.MaxPathBytes))),
	}
}

// FileTransferEntry is one file of a result, receipt or record: its path
// relative to the root, size and SHA-256 (lower-case hex), or the reason it
// was skipped.
type FileTransferEntry struct {
	Path          string `json:"path"`
	Size          int64  `json:"size"`
	SHA256        string `json:"sha256,omitempty"`
	SkippedReason string `json:"skipped_reason,omitempty"`
}

// FileTransferResult is the result of file_pull and file_push. At stage
// "admitted" (the Hub's answer) it carries the admission: everything up to
// LegToken. At stage "completed" (the caller side's final answer) it carries
// the outcome and the totals, and LegToken is empty. Its encoding is at most
// proto.MaxFileControlResult bytes.
type FileTransferResult struct {
	Stage      string `json:"stage"`
	TransferID string `json:"transfer_id"`
	Direction  string `json:"direction"`
	// CallerRoot and TargetRoot are display roots (redacted, never an
	// absolute path under home). CallerRoot is empty for a browser.
	CallerRoot string `json:"caller_root,omitempty"`
	TargetRoot string `json:"target_root"`
	Kind       string `json:"kind"`
	// Size is the size of a single file (pull); ETag its strong entity tag
	// when the file is stable (resume with Range and If-Range).
	Size int64  `json:"size,omitempty"`
	ETag string `json:"etag,omitempty"`
	// Encodings are the content codings the far end offers (proto.Encoding*).
	Encodings []string            `json:"encodings,omitempty"`
	Limits    *FileTransferLimits `json:"limits,omitempty"`
	// ExpiresAt is the deadline to open the data leg (proto.LegOpenWindow
	// after admission).
	ExpiresAt *Timestamp `json:"expires_at,omitempty"`
	// LegToken is the caller leg's single-use token: sent only in the
	// Minion-Leg-Token header of the data leg and in file_receipt, never in a
	// URL or a log.
	LegToken string `json:"leg_token,omitempty"`
	// LegPath is the data-leg path (proto.FilesLegPath).
	LegPath string `json:"leg_path,omitempty"`

	// Outcome is set at stage completed: delivered, failed_not_sent, partial
	// or unknown.
	Outcome Outcome `json:"outcome,omitempty"`
	// Reason is the code of a refusal or failure (proto codes), empty when
	// delivered.
	Reason         string `json:"reason,omitempty"`
	Files          int64  `json:"files,omitempty"`
	Bytes          int64  `json:"bytes,omitempty"`
	WireBytes      int64  `json:"wire_bytes,omitempty"`
	ManifestDigest string `json:"manifest_digest,omitempty"`
	// Entries are the first proto.MaxResultEntries entries, published and
	// skipped; EntriesOmitted counts the rest and Skipped all skipped ones.
	Entries        []FileTransferEntry `json:"entries,omitempty"`
	EntriesOmitted int64               `json:"entries_omitted,omitempty"`
	Skipped        int64               `json:"skipped,omitempty"`
}

// FileReceipt is the receiver's terminal statement of a transfer: the JSON
// body of a push's 201 answer, and the content of file_receipt. Its encoding
// is at most proto.MaxFileControlResult bytes.
type FileReceipt struct {
	Outcome Outcome `json:"outcome"`
	// Reason is the code of the failure (a proto code), empty when delivered.
	Reason         string              `json:"reason,omitempty"`
	Files          int64               `json:"files"`
	Bytes          int64               `json:"bytes"`
	ManifestDigest string              `json:"manifest_digest,omitempty"`
	Entries        []FileTransferEntry `json:"entries,omitempty"`
	EntriesOmitted int64               `json:"entries_omitted,omitempty"`
}

// Validate checks the outcome, the reason code, the digest form and the
// bounds.
func (r FileReceipt) Validate() error {
	if !r.Outcome.Valid() {
		return invalid("outcome", "must be delivered, failed_not_sent, partial or unknown")
	}
	if r.Outcome == OutcomeDelivered {
		if r.Reason != "" {
			return invalid("reason", "a delivered transfer has no reason")
		}
		if r.ManifestDigest == "" {
			return invalid("manifest_digest", "a delivered transfer names its manifest digest")
		}
	} else if r.Reason != "" && !proto.Code(r.Reason).Known() {
		return invalid("reason", "must be an error code")
	}
	if r.ManifestDigest != "" && !proto.ValidManifestDigest(r.ManifestDigest) {
		return invalid("manifest_digest", "must be 64 lower-case hex digits")
	}
	if r.Files < 0 || r.Bytes < 0 || r.EntriesOmitted < 0 {
		return invalid("files", "counts must not be negative")
	}
	if len(r.Entries) > proto.MaxResultEntries {
		return invalid("entries", "at most %d entries", proto.MaxResultEntries)
	}
	return nil
}

// FileReceiptArgs are the arguments of file_receipt: the receipt of the
// receiving end of a pull, bound to the transfer by the caller leg's token.
type FileReceiptArgs struct {
	TransferID string `json:"transfer_id"`
	LegToken   string `json:"leg_token"`
	FileReceipt
}

// Validate checks the transfer id, the token form and the receipt.
func (a FileReceiptArgs) Validate() error {
	if !ValidUUIDv7(a.TransferID) {
		return invalid("transfer_id", "must be a UUIDv7")
	}
	if !proto.ValidLegToken(a.LegToken) {
		return invalid("leg_token", "is not a leg token")
	}
	return a.FileReceipt.Validate()
}

// FileReceiptResult acknowledges a receipt: the record's state after it. A
// repeated receipt (outbox resend) is acknowledged again.
type FileReceiptResult struct {
	TransferID string `json:"transfer_id"`
	State      string `json:"state"`
}

// Limits of file_transfers_list.
const (
	FileTransfersDefaultLimit = 50
	FileTransfersMaxLimit     = 200
)

// FileTransfersListArgs are the arguments of file_transfers_list.
type FileTransfersListArgs struct {
	// WorkerID limits the list to transfers that touch that Worker.
	WorkerID string `json:"worker_id,omitempty"`
	// TransferID selects one transfer.
	TransferID string `json:"transfer_id,omitempty"`
	// Cursor is the next_cursor of the previous page.
	Cursor string `json:"cursor,omitempty"`
	// Limit caps the records of the page; 0 means FileTransfersDefaultLimit.
	Limit int `json:"limit,omitempty"`
}

// Validate checks the filters, the cursor form and the limit.
func (a FileTransfersListArgs) Validate() error {
	if a.WorkerID != "" && !ValidUUIDv7(a.WorkerID) {
		return invalid("worker_id", "must be a UUIDv7")
	}
	if a.TransferID != "" && !ValidUUIDv7(a.TransferID) {
		return invalid("transfer_id", "must be a UUIDv7")
	}
	if !validCursor(a.Cursor) {
		return invalid("cursor", "is not a cursor of file_transfers_list")
	}
	if a.Limit < 0 || a.Limit > FileTransfersMaxLimit {
		return invalid("limit", "must be between 0 and %d", FileTransfersMaxLimit)
	}
	return nil
}

// EffectiveLimit returns Limit or FileTransfersDefaultLimit when it is 0.
func (a FileTransfersListArgs) EffectiveLimit() int {
	if a.Limit == 0 {
		return FileTransfersDefaultLimit
	}
	return a.Limit
}

// FileTransferRecord is one transfer of the history. It names no absolute
// path, no token and no content.
type FileTransferRecord struct {
	TransferID string `json:"transfer_id"`
	Direction  string `json:"direction"`
	// State is one of the FileState* values; Reason the code of a refusal or
	// failure.
	State  string `json:"state"`
	Reason string `json:"reason,omitempty"`
	// Principal is who initiated the transfer (display form); ActingAs the
	// account acted as, if any.
	Principal string `json:"principal"`
	ActingAs  string `json:"acting_as,omitempty"`
	// SourceWorkerID and SourceSession are the caller's end; empty for a REST
	// or browser caller.
	SourceWorkerID string `json:"source_worker_id,omitempty"`
	SourceSession  string `json:"source_session,omitempty"`
	TargetWorkerID string `json:"target_worker_id"`
	TargetSession  string `json:"target_session"`
	// GrantID or AcceptanceID names the grant or master acceptance row that
	// authorized a session caller.
	GrantID      string `json:"grant_id,omitempty"`
	AcceptanceID string `json:"acceptance_id,omitempty"`
	CallerRoot   string `json:"caller_root,omitempty"`
	TargetRoot   string `json:"target_root,omitempty"`
	// Path is the requested path relative to the target root.
	Path           string              `json:"path"`
	Kind           string              `json:"kind,omitempty"`
	Files          int64               `json:"files"`
	Bytes          int64               `json:"bytes"`
	WireBytes      int64               `json:"wire_bytes"`
	ManifestDigest string              `json:"manifest_digest,omitempty"`
	Protected      string              `json:"protected,omitempty"` // "off" when the receiver relaxed its lists
	Entries        []FileTransferEntry `json:"entries,omitempty"`
	EntriesOmitted int64               `json:"entries_omitted,omitempty"`
	CreatedAt      Timestamp           `json:"created_at"`
	EndedAt        *Timestamp          `json:"ended_at,omitempty"`
}

// FileTransfersListResult is one page of the history, newest first.
type FileTransfersListResult struct {
	Transfers  []FileTransferRecord `json:"transfers"`
	NextCursor string               `json:"next_cursor,omitempty"`
}

// MarshalJSON writes transfers as an empty array instead of null.
func (r FileTransfersListResult) MarshalJSON() ([]byte, error) {
	type plain FileTransfersListResult
	if r.Transfers == nil {
		r.Transfers = []FileTransferRecord{}
	}
	return json.Marshal(plain(r))
}

func init() {
	read := MustActionSet(ActionFileRead)
	write := MustActionSet(ActionFileWrite)
	Register(Op{Name: OpFileList, Summary: "List one folder of a session's folder (paginated): names, types, sizes and modification times.",
		Scope: ScopeWorker, Actions: read, Unreleased: true,
		Args: reflect.TypeFor[FileListArgs](), Result: reflect.TypeFor[FileListResult]()})
	Register(Op{Name: OpFilePull, Summary: "Copy a file or folder from a session's folder into the caller's folder.",
		Scope: ScopeWorker, Actions: read, Mutates: true, Unreleased: true,
		Args: reflect.TypeFor[FilePullArgs](), Result: reflect.TypeFor[FileTransferResult]()})
	Register(Op{Name: OpFilePush, Summary: "Copy a file or folder from the caller's folder into a session's folder.",
		Scope: ScopeWorker, Actions: write, Mutates: true, Unreleased: true,
		Args: reflect.TypeFor[FilePushArgs](), Result: reflect.TypeFor[FileTransferResult]()})
	Register(Op{Name: OpFileReceipt, Summary: "Report the final outcome of a received file transfer (receiving end only).",
		Scope: ScopeHub, Actions: MustActionSet(ActionFileRead, ActionFileWrite), Mutates: true, Unreleased: true,
		Args: reflect.TypeFor[FileReceiptArgs](), Result: reflect.TypeFor[FileReceiptResult]()})
	Register(Op{Name: OpFileTransfersList, Summary: "List the file transfers that touch your Workers or that you started (administrators: all).",
		Scope: ScopeHub, Actions: read, Unreleased: true,
		Args: reflect.TypeFor[FileTransfersListArgs](), Result: reflect.TypeFor[FileTransfersListResult]()})
}
