package agent

// The caller end of a file transfer (files.v1, BRD minion-file-transfer v2,
// WP8, FR-4, FR-9, FR-15, FR-16).
//
// A caller asks the Hub to admit a pull or a push of a target session's
// folder and then moves the bytes itself on its own data leg to the Hub: it
// receives (pull) or sends (push) relative to its own pinned root. Two
// callers share the engine here ([CallerPull], [CallerPush]):
//
//   - the local daemon for a session caller (the CLI, `mn` or MCP inside a
//     pane of this Worker): its root is the folder of the calling pane, its
//     admission is a call stream to the Hub and its leg an fx stream on the
//     same connection ([localExecutor.fileCall]);
//   - the REST CLI with an API key (internal/cli): its root is its current
//     folder (filexfer.OpenCwdRoot), its admission the REST op and its leg the
//     Hub's public endpoint.
//
// Everything that can be refused locally is refused before the admission is
// sent: a destination or source path outside the root, the caller's own
// switch, protected destinations and sensitive sources. The receiver
// publishes nothing before the whole transfer is verified (FR-9); a cancelled
// context resets the leg and leaves nothing published. The final result is
// the FileTransferResult at stage completed, without the leg token, within
// proto.MaxFileControlResult bytes. Nothing here logs or returns a path of the
// caller's file system, a token or content.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/patrick-1984/ai-minion/internal/filexfer"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// FileLeg is an open data leg of the caller: an HTTP client whose requests go
// to the Hub's handler of the leg, the URL of the leg and the headers every
// request carries besides the leg token (a REST caller's authorization).
// Close releases it.
type FileLeg struct {
	Client *http.Client
	URL    string
	Header http.Header
	Close  func()
}

// FileProgress is one progress report of a running transfer: the bytes moved
// on the data leg so far and the declared total (0 when unknown).
type FileProgress struct {
	TransferID string `json:"transfer_id"`
	Direction  string `json:"direction"`
	Bytes      int64  `json:"bytes"`
	Total      int64  `json:"total,omitempty"`
}

// FileCaller configures one caller end of a transfer.
type FileCaller struct {
	// Root is the caller's pinned root. The caller owns it.
	Root *filexfer.Root
	// Allow checks the caller end's own switch for an effect
	// (ops.FileEffectSend or ops.FileEffectReceive): files_disabled, or
	// forbidden/preview_only for a receive. It is asked before the admission,
	// before the publish of a pull and before the trailers of a push. Nil
	// allows everything (a REST caller has no Worker switch).
	Allow func(effect string) error
	// Admit sends the admission (file_pull or file_push) to the Hub and
	// returns its result at stage admitted.
	Admit func(ctx context.Context, op string, args any) (ops.FileTransferResult, error)
	// Leg opens the data leg of an admitted transfer.
	Leg func(ctx context.Context, res ops.FileTransferResult) (*FileLeg, error)
	// Receipt sends the receipt of a pull; nil sends none. A failed receipt
	// is the caller's to keep (the outbox) and does not fail the transfer.
	Receipt func(ctx context.Context, args ops.FileReceiptArgs)
	// Progress, when not nil, gets a report at most every progressEvery.
	Progress func(FileProgress)
	// MinFreeBytes and MinFreePermille are the free-space reserve of a pull
	// (filexfer.StageOptions).
	MinFreeBytes    int64
	MinFreePermille int64
}

// progressEvery bounds the rate of progress reports.
const progressEvery = 250 * time.Millisecond

func (c FileCaller) allow(effect string) error {
	if c.Allow == nil {
		return nil
	}
	return c.Allow(effect)
}

// CheckPullLocal checks what a pull can be refused for before anything is
// sent: the path forms, the destination in the caller's root (FR-4), the
// caller's switch and the protected destinations of the caller.
func (c FileCaller) CheckPullLocal(args ops.FilePullArgs) (dest string, err error) {
	if err := filexfer.CheckPath(args.Path, false); err != nil {
		return "", fxErr(err)
	}
	dest = args.To
	if dest == "" {
		dest = filexfer.BaseName(args.Path)
	}
	if err := filexfer.CheckPath(dest, false); err != nil {
		return "", fxErr(err)
	}
	if err := c.allow(ops.FileEffectReceive); err != nil {
		return "", err
	}
	if c.Root == nil {
		return "", apiErr(proto.CodeFilesRootUnknown, "", "the caller has no folder to receive into")
	}
	if err := c.Root.CheckPush(); err != nil {
		return "", fxErr(err)
	}
	return dest, nil
}

// CallerPull runs a pull: admission, the GET data leg, staging, verification,
// publication into the caller's root, the receipt. The error is a refusal or
// failure before anything could be published; once the leg ran, the outcome
// is in the result.
func CallerPull(ctx context.Context, c FileCaller, args ops.FilePullArgs) (ops.FileTransferResult, error) {
	dest, err := c.CheckPullLocal(args)
	if err != nil {
		return ops.FileTransferResult{}, err
	}
	wire := ops.FilePullArgs{Target: args.Target, Path: args.Path, Overwrite: args.Overwrite}
	adm, err := c.Admit(ctx, ops.OpFilePull, wire)
	if err != nil {
		return ops.FileTransferResult{}, err
	}
	if err := checkAdmission(adm, ops.FileDirectionPull); err != nil {
		return ops.FileTransferResult{}, err
	}
	res := completed(adm, c.Root)
	rc, wireBytes, err := c.receive(ctx, adm, args, dest)
	res.WireBytes = wireBytes
	if err != nil {
		rc, _ = failedReceipt(err)
	}
	if c.Receipt != nil {
		// The receipt goes out whatever the context: a cancelled transfer is
		// still reported.
		c.Receipt(context.WithoutCancel(ctx), ops.FileReceiptArgs{TransferID: adm.TransferID, LegToken: adm.LegToken, FileReceipt: rc})
	}
	fromReceipt(&res, rc)
	return res, nil
}

// checkAdmission checks the Hub's admission result.
func checkAdmission(adm ops.FileTransferResult, direction string) error {
	switch {
	case adm.Stage != ops.FileStageAdmitted || adm.Direction != direction:
		return apiErr(proto.CodeInternal, "", "the Hub answered the admission with another result")
	case !ops.ValidUUIDv7(adm.TransferID) || !proto.ValidLegToken(adm.LegToken):
		return apiErr(proto.CodeInternal, "", "the Hub's admission has no usable transfer id or leg token")
	case adm.Kind != ops.FileKindFile && adm.Kind != ops.FileKindDir:
		return apiErr(proto.CodeInternal, "", "the Hub's admission names no kind")
	}
	return nil
}

// completed returns the final result of an admission, without the token.
func completed(adm ops.FileTransferResult, root *filexfer.Root) ops.FileTransferResult {
	res := adm
	res.Stage = ops.FileStageCompleted
	res.LegToken, res.ExpiresAt, res.LegPath, res.Encodings, res.ETag = "", nil, "", nil, ""
	if root != nil && res.CallerRoot == "" {
		res.CallerRoot = root.Display()
	}
	return res
}

// fromReceipt puts the outcome of a receipt in the final result, bounded to
// proto.MaxFileControlResult.
func fromReceipt(res *ops.FileTransferResult, rc ops.FileReceipt) {
	res.Outcome, res.Reason = rc.Outcome, rc.Reason
	res.Files, res.Bytes, res.ManifestDigest = rc.Files, rc.Bytes, rc.ManifestDigest
	res.Entries, res.EntriesOmitted = rc.Entries, rc.EntriesOmitted
	n := len(res.Entries)
	for ; n > 0; n /= 2 {
		res.Entries = rc.Entries[:n]
		if b, err := json.Marshal(res); err == nil && len(b) <= proto.MaxFileControlResult {
			break
		}
	}
	if n == 0 {
		res.Entries = nil
	}
	res.EntriesOmitted = rc.EntriesOmitted + int64(len(rc.Entries)-n)
}

// progressReader counts the bytes read and reports them.
type progressReader struct {
	r     io.Reader
	n     atomic.Int64
	last  time.Time
	every func(n int64)
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	total := p.n.Add(int64(n))
	if p.every != nil && (err != nil || time.Since(p.last) >= progressEvery) {
		p.last = time.Now()
		p.every(total)
	}
	return n, err
}

func (c FileCaller) reporter(adm ops.FileTransferResult) func(int64) {
	if c.Progress == nil {
		return nil
	}
	return func(n int64) {
		c.Progress(FileProgress{TransferID: adm.TransferID, Direction: adm.Direction, Bytes: n, Total: adm.Size})
	}
}

// legRequest builds a request of the leg.
func legRequest(ctx context.Context, leg *FileLeg, method string, tok string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, leg.URL, body)
	if err != nil {
		return nil, internalErr("build the data leg request", err)
	}
	for k, vs := range leg.Header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	req.Header.Set(proto.HeaderLegToken, tok)
	return req, nil
}

// legRefused maps a refused leg answer (a JSON proto error body) to an error.
func legRefused(resp *http.Response) error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<10))
	var pe proto.Error
	if json.Unmarshal(b, &pe) == nil && pe.Code != "" && pe.Code.Known() {
		return apiErr(pe.Code, "", "the data leg was refused: "+pe.Message)
	}
	return apiErr(proto.CodeInternal, "", "the data leg was refused with HTTP status "+http.StatusText(resp.StatusCode))
}

// openLeg opens the leg and maps a failure.
func (c FileCaller) openLeg(ctx context.Context, adm ops.FileTransferResult) (*FileLeg, error) {
	leg, err := c.Leg(ctx, adm)
	if err != nil {
		if _, ok := AsAPIError(err); ok {
			return nil, err
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, apiErr(proto.CodeConnectionLost, "", "the transfer was cancelled")
		}
		return nil, &APIError{Code: proto.CodeWorkerOffline, Message: "the data leg to the Hub cannot be opened", Err: err}
	}
	return leg, nil
}

// receive runs the GET leg of a pull and stages, verifies and publishes it.
func (c FileCaller) receive(ctx context.Context, adm ops.FileTransferResult, args ops.FilePullArgs, dest string) (ops.FileReceipt, int64, error) {
	leg, err := c.openLeg(ctx, adm)
	if err != nil {
		return ops.FileReceipt{}, 0, err
	}
	defer leg.Close()
	req, err := legRequest(ctx, leg, http.MethodGet, adm.LegToken, nil)
	if err != nil {
		return ops.FileReceipt{}, 0, err
	}
	req.Header.Set("Accept-Encoding", proto.EncodingZstd+", "+proto.EncodingIdentity)
	resp, err := leg.Client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ops.FileReceipt{}, 0, apiErr(proto.CodeConnectionLost, "", "the transfer was cancelled")
		}
		return ops.FileReceipt{}, 0, &APIError{Code: proto.CodeConnectionLost, Message: "the data leg ended", Err: err}
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return ops.FileReceipt{}, 0, legRefused(resp)
	}
	enc := resp.Header.Get("Content-Encoding")
	if enc == "" {
		enc = proto.EncodingIdentity
	}
	if !proto.ValidEncoding(enc) {
		return ops.FileReceipt{}, 0, apiErr(proto.CodeIntegrityFailed, "", "the data leg has a content coding that is not one of files.v1")
	}
	limits := ops.DefaultFileTransferLimits()
	if adm.Limits != nil {
		limits = limits.Lower(*adm.Limits)
	}
	lim := filexfer.LimitsFrom(limits)
	pr := &progressReader{r: resp.Body, every: c.reporter(adm)}
	wire := filexfer.NewWireReader(pr, lim.MaxWire())
	rc, err := c.stage(ctx, wire, resp, adm, args, dest, enc, lim)
	return rc, wire.N(), err
}

func (c FileCaller) stage(ctx context.Context, wire *filexfer.WireReader, resp *http.Response, adm ops.FileTransferResult, args ops.FilePullArgs, dest, enc string, lim filexfer.Limits) (ops.FileReceipt, error) {
	dec, err := filexfer.NewDecoder(wire, enc, lim)
	if err != nil {
		return ops.FileReceipt{}, fxErr(err)
	}
	defer func() { _ = dec.Close() }()
	st, err := filexfer.NewStager(c.Root, adm.TransferID, dest, adm.Kind, filexfer.StageOptions{
		Limits: lim, Overwrite: args.Overwrite, Check: filexfer.PushCheck(c.Root, nil),
		MinFreeBytes: c.MinFreeBytes, MinFreePermille: c.MinFreePermille,
	})
	if err != nil {
		return ops.FileReceipt{}, fxErr(err)
	}
	defer func() { _ = st.Abort() }()
	var (
		m   filexfer.Manifest
		sum [32]byte
	)
	if adm.Kind == ops.FileKindFile {
		mtime := time.Now()
		if lm, err := http.ParseTime(resp.Header.Get("Last-Modified")); err == nil {
			mtime = lm
		}
		if sum, err = filexfer.ReceiveFile(dec, st, adm.Size, mtime); err != nil {
			return ops.FileReceipt{}, fxErr(err)
		}
		m = filexfer.SingleFileManifest(filexfer.BaseName(args.Path), adm.Size, sum)
	} else if m, err = filexfer.Unpack(dec, st); err != nil {
		return ops.FileReceipt{}, fxErr(err)
	}
	if n, err := io.Copy(io.Discard, wire); err != nil || n > 0 {
		return ops.FileReceipt{}, apiErr(proto.CodeIntegrityFailed, "", "the body did not end cleanly")
	}
	md, ok := proto.ParseDigestField(resp.Trailer.Get(proto.HeaderManifestDigest))
	if !ok {
		return ops.FileReceipt{}, apiErr(proto.CodeIntegrityFailed, "", "the manifest digest trailer is missing")
	}
	cd, ok := proto.ParseDigestField(resp.Trailer.Get(proto.HeaderContentDigest))
	if !ok || !bytes.Equal(cd, wire.Sum()) {
		return ops.FileReceipt{}, apiErr(proto.CodeIntegrityFailed, "", "the content digest does not match")
	}
	if adm.Kind == ops.FileKindFile {
		rd, ok := proto.ParseDigestField(resp.Trailer.Get(proto.HeaderReprDigest))
		if !ok || !bytes.Equal(rd, sum[:]) {
			return ops.FileReceipt{}, apiErr(proto.CodeIntegrityFailed, "", "the file digest does not match")
		}
	}
	if err := st.Verify(m, md); err != nil {
		return ops.FileReceipt{}, fxErr(err)
	}
	// The switch as it is now, just before anything becomes visible (FR-18).
	if err := c.allow(ops.FileEffectReceive); err != nil {
		return ops.FileReceipt{}, err
	}
	if ctx.Err() != nil {
		return ops.FileReceipt{}, apiErr(proto.CodeConnectionLost, "", "the transfer was cancelled before the publish")
	}
	var digest [32]byte
	copy(digest[:], md)
	pub, perr := st.Publish()
	rc := ops.FileReceipt{Outcome: ops.OutcomeDelivered, Files: pub.Files, Bytes: pub.Bytes, ManifestDigest: filexfer.DigestHex(digest)}
	if perr != nil {
		if len(pub.Published) == 0 {
			return ops.FileReceipt{}, fxErr(perr)
		}
		rc.Outcome, rc.Reason = ops.OutcomePartial, string(apiProto(fxErr(perr)).Code)
	}
	boundEntries(&rc, pub.Published)
	return rc, nil
}

// pushSource is the inspected source of a push.
type pushSource struct {
	kind    string
	size    int64
	entries int64
	src     *filexfer.SourceFile
}

// CheckPushLocal inspects the source of a push in the caller's root and
// checks what can be refused before anything is sent: the path forms, the
// caller's switch, the pull-sensitive lists (the caller's files leave it) and
// the transfer limits.
func (c FileCaller) CheckPushLocal(ctx context.Context, args ops.FilePushArgs) (as string, s pushSource, err error) {
	if err := filexfer.CheckPath(args.Path, false); err != nil {
		return "", s, fxErr(err)
	}
	as = args.As
	if as == "" {
		as = filexfer.BaseName(args.Path)
	}
	if err := filexfer.CheckPath(as, false); err != nil {
		return "", s, fxErr(err)
	}
	if err := c.allow(ops.FileEffectSend); err != nil {
		return "", s, err
	}
	if c.Root == nil {
		return "", s, apiErr(proto.CodeFilesRootUnknown, "", "the caller has no folder to send from")
	}
	if err := filexfer.CheckPull(args.Path); err != nil {
		return "", s, fxErr(err)
	}
	h, err := c.Root.OpenFile(args.Path)
	if err != nil {
		return "", s, fxErr(err)
	}
	st, err := h.Stat()
	_ = h.Close()
	if err != nil {
		return "", s, internalErr("read the source", err)
	}
	lim := ops.DefaultFileTransferLimits()
	switch {
	case st.IsDir():
		if id, ok := identityOf(st); ok && c.Root.Floor() != nil && c.Root.Floor().SensitiveDir(id) {
			return "", s, apiErr(proto.CodePathSensitive, "", "the folder is sensitive and cannot be sent")
		}
		stats, err := filexfer.Walk(ctx, c.Root, args.Path, filexfer.WalkOptions{Limits: filexfer.LimitsFrom(lim), Filter: filexfer.PullFilter(c.Root, nil)}, false,
			func(filexfer.Entry, *os.File) error { return nil })
		if err != nil {
			return "", s, fxErr(err)
		}
		s = pushSource{kind: ops.FileKindDir, size: stats.Bytes, entries: stats.Files + stats.Dirs}
	case st.Mode().IsRegular():
		src, err := filexfer.OpenSourceFile(c.Root, args.Path)
		if err != nil {
			return "", s, fxErr(err)
		}
		if src.Size > lim.MaxBytes {
			_ = src.Close()
			return "", s, apiErr(proto.CodeTransferTooLarge, "", "the file is larger than the transfer limit")
		}
		s = pushSource{kind: ops.FileKindFile, size: src.Size, entries: 1, src: src}
	default:
		return "", s, apiErr(proto.CodeFileTypeRefused, "", "the entry is not a regular file or folder")
	}
	return as, s, nil
}

// CallerPush runs a push: inspection of the source, admission, the PUT data
// leg with the trailers, and the target's receipt (the 201 answer).
func CallerPush(ctx context.Context, c FileCaller, args ops.FilePushArgs) (ops.FileTransferResult, error) {
	as, src, err := c.CheckPushLocal(ctx, args)
	if err != nil {
		return ops.FileTransferResult{}, err
	}
	if src.src != nil {
		defer func() { _ = src.src.Close() }()
	}
	wire := ops.FilePushArgs{Target: args.Target, Path: filexfer.BaseName(args.Path), As: as, Overwrite: args.Overwrite,
		Kind: src.kind, Size: src.size, Entries: src.entries}
	adm, err := c.Admit(ctx, ops.OpFilePush, wire)
	if err != nil {
		return ops.FileTransferResult{}, err
	}
	if err := checkAdmission(adm, ops.FileDirectionPush); err != nil {
		return ops.FileTransferResult{}, err
	}
	if adm.Size == 0 {
		adm.Size = src.size
	}
	res := completed(adm, c.Root)
	rc, wireBytes, err := c.send(ctx, adm, args, src)
	res.WireBytes = wireBytes
	if err != nil {
		rc, _ = failedReceipt(err)
	}
	fromReceipt(&res, rc)
	return res, nil
}

// send runs the PUT leg of a push and reads the target's receipt.
func (c FileCaller) send(ctx context.Context, adm ops.FileTransferResult, args ops.FilePushArgs, src pushSource) (ops.FileReceipt, int64, error) {
	leg, err := c.openLeg(ctx, adm)
	if err != nil {
		return ops.FileReceipt{}, 0, err
	}
	defer leg.Close()
	enc := proto.EncodingIdentity
	if offered(adm.Encodings, proto.EncodingZstd) {
		enc = proto.EncodingZstd
	}
	if src.kind == ops.FileKindFile && src.size > 0 {
		head := make([]byte, min(src.size, filexfer.ProbeBytes))
		n, _ := src.src.File.ReadAt(head, 0)
		if filexfer.LooksCompressed(filexfer.BaseName(args.Path), head[:n]) {
			enc = proto.EncodingIdentity
		}
	}
	limits := ops.DefaultFileTransferLimits()
	if adm.Limits != nil {
		limits = limits.Lower(*adm.Limits)
	}
	pipeR, pipeW := io.Pipe()
	trailer := http.Header{proto.HeaderContentDigest: nil, proto.HeaderManifestDigest: nil}
	if src.kind == ops.FileKindFile {
		trailer[proto.HeaderReprDigest] = nil
	}
	var wireN atomic.Int64
	go func() {
		pipeW.CloseWithError(c.produce(ctx, pipeW, trailer, &wireN, args, src, enc, limits))
	}()
	pr := &progressReader{r: pipeR, every: c.reporter(adm)}
	req, err := legRequest(ctx, leg, http.MethodPut, adm.LegToken, pr)
	if err != nil {
		_ = pipeR.CloseWithError(err)
		return ops.FileReceipt{}, 0, err
	}
	req.ContentLength = -1
	req.Trailer = trailer
	if src.kind == ops.FileKindFile {
		req.Header.Set("Content-Type", proto.ContentTypeFile)
		req.Header.Set("Last-Modified", src.src.MTime.UTC().Format(http.TimeFormat))
	} else {
		req.Header.Set("Content-Type", proto.ContentTypeTar)
	}
	if enc != proto.EncodingIdentity {
		req.Header.Set("Content-Encoding", enc)
	}
	resp, err := leg.Client.Do(req)
	_ = pipeR.Close()
	if err != nil {
		if ctx.Err() != nil {
			return ops.FileReceipt{}, wireN.Load(), apiErr(proto.CodeConnectionLost, "", "the transfer was cancelled")
		}
		// The body may have reached the target: its outcome is not known.
		return ops.FileReceipt{Outcome: ops.OutcomeUnknown, Reason: string(proto.CodeConnectionLost)}, wireN.Load(), nil
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, proto.MaxFileControlResult+1))
	var rc ops.FileReceipt
	if strings.HasPrefix(resp.Header.Get("Content-Type"), proto.ContentTypeReceipt) && json.Unmarshal(b, &rc) == nil && rc.Validate() == nil {
		return rc, wireN.Load(), nil
	}
	if resp.StatusCode == http.StatusCreated {
		return ops.FileReceipt{Outcome: ops.OutcomeUnknown, Reason: string(proto.CodeInternal)}, wireN.Load(), nil
	}
	var pe proto.Error
	if json.Unmarshal(b, &pe) == nil && pe.Code.Known() {
		return ops.FileReceipt{}, wireN.Load(), apiErr(pe.Code, "", "the data leg was refused: "+pe.Message)
	}
	return ops.FileReceipt{}, wireN.Load(), apiErr(proto.CodeInternal, "", "the data leg was refused")
}

// produce writes the coded body of a push to w and sets the trailers.
func (c FileCaller) produce(ctx context.Context, w io.Writer, trailer http.Header, wireN *atomic.Int64, args ops.FilePushArgs, src pushSource, enc string, limits ops.FileTransferLimits) error {
	ww := filexfer.NewWireWriter(w)
	defer func() { wireN.Store(ww.N()) }()
	cw, err := filexfer.NewEncoder(ww, enc)
	if err != nil {
		return err
	}
	var m filexfer.Manifest
	if src.kind == ops.FileKindDir {
		res, err := filexfer.PackDir(ctx, cw, c.Root, args.Path,
			filexfer.WalkOptions{Limits: filexfer.LimitsFrom(limits), Filter: filexfer.PullFilter(c.Root, nil)})
		if err != nil {
			return err
		}
		m = res.Manifest
		if err := cw.Close(); err != nil {
			return err
		}
	} else {
		sum, err := filexfer.CopyFile(cw, io.NewSectionReader(src.src.File, 0, src.size), src.size)
		if err != nil {
			return err
		}
		if err := cw.Close(); err != nil {
			return err
		}
		if changed, err := src.src.Changed(); err != nil || changed {
			return apiErr(proto.CodeIntegrityFailed, "", "the file changed while it was sent")
		}
		m = filexfer.SingleFileManifest(filexfer.BaseName(args.Path), src.size, sum)
		trailer.Set(proto.HeaderReprDigest, proto.FormatDigestField(sum[:]))
	}
	// The switch as it is now: a caller turned off mid-push sends no
	// trailers, so the target can verify nothing.
	if err := c.allow(ops.FileEffectSend); err != nil {
		return err
	}
	digest, err := m.Digest()
	if err != nil {
		return err
	}
	trailer.Set(proto.HeaderContentDigest, proto.FormatDigestField(ww.Sum()))
	trailer.Set(proto.HeaderManifestDigest, proto.FormatDigestField(digest[:]))
	return nil
}

func offered(list []string, enc string) bool {
	for _, e := range list {
		if e == enc {
			return true
		}
	}
	return false
}
