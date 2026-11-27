package proto

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"strings"
	"time"
)

// File transfer wire contract (files.v1, BRD minion-file-transfer v2).
//
// The control ops (file_list, file_pull, file_push, file_receipt,
// file_transfers_list) live in the ops catalog (internal/ops/files.go). The
// bytes move only on data legs: one yamux stream of kind StreamFiles per leg,
// carrying exactly one HTTP/1.1 request and response (RFC 9112) for
// FilesLegPath(transfer id): GET when the far end sends (pull), PUT when it
// receives (push). This file holds the names both ends of a data leg must
// agree on: the stream kind, the path, the header and trailer names, the
// content types and encodings, the leg token, the reserved names, the digest
// field syntax and the limits. Nothing here opens a stream or a file.

// StreamFiles is the stream kind of a data leg ("fx"). It is reserved here and
// not yet accepted by ParseStreamKind: the transport accepts it only on a
// connection whose peer announced FeatureFilesV1 (an older peer resets an
// unknown kind).
const StreamFiles StreamKind = "fx"

// FilesLegPathPrefix and FilesLegPathSuffix frame the path of a data leg:
// /files/v1/transfers/<transfer_id>/content.
const (
	FilesLegPathPrefix = "/files/v1/transfers/"
	FilesLegPathSuffix = "/content"
)

// FilesLegPath returns the request path of the data leg of a transfer.
func FilesLegPath(transferID string) string {
	return FilesLegPathPrefix + transferID + FilesLegPathSuffix
}

// ParseFilesLegPath returns the transfer id of a data-leg path. ok is false for
// any other path, or when the id part is empty or contains a slash. The caller
// still checks that the id is a UUIDv7 and names a prepared transfer.
func ParseFilesLegPath(p string) (transferID string, ok bool) {
	rest, found := strings.CutPrefix(p, FilesLegPathPrefix)
	if !found {
		return "", false
	}
	id, found := strings.CutSuffix(rest, FilesLegPathSuffix)
	if !found || id == "" || strings.ContainsAny(id, "/?#%") {
		return "", false
	}
	return id, true
}

// Header and trailer names of a data leg. The leg token is never put in a URL
// or a log.
const (
	// HeaderLegToken carries the single-use leg token of the leg (request).
	HeaderLegToken = "Minion-Leg-Token"
	// HeaderManifestDigest is the trailer with the SHA-256 of the canonical
	// manifest bytes (RFC 8785), in digest field syntax (FormatDigestField).
	HeaderManifestDigest = "Minion-Manifest-Digest"
	// HeaderContentDigest is the RFC 9530 trailer over the transferred content.
	HeaderContentDigest = "Content-Digest"
	// HeaderReprDigest is the RFC 9530 trailer over the whole single file.
	HeaderReprDigest = "Repr-Digest"
)

// Content types of a data-leg body.
const (
	ContentTypeFile    = "application/octet-stream" // a single regular file, raw bytes
	ContentTypeTar     = "application/x-tar"        // a folder, restricted pax profile
	ContentTypeReceipt = "application/json"         // the receipt of a PUT (201)
)

// Content codings of a data-leg body (Accept-Encoding / Content-Encoding). The
// Hub relays them and never decodes.
const (
	EncodingZstd     = "zstd"     // default between Minion ends (RFC 8878)
	EncodingGzip     = "gzip"     // web folder downloads (RFC 1952)
	EncodingIdentity = "identity" // already compressed content, resumable single files
)

// ValidEncoding reports whether e is one of the three codings.
func ValidEncoding(e string) bool {
	switch e {
	case EncodingZstd, EncodingGzip, EncodingIdentity:
		return true
	}
	return false
}

// Reserved names.
const (
	// ManifestName is the last entry of a folder archive. It is never written
	// to disk by Minion and is refused as user content.
	ManifestName = ".minion/manifest.json"
	// StagingPrefix starts the private staging name of a received file:
	// StagingPrefix + "<transfer_id>-<n>". A path element with this prefix is
	// refused on the wire.
	StagingPrefix = ".minion-part-"
)

// LegTokenBytes is the size of a leg token (256 bits). Its text form is
// unpadded base64url, LegTokenLen characters.
const (
	LegTokenBytes = 32
	LegTokenLen   = 43
)

// NewLegToken returns a fresh random leg token.
func NewLegToken() (string, error) {
	b := make([]byte, LegTokenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// ValidLegToken reports whether s has the form of a leg token.
func ValidLegToken(s string) bool {
	if len(s) != LegTokenLen {
		return false
	}
	b, err := base64.RawURLEncoding.Strict().DecodeString(s)
	return err == nil && len(b) == LegTokenBytes
}

// LegTokenEqual compares two leg tokens in constant time. Two malformed tokens
// are never equal.
func LegTokenEqual(a, b string) bool {
	if !ValidLegToken(a) || !ValidLegToken(b) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// FormatDigestField writes a SHA-256 digest in the field syntax of RFC 9530
// (an RFC 8941 dictionary member with a byte sequence): "sha-256=:<base64>:".
// It is used for Content-Digest, Repr-Digest and Minion-Manifest-Digest.
func FormatDigestField(sum []byte) string {
	return "sha-256=:" + base64.StdEncoding.EncodeToString(sum) + ":"
}

// ParseDigestField reads the sha-256 member of a digest field. Other members
// (another algorithm) are ignored; ok is false when there is no well-formed
// sha-256 member of 32 bytes.
func ParseDigestField(v string) (sum []byte, ok bool) {
	for _, m := range strings.Split(v, ",") {
		m = strings.TrimSpace(m)
		val, found := strings.CutPrefix(m, "sha-256=:")
		if !found || !strings.HasSuffix(val, ":") {
			continue
		}
		b, err := base64.StdEncoding.Strict().DecodeString(strings.TrimSuffix(val, ":"))
		if err != nil || len(b) != 32 {
			return nil, false
		}
		return b, true
	}
	return nil, false
}

// ValidManifestDigest reports whether s is the form of manifest_digest in
// results, receipts and audit: 64 lower-case hex digits (SHA-256).
func ValidManifestDigest(s string) bool {
	if len(s) != 64 || strings.ToLower(s) != s {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

// ManifestEntry is one element of the manifest of a transfer: a JSON array of
// these objects in walk order (byte order of names per directory), serialized
// with RFC 8785 (JCS). SHA256 is lower-case hex of the file content; a
// directory has Size 0 and SHA256 "". The manifest digest is the SHA-256 of
// the canonical bytes.
type ManifestEntry struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
	Type   string `json:"type"` // FileTypeFile or FileTypeDir
}

// Entry types of a listing and a manifest. Only FileTypeFile and FileTypeDir
// are ever transferred; the others appear in file_list with a skipped reason.
const (
	FileTypeFile    = "file"
	FileTypeDir     = "dir"
	FileTypeSymlink = "symlink"
	FileTypeOther   = "other" // device, FIFO, socket or anything else
)

// Skip reasons of a listing entry or a folder walk.
const (
	SkipSymlink   = "symlink"   // a symbolic link is never followed
	SkipHardlink  = "hardlink"  // a regular file with more than one link
	SkipSpecial   = "special"   // device, FIFO, socket
	SkipMount     = "mount"     // another file system or a bind mount
	SkipSensitive = "sensitive" // pull-sensitive name or directory
	SkipProtected = "protected" // push-protected name or directory
	SkipName      = "name"      // a name that is not a valid wire path element
)

// Limits of files.v1 (BRD NFR-6, FR-5, FR-12, FR-13). They are the defaults;
// the Hub and the Worker may configure lower values, and the lower one wins.
const (
	// MaxTransferBytes is the default logical byte limit of one transfer (Q7).
	MaxTransferBytes int64 = 4 << 30
	// MaxTransferEntries caps the entries (files and directories) of one transfer.
	MaxTransferEntries = 100000
	// MaxPathBytes, MaxNameBytes and MaxPathDepth bound a wire path.
	MaxPathBytes = 4096
	MaxNameBytes = 255
	MaxPathDepth = 32
	// MaxEntryHeaderBytes caps the tar header plus pax records of one entry.
	MaxEntryHeaderBytes = 4 << 10
	// MaxManifestBytes caps the canonical manifest.
	MaxManifestBytes = 16 << 20
	// WireSlackBytes and WireRatioPermille bound compressed wire bytes:
	// logical x 1.05 + 16 MiB (see MaxWireBytes).
	WireSlackBytes    int64 = 16 << 20
	WireRatioPermille int64 = 1050
	// ZstdMaxWindow and ZstdMaxMemory bound the zstd decoder
	// (WithDecoderMaxWindow, WithDecoderMaxMemory); Android and armv7 use
	// ZstdMaxMemorySmall. ZstdMaxEncoderWindow bounds the encoder.
	ZstdMaxWindow        = 8 << 20
	ZstdMaxEncoderWindow = 8 << 20
	ZstdMaxMemory        = 64 << 20
	ZstdMaxMemorySmall   = 32 << 20
	// MaxFileControlResult bounds the encoded result of every file control op
	// and of a PUT receipt, below the 64 KiB master result limit and the frame
	// limit.
	MaxFileControlResult = 48 << 10
	// FileListDefaultLimit and FileListMaxLimit bound the entries of one
	// file_list page.
	FileListDefaultLimit = 200
	FileListMaxLimit     = 500
	// MaxFileListCursor bounds the opaque cursor of file_list.
	MaxFileListCursor = 1024
	// MaxResultEntries is the number of entries named in a final result or
	// receipt; the rest are counted.
	MaxResultEntries = 200
	// MaxRecordedEntries is the number of entries the Hub stores per transfer.
	MaxRecordedEntries = 1000
	// MaxLegHeaderBytes bounds the request and response headers of a leg.
	MaxLegHeaderBytes = 16 << 10
	// LegChunkBytes is the largest write of a data-leg writer (pacer unit).
	LegChunkBytes = 64 << 10
	// MaxActiveLegsPerConn is the number of active data legs per Worker
	// connection.
	MaxActiveLegsPerConn = 2
	// Concurrency and admission rate of transfers.
	MaxTransfersPerWorker        = 2
	MaxTransfersPerUser          = 4
	MaxTransfersPerHub           = 32
	MaxAdmissionsPerWorkerMinute = 20
	// MinFreeBytes and MinFreePermille: a receiver refuses at admission when
	// less than 1 GiB or 5 % of the file system would remain.
	MinFreeBytes    int64 = 1 << 30
	MinFreePermille int64 = 50
	// FileTransferRetentionDays is the default retention of transfer records.
	FileTransferRetentionDays = 90
)

// Durations of files.v1.
const (
	// LegOpenWindow is the time from admission to the opening of the data leg
	// (expires_at).
	LegOpenWindow = 60 * time.Second
	// LegIdleTimeout ends a leg without body bytes for this long.
	LegIdleTimeout = 60 * time.Second
	// LegReadHeaderTimeout bounds the reading of a leg's request headers.
	LegReadHeaderTimeout = 10 * time.Second
	// RedecideInterval is the period of the Hub's re-decision of both ends
	// during a transfer.
	RedecideInterval = 15 * time.Second
	// StableFileAge: a file whose ctime and mtime are at least this old at
	// admission is stable and gets a strong ETag (resume).
	StableFileAge = 2 * time.Second
	// ReceiptOutboxTTL is how long a receiving daemon resends an
	// unacknowledged receipt.
	ReceiptOutboxTTL = 7 * 24 * time.Hour
	// StaleStagingAge: staged files older than this are removed at start-up.
	StaleStagingAge = 24 * time.Hour
	// ReconcileInterval is the period of the Hub's reconciliation of
	// non-terminal transfer records.
	ReconcileInterval = 10 * time.Minute
)

// MaxWireBytes is the bound of compressed wire bytes for a transfer of logical
// bytes: logical x 1.05 + 16 MiB.
func MaxWireBytes(logical int64) int64 {
	if logical < 0 {
		logical = 0
	}
	return logical/1000*WireRatioPermille + (logical%1000)*WireRatioPermille/1000 + WireSlackBytes
}

// MaxDecodedBytes is the bound of decoded archive bytes, headers included:
// logical bytes plus MaxEntryHeaderBytes per entry.
func MaxDecodedBytes(logical int64, entries int64) int64 {
	if logical < 0 {
		logical = 0
	}
	if entries < 0 {
		entries = 0
	}
	return logical + entries*MaxEntryHeaderBytes
}
