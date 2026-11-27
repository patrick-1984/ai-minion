package agent

// The data-leg HTTP handler of the target end (files.v1, BRD 5.2, 5.5, 5.7,
// FR-9, FR-14, FR-15). One fx stream carries one HTTP/1.1 exchange for
// proto.FilesLegPath(transfer id): GET sends the prepared source (a pull), PUT
// receives into the prepared destination (a push). The leg is bound to its
// prepared transfer by the Minion-Leg-Token header, the method (direction)
// and the connection it arrived on ([Files.claim]).
//
// Sending: a single file is raw bytes with a strong ETag only when it is
// stable, Range and If-Range answered with 206 or 416 (RFC 9110), and the
// trailers Content-Digest (the coded bytes sent), Repr-Digest (the whole file)
// and Minion-Manifest-Digest; a folder is a restricted pax archive with the
// manifest last and the trailers Content-Digest and Minion-Manifest-Digest. A
// file that changes while it is sent ends the leg without trailers, so the
// receiver can verify nothing and publishes nothing.
//
// Receiving: the body is decoded within the limits, staged, checked against
// its trailers and its manifest (FR-9), and published only after the local
// switch and preview_only were read again; the answer is 201 with the JSON
// receipt (ops.FileReceipt), or the code's status with a failed_not_sent
// receipt.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/patrick-1984/ai-minion/internal/filexfer"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/transport"
)

// ServeLeg serves one data leg (an fx stream the Hub opened) of the link l.
// It returns when the exchange has ended.
func (f *Files) ServeLeg(ctx context.Context, l *Link, st *transport.Stream) {
	c := transport.NewLegConn(st)
	srv := transport.NewLegServer(f.LegHandler(l))
	if err := transport.ServeLeg(ctx, srv, c); err != nil && !errors.Is(err, context.Canceled) {
		f.log.Debug("data leg ended", "error", err.Error())
	}
}

// LegHandler returns the HTTP handler of the data legs that arrive on conn
// (the *Link of the connection; tests pass any comparable value).
func (f *Files) LegHandler(conn any) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, ok := proto.ParseFilesLegPath(r.URL.Path)
		if !ok || !ops.ValidUUIDv7(id) {
			writeLegError(w, proto.CodeForbidden, "no transfer is prepared for this data leg")
			return
		}
		var direction string
		switch r.Method {
		case http.MethodGet:
			direction = ops.FileDirectionPull
		case http.MethodPut:
			direction = ops.FileDirectionPush
		default:
			writeLegError(w, proto.CodeForbidden, "a data leg is a GET or a PUT")
			return
		}
		t, refused := f.claim(id, r.Header.Get(proto.HeaderLegToken), direction, conn)
		if refused != nil {
			f.log.Info("data leg refused", "transfer", id, "code", string(refused.code))
			writeLegError(w, refused.code, refused.msg)
			return
		}
		defer f.release(t)
		if direction == ops.FileDirectionPull {
			f.serveSend(w, r, t)
			return
		}
		f.serveReceive(w, r, t)
	})
}

// writeLegError answers a leg with the status of code and the JSON error.
func writeLegError(w http.ResponseWriter, code proto.Code, msg string) {
	b, _ := json.Marshal(proto.NewError(code, msg))
	w.Header().Set("Content-Type", proto.ContentTypeReceipt)
	w.Header().Set("Content-Length", strconv.Itoa(len(b)))
	w.WriteHeader(proto.HTTPStatus(code))
	_, _ = w.Write(b)
}

// abortLeg ends a leg whose response has started without its trailers: the
// HTTP server closes the connection, which resets the stream, and the
// receiver sees a truncated body.
func abortLeg() { panic(http.ErrAbortHandler) }

// ---- sending ----

func (f *Files) serveSend(w http.ResponseWriter, r *http.Request, t *fxTransfer) {
	if err := f.requireEffect(ops.FileEffectSend); err != nil {
		writeAPIError(w, err)
		return
	}
	if t.kind == ops.FileKindDir {
		f.sendDir(w, r, t)
		return
	}
	f.sendFile(w, r, t)
}

// writeAPIError answers a leg with the code of an executor error.
func writeAPIError(w http.ResponseWriter, err error) {
	pe := apiProto(err)
	writeLegError(w, pe.Code, pe.Message)
}

// apiProto returns the wire error of an executor or engine error.
func apiProto(err error) *proto.Error {
	if ae, ok := AsAPIError(err); ok {
		return proto.NewError(ae.Code, ae.Message)
	}
	return filexfer.ProtoError(err)
}

func (f *Files) sendDir(w http.ResponseWriter, r *http.Request, t *fxTransfer) {
	enc := filexfer.Negotiate(r.Header.Get("Accept-Encoding"), proto.EncodingZstd, proto.EncodingGzip)
	h := w.Header()
	h.Set("Content-Type", proto.ContentTypeTar)
	if enc != proto.EncodingIdentity {
		h.Set("Content-Encoding", enc)
	}
	h.Set("Trailer", proto.HeaderContentDigest+", "+proto.HeaderManifestDigest)
	w.WriteHeader(http.StatusOK)
	ww := filexfer.NewWireWriter(w)
	cw, err := filexfer.NewEncoder(ww, enc)
	if err != nil {
		abortLeg()
	}
	res, err := filexfer.PackDir(r.Context(), cw, t.root, t.path,
		filexfer.WalkOptions{Limits: filexfer.LimitsFrom(t.limits), Filter: filexfer.PullFilter(t.root, nil)})
	if err != nil {
		f.log.Info("folder send failed", "transfer", t.id, "code", string(filexfer.ProtoError(err).Code))
		abortLeg()
	}
	if err := cw.Close(); err != nil {
		abortLeg()
	}
	digest, err := res.Manifest.Digest()
	if err != nil || f.requireEffect(ops.FileEffectSend) != nil {
		abortLeg()
	}
	h.Set(proto.HeaderContentDigest, proto.FormatDigestField(ww.Sum()))
	h.Set(proto.HeaderManifestDigest, proto.FormatDigestField(digest[:]))
}

func (f *Files) sendFile(w http.ResponseWriter, r *http.Request, t *fxTransfer) {
	src, size := t.src, t.size
	h := w.Header()
	start, end, partial := int64(0), size-1, false
	if t.etag != "" {
		h.Set("ETag", t.etag)
		h.Set("Accept-Ranges", "bytes")
		if rh := r.Header.Get("Range"); rh != "" {
			if ir := r.Header.Get("If-Range"); ir == "" || ir == t.etag {
				s, e, ok, unsat := parseByteRange(rh, size)
				switch {
				case unsat:
					h.Set("Content-Range", "bytes */"+strconv.FormatInt(size, 10))
					w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
					return
				case ok:
					start, end, partial = s, e, true
				}
			}
		}
	} else {
		h.Set("Accept-Ranges", "none")
	}
	enc := proto.EncodingIdentity
	if t.etag == "" && size > 0 {
		head := make([]byte, min(size, filexfer.ProbeBytes))
		n, _ := src.File.ReadAt(head, 0)
		if !filexfer.LooksCompressed(t.srcName, head[:n]) {
			enc = filexfer.Negotiate(r.Header.Get("Accept-Encoding"), proto.EncodingZstd)
		}
	}
	repr := sha256.New()
	if partial && start > 0 {
		if _, err := io.Copy(repr, io.NewSectionReader(src.File, 0, start)); err != nil {
			writeLegError(w, proto.CodeInternal, "the file cannot be read")
			return
		}
	}
	h.Set("Content-Type", proto.ContentTypeFile)
	if enc != proto.EncodingIdentity {
		h.Set("Content-Encoding", enc)
	}
	h.Set("Trailer", proto.HeaderContentDigest+", "+proto.HeaderReprDigest+", "+proto.HeaderManifestDigest)
	status := http.StatusOK
	if partial {
		h.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, size))
		status = http.StatusPartialContent
	}
	w.WriteHeader(status)
	ww := filexfer.NewWireWriter(w)
	cw, err := filexfer.NewEncoder(ww, enc)
	if err != nil {
		abortLeg()
	}
	n := end - start + 1
	if size == 0 {
		n = 0
	}
	if _, err := filexfer.CopyFile(io.MultiWriter(cw, repr), io.NewSectionReader(src.File, start, n), n); err != nil {
		abortLeg()
	}
	if err := cw.Close(); err != nil {
		abortLeg()
	}
	if end+1 < size {
		if _, err := io.Copy(repr, io.NewSectionReader(src.File, end+1, size-end-1)); err != nil {
			abortLeg()
		}
	}
	// The stability rule: a file that changed while it was sent gets no
	// trailers, so the receiver can verify nothing.
	if changed, err := src.Changed(); err != nil || changed || f.requireEffect(ops.FileEffectSend) != nil {
		abortLeg()
	}
	var sum [32]byte
	copy(sum[:], repr.Sum(nil))
	digest, err := filexfer.SingleFileManifest(t.srcName, size, sum).Digest()
	if err != nil {
		abortLeg()
	}
	h.Set(proto.HeaderContentDigest, proto.FormatDigestField(ww.Sum()))
	h.Set(proto.HeaderReprDigest, proto.FormatDigestField(sum[:]))
	h.Set(proto.HeaderManifestDigest, proto.FormatDigestField(digest[:]))
}

// parseByteRange reads a Range value of one byte range for a file of size
// bytes (RFC 9110 section 14.1.2). ok is false for a form it does not serve
// (several ranges, another unit, a malformed value): the full file is sent
// then. unsat is true for a well-formed range that the file cannot satisfy.
func parseByteRange(v string, size int64) (start, end int64, ok, unsat bool) {
	spec, found := strings.CutPrefix(strings.TrimSpace(v), "bytes=")
	if !found || strings.Contains(spec, ",") {
		return 0, 0, false, false
	}
	a, b, found := strings.Cut(strings.TrimSpace(spec), "-")
	if !found {
		return 0, 0, false, false
	}
	if a == "" {
		n, err := strconv.ParseInt(b, 10, 64)
		if err != nil || n < 0 {
			return 0, 0, false, false
		}
		if n == 0 || size == 0 {
			return 0, 0, false, true
		}
		return max(size-n, 0), size - 1, true, false
	}
	s, err := strconv.ParseInt(a, 10, 64)
	if err != nil || s < 0 {
		return 0, 0, false, false
	}
	e := size - 1
	if b != "" {
		if e, err = strconv.ParseInt(b, 10, 64); err != nil || e < s {
			return 0, 0, false, false
		}
		e = min(e, size-1)
	}
	if s >= size {
		return 0, 0, false, true
	}
	return s, e, true, false
}

// ---- receiving ----

func (f *Files) serveReceive(w http.ResponseWriter, r *http.Request, t *fxTransfer) {
	rc, status := f.receive(r, t)
	b, err := json.Marshal(rc)
	if err != nil {
		writeLegError(w, proto.CodeInternal, "the receipt cannot be encoded")
		return
	}
	w.Header().Set("Content-Type", proto.ContentTypeReceipt)
	w.Header().Set("Content-Length", strconv.Itoa(len(b)))
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

// failedReceipt is the receipt of a push that published nothing.
func failedReceipt(err error) (ops.FileReceipt, int) {
	pe := apiProto(err)
	return ops.FileReceipt{Outcome: ops.OutcomeFailedNotSent, Reason: string(pe.Code)}, proto.HTTPStatus(pe.Code)
}

// receive stages, verifies and publishes the body of a push and returns the
// receipt with its HTTP status.
func (f *Files) receive(r *http.Request, t *fxTransfer) (ops.FileReceipt, int) {
	if err := f.requireEffect(ops.FileEffectReceive); err != nil {
		return failedReceipt(err)
	}
	enc := r.Header.Get("Content-Encoding")
	if enc == "" {
		enc = proto.EncodingIdentity
	}
	if !proto.ValidEncoding(enc) {
		return failedReceipt(apiErr(proto.CodeInvalidArgument, "", "the content coding is not one of files.v1"))
	}
	lim := filexfer.LimitsFrom(t.limits)
	wire := filexfer.NewWireReader(r.Body, lim.MaxWire())
	dec, err := filexfer.NewDecoder(wire, enc, lim)
	if err != nil {
		return failedReceipt(err)
	}
	defer func() { _ = dec.Close() }()
	st, err := filexfer.NewStager(t.root, t.id, t.path, t.kind, filexfer.StageOptions{
		Limits: lim, Overwrite: t.overwrit, Check: filexfer.PushCheck(t.root, nil),
		MinFreeBytes: f.cfg.MinFreeBytes, MinFreePermille: f.cfg.MinFreePermille,
	})
	if err != nil {
		return failedReceipt(err)
	}
	defer func() { _ = st.Abort() }()
	if f.cfg.StateDir != "" {
		rel, dev, ino := st.StagingDir()
		if abs := f.stagingDir(t, rel); abs != "" {
			slog := filexfer.NewStagingLog(f.cfg.StateDir)
			if err := slog.Add(filexfer.StagingRecord{Dir: abs, Dev: dev, Ino: ino, TransferID: t.id, At: f.now()}); err == nil {
				defer func() { _ = slog.Done(t.id) }()
			}
		}
	}

	var (
		m   filexfer.Manifest
		sum [32]byte
	)
	if t.kind == ops.FileKindFile {
		mtime := f.now()
		if lm, err := http.ParseTime(r.Header.Get("Last-Modified")); err == nil {
			mtime = lm
		}
		if sum, err = filexfer.ReceiveFile(dec, st, t.size, mtime); err != nil {
			return failedReceipt(err)
		}
		m = filexfer.SingleFileManifest(t.srcName, t.size, sum)
	} else if m, err = filexfer.Unpack(dec, st); err != nil {
		return failedReceipt(err)
	}
	// The body must end here, with its trailers.
	if n, err := io.Copy(io.Discard, wire); err != nil || n > 0 {
		return failedReceipt(apiErr(proto.CodeIntegrityFailed, "", "the body did not end cleanly"))
	}
	md, ok := proto.ParseDigestField(r.Trailer.Get(proto.HeaderManifestDigest))
	if !ok {
		return failedReceipt(apiErr(proto.CodeIntegrityFailed, "", "the manifest digest trailer is missing"))
	}
	cd, ok := proto.ParseDigestField(r.Trailer.Get(proto.HeaderContentDigest))
	if !ok || !bytes.Equal(cd, wire.Sum()) {
		return failedReceipt(apiErr(proto.CodeIntegrityFailed, "", "the content digest does not match"))
	}
	if t.kind == ops.FileKindFile {
		rd, ok := proto.ParseDigestField(r.Trailer.Get(proto.HeaderReprDigest))
		if !ok || !bytes.Equal(rd, sum[:]) {
			return failedReceipt(apiErr(proto.CodeIntegrityFailed, "", "the file digest does not match"))
		}
	}
	if err := st.Verify(m, md); err != nil {
		return failedReceipt(err)
	}
	// The receiver re-check (BRD 5.6, FR-18): the switch and preview_only as
	// they are now, just before anything becomes visible.
	if err := f.requireEffect(ops.FileEffectReceive); err != nil {
		return failedReceipt(err)
	}
	if r.Context().Err() != nil {
		return failedReceipt(apiErr(proto.CodeConnectionLost, "", "the data leg ended before the publish"))
	}
	var digest [32]byte
	copy(digest[:], md)
	pub, perr := st.Publish()
	rc := ops.FileReceipt{Outcome: ops.OutcomeDelivered, Files: pub.Files, Bytes: pub.Bytes, ManifestDigest: filexfer.DigestHex(digest)}
	if perr != nil {
		if len(pub.Published) == 0 {
			return failedReceipt(perr)
		}
		rc.Outcome, rc.Reason = ops.OutcomePartial, string(apiProto(perr).Code)
	}
	boundEntries(&rc, pub.Published)
	f.log.Info("file transfer received", "transfer", t.id, "outcome", string(rc.Outcome), "files", rc.Files, "bytes", rc.Bytes)
	return rc, http.StatusCreated
}

// boundEntries puts the first published entries in the receipt so that its
// encoding stays within proto.MaxFileControlResult, and counts the rest.
func boundEntries(rc *ops.FileReceipt, all []ops.FileTransferEntry) {
	n := min(len(all), proto.MaxResultEntries)
	for ; n > 0; n /= 2 {
		rc.Entries = all[:n]
		if b, err := json.Marshal(rc); err == nil && len(b) <= proto.MaxFileControlResult {
			break
		}
	}
	if n == 0 {
		rc.Entries = nil
	}
	rc.EntriesOmitted = int64(len(all) - n)
}
