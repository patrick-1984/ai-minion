package files

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/transport"
)

// relayBuffer is the size of each of the two copy buffers of a transfer on
// the Hub (BRD NFR-1: <= 1 MiB per transfer with headers).
const relayBuffer = 256 << 10

// Headers relayed from the caller's request to the target, and from the
// target's response to the caller. Everything else (cookies, the caller's
// leg token, hop-by-hop fields) stays where it is.
var (
	requestHeaders  = []string{"Accept-Encoding", "Range", "If-Range", "Content-Type", "Content-Encoding"}
	responseHeaders = []string{"Content-Type", "Content-Encoding", "Content-Range", "ETag", "Accept-Ranges"}
	// legTrailers are the trailers a leg carries: held back until the commit
	// gate has passed, then forwarded.
	legTrailers = []string{proto.HeaderContentDigest, proto.HeaderReprDigest, proto.HeaderManifestDigest}
)

// CallerLeg is who opened a caller leg: the identity it authenticated as
// ([IdentityOf], [WorkerIdentity]) and, for a leg on a Worker connection,
// that connection's handshake transcript (nil for the Hub's HTTP endpoint).
type CallerLeg struct {
	Identity string
	Conn     []byte
}

// legError answers a refused leg with the status of code and a JSON error
// body; a refusal never names the token or a path.
func legError(w http.ResponseWriter, code proto.Code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(proto.HTTPStatus(code))
	_ = json.NewEncoder(w).Encode(proto.NewError(code, msg).Frame())
}

// claim checks the binding of a caller leg and marks the transfer used. It
// returns the transfer or the refusal.
func (s *Service) claim(r *http.Request, caller CallerLeg) (*transfer, proto.Code, string) {
	id, ok := proto.ParseFilesLegPath(r.URL.Path)
	if !ok || !ops.ValidUUIDv7(id) {
		return nil, proto.CodeNotFound, "not found"
	}
	tok := r.Header.Get(proto.HeaderLegToken)
	t := s.get(id)
	if t == nil {
		return nil, proto.CodeTransferExpired, "the transfer is not open (expired, used or unknown)"
	}
	if !proto.LegTokenEqual(tok, t.callerToken) || caller.Identity == "" || caller.Identity != t.callerID ||
		!bytes.Equal(caller.Conn, t.callerConn) {
		return nil, proto.CodeForbidden, "this leg is not bound to the transfer"
	}
	want := http.MethodGet
	if t.direction == ops.FileDirectionPush {
		want = http.MethodPut
	}
	if r.Method != want {
		return nil, proto.CodeForbidden, "the method does not match the direction of the transfer"
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.used || t.finished {
		return nil, proto.CodeTransferExpired, "the leg token was used"
	}
	if s.now().After(t.expires) {
		return nil, proto.CodeTransferExpired, "the transfer expired"
	}
	t.used = true
	if t.timer != nil {
		t.timer.Stop()
		t.timer = nil
	}
	return t, "", ""
}

// ServeLeg serves one caller leg of an admitted transfer: GET for a pull,
// PUT for a push, at proto.FilesLegPath. caller is who opened it; the leg
// must carry the caller's token in proto.HeaderLegToken, come from the
// caller's identity and connection, match the direction and come within the
// admission's expires_at, once. It opens the target leg, relays the body
// without decoding, counts and bounds the wire bytes, ends a leg without
// body bytes for the idle timeout, decides both ends again every 15 s and,
// when the sender's body has ended, forwards the final trailers only after a
// fresh decision passes (commit gate). A failure resets both legs.
func (s *Service) ServeLeg(w http.ResponseWriter, r *http.Request, caller CallerLeg) {
	t, code, msg := s.claim(r, caller)
	if t == nil {
		if code == proto.CodeForbidden {
			s.deniedLeg(r.Context(), r.URL.Path)
		}
		legError(w, code, msg)
		return
	}
	ctx, cancel := context.WithCancelCause(r.Context())
	defer cancel(nil)
	if err := s.redecide(ctx, t); err != nil {
		s.finish(ctx, t, failed(codeOf(err), 0))
		legError(w, codeOf(err), "the transfer is no longer allowed")
		return
	}
	conn, err := s.cfg.Legs.OpenLeg(ctx, t.targetWorker, t.targetConn)
	if err != nil {
		code := proto.CodeWorkerOffline
		if errors.Is(err, transport.ErrTooManyLegs) {
			code = proto.CodeRateLimited
		}
		s.finish(ctx, t, failed(code, 0))
		legError(w, code, "the target Worker cannot take the data leg")
		return
	}
	defer func() { _ = conn.Close() }()
	stop := s.watch(ctx, t, cancel)
	defer stop()
	rl := &relay{s: s, t: t, ctx: ctx, cancel: cancel, maxWire: proto.MaxWireBytes(t.limits.MaxBytes)}
	rl.touch()
	go rl.idle(dur(s.cfg.IdleTimeout, proto.LegIdleTimeout))
	client := &http.Client{Transport: transport.NewLegTransport(conn)}
	if t.direction == ops.FileDirectionPull {
		rl.pull(w, r, client)
	} else {
		rl.push(w, r, client)
	}
}

// deniedLeg audits a leg that presented a binding that does not match.
func (s *Service) deniedLeg(ctx context.Context, path string) {
	id, ok := proto.ParseFilesLegPath(path)
	if !ok || !ops.ValidUUIDv7(id) {
		return
	}
	row, err := s.cfg.Store.FileTransfer(ctx, id)
	if err != nil {
		return
	}
	s.record(ctx, row, AuditDenied)
}

// watch re-decides the transfer every Redecide period until stop; a
// refusal cancels ctx with it, which resets both legs.
func (s *Service) watch(ctx context.Context, t *transfer, cancel context.CancelCauseFunc) (stop func()) {
	every := dur(s.cfg.Redecide, proto.RedecideInterval)
	ctx, end := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		tk := time.NewTicker(every)
		defer tk.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tk.C:
			}
			if err := s.redecide(ctx, t); err != nil {
				if ctx.Err() == nil {
					cancel(err)
				}
				return
			}
		}
	}()
	return func() { end(); <-done }
}

// relay is the state of one transfer's two legs.
type relay struct {
	s       *Service
	t       *transfer
	ctx     context.Context
	cancel  context.CancelCauseFunc
	maxWire int64
	wire    atomic.Int64
	last    atomic.Int64 // unix nanoseconds of the last body bytes
	once    sync.Once
}

func (rl *relay) touch() { rl.last.Store(time.Now().UnixNano()) }

// idle cancels the transfer when no body bytes moved for d.
func (rl *relay) idle(d time.Duration) {
	tk := time.NewTicker(min(d/4+1, time.Second))
	defer tk.Stop()
	for {
		select {
		case <-rl.ctx.Done():
			return
		case <-tk.C:
		}
		if time.Since(time.Unix(0, rl.last.Load())) > d {
			rl.cancel(proto.NewError(proto.CodeConnectionLost, "the data leg was idle too long"))
			return
		}
	}
}

// count counts n wire bytes and refuses beyond the wire limit.
func (rl *relay) count(n int) error {
	if n > 0 {
		rl.touch()
	}
	if rl.wire.Add(int64(n)) > rl.maxWire {
		err := proto.NewError(proto.CodeTransferTooLarge, "the transfer is larger than its limit")
		rl.cancel(err)
		return err
	}
	return nil
}

// cause is why the transfer stopped: the cancel cause (a refusal, the idle
// timeout, the limit) or err.
func (rl *relay) cause(err error) proto.Code {
	if c := context.Cause(rl.ctx); c != nil && !errors.Is(c, context.Canceled) {
		return codeOf(c)
	}
	if rl.ctx.Err() != nil {
		return proto.CodeConnectionLost
	}
	var pe *proto.Error
	if errors.As(err, &pe) {
		return pe.Code
	}
	return proto.CodeConnectionLost
}

// end finishes the record once.
func (rl *relay) end(out store.FileOutcome) {
	rl.once.Do(func() {
		out.WireBytes = rl.wire.Load()
		rl.s.finish(rl.ctx, rl.t, out)
	})
}

// abort resets the caller leg: the response ends without its final chunk
// and trailers, so the receiver cannot verify and publishes nothing.
func abort() { panic(http.ErrAbortHandler) }

// countingReader counts the bytes read from r on the relay.
type countingReader struct {
	r  io.Reader
	rl *relay
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	if cerr := c.rl.count(n); cerr != nil {
		return n, cerr
	}
	return n, err
}

// pull relays a GET: the target sends, the caller receives.
func (rl *relay) pull(w http.ResponseWriter, r *http.Request, client *http.Client) {
	t := rl.t
	out, err := http.NewRequestWithContext(rl.ctx, http.MethodGet, transport.LegURL(proto.FilesLegPath(t.id)), nil)
	if err != nil {
		rl.end(failed(proto.CodeInternal, 0))
		legError(w, proto.CodeInternal, "the data leg could not be started")
		return
	}
	copyHeaders(out.Header, r.Header, requestHeaders)
	out.Header.Set(proto.HeaderLegToken, t.targetToken)
	resp, err := client.Do(out)
	if err != nil {
		code := rl.cause(err)
		rl.end(failed(code, 0))
		legError(w, code, "the target Worker did not answer the data leg")
		return
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		rl.refused(w, resp)
		return
	}
	copyHeaders(w.Header(), resp.Header, responseHeaders)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// The trailers are declared now and set only after the commit gate; the
	// body is chunked, so a leg that is aborted ends without them.
	w.Header().Set("Trailer", strings.Join(legTrailers, ", "))
	w.WriteHeader(resp.StatusCode)
	_ = http.NewResponseController(w).Flush()
	buf := make([]byte, relayBuffer)
	_, err = io.CopyBuffer(w, &countingReader{r: resp.Body, rl: rl}, buf)
	if err != nil || rl.ctx.Err() != nil {
		rl.end(failed(rl.cause(err), 0))
		abort()
	}
	// Commit gate: the body has ended; the trailers go to the receiver only
	// after a fresh decision of both ends.
	if err := rl.s.redecide(rl.ctx, t); err != nil {
		rl.end(failed(codeOf(err), 0))
		abort()
	}
	for _, k := range legTrailers {
		if v := resp.Trailer.Get(k); v != "" {
			w.Header().Set(k, v)
		}
	}
	if t.web {
		// A browser download establishes bytes served, never bytes committed.
		out := store.FileOutcome{State: ops.FileStateServed, Bytes: rl.wire.Load()}
		if sum, ok := proto.ParseDigestField(resp.Trailer.Get(proto.HeaderManifestDigest)); ok {
			out.ManifestDigest = hexOf(sum)
		}
		if t.kind == ops.FileKindFile {
			out.Files = 1
		}
		rl.end(out)
		return
	}
	// The receiver now verifies and publishes; its receipt ends the record.
	t.wire.Store(rl.wire.Load())
	rl.s.awaitReceipt(t)
}

// refused relays a target's refusal of the leg (a bounded error body) and
// ends the record failed_not_sent: no bytes of content moved.
func (rl *relay) refused(w http.ResponseWriter, resp *http.Response) {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, proto.MaxFileControlResult))
	code := proto.CodeInternal
	var f proto.ErrorFrame
	if json.Unmarshal(body, &f) == nil {
		if e := f.Err(); e != nil {
			code = e.Code
		}
	}
	if resp.StatusCode == http.StatusRequestedRangeNotSatisfiable {
		code = proto.CodeInvalidArgument
		copyHeaders(w.Header(), resp.Header, []string{"Content-Range"})
	}
	rl.end(failed(code, 0))
	w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(body)
}

// gateReader is the body of the push request to the target: the caller's
// body, counted, whose end runs the commit gate and only then copies the
// caller's trailers into the outgoing request's trailers. A refusal fails the
// read, which aborts the request without trailers: the target publishes
// nothing.
type gateReader struct {
	rl  *relay
	in  *http.Request // the caller's request
	out *http.Request // the request to the target
	src io.Reader
	// gated is set when the gate passed and the trailers were set.
	gated bool
}

func (g *gateReader) Read(p []byte) (int, error) {
	n, err := g.src.Read(p)
	if cerr := g.rl.count(n); cerr != nil {
		return n, cerr
	}
	if errors.Is(err, io.EOF) {
		if gerr := g.rl.s.redecide(g.rl.ctx, g.rl.t); gerr != nil {
			g.rl.cancel(gerr)
			return n, gerr
		}
		for _, k := range legTrailers {
			if v := g.in.Trailer.Get(k); v != "" {
				g.out.Trailer.Set(k, v)
			}
		}
		g.gated = true
	}
	return n, err
}

// push relays a PUT: the caller sends, the target receives and answers 201
// with its receipt.
func (rl *relay) push(w http.ResponseWriter, r *http.Request, client *http.Client) {
	t := rl.t
	// A read of the caller's body that waits must end with the transfer.
	rc := http.NewResponseController(w)
	stopRead := context.AfterFunc(rl.ctx, func() { _ = rc.SetReadDeadline(time.Now()) })
	defer stopRead()
	g := &gateReader{rl: rl, in: r, src: r.Body}
	out, err := http.NewRequestWithContext(rl.ctx, http.MethodPut, transport.LegURL(proto.FilesLegPath(t.id)), io.NopCloser(g))
	if err != nil {
		rl.end(failed(proto.CodeInternal, 0))
		legError(w, proto.CodeInternal, "the data leg could not be started")
		return
	}
	g.out = out
	out.ContentLength = -1
	out.Trailer = http.Header{}
	for _, k := range legTrailers {
		if _, ok := r.Trailer[http.CanonicalHeaderKey(k)]; ok {
			out.Trailer[http.CanonicalHeaderKey(k)] = nil
		}
	}
	copyHeaders(out.Header, r.Header, requestHeaders)
	out.Header.Set(proto.HeaderLegToken, t.targetToken)
	resp, err := client.Do(out)
	if err != nil {
		// The trailers did not reach the target unless the gate passed and
		// the body ended: before that, nothing can have been published.
		code := rl.cause(err)
		if g.gated {
			rl.end(store.FileOutcome{State: ops.FileStateUnknown, Reason: string(code)})
		} else {
			rl.end(failed(code, 0))
		}
		legError(w, code, "the data leg to the target Worker ended")
		return
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusCreated {
		rl.refused(w, resp)
		return
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, proto.MaxFileControlResult+1))
	var rec ops.FileReceipt
	if err != nil || len(body) > proto.MaxFileControlResult || json.Unmarshal(body, &rec) != nil || rec.Validate() != nil {
		// The target may have published: without a readable receipt the
		// outcome is unknown.
		rl.end(store.FileOutcome{State: ops.FileStateUnknown, Reason: string(proto.CodeInternal)})
		legError(w, proto.CodeInternal, "the target Worker sent an unreadable receipt")
		return
	}
	rl.end(receiptOutcome(rec))
	w.Header().Set("Content-Type", proto.ContentTypeReceipt)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusCreated)
	_, _ = w.Write(body)
}

// receiptOutcome is the record outcome of a receiver's receipt.
func receiptOutcome(rec ops.FileReceipt) store.FileOutcome {
	return store.FileOutcome{
		State: string(rec.Outcome), Reason: rec.Reason, Files: rec.Files, Bytes: rec.Bytes,
		ManifestDigest: rec.ManifestDigest, Entries: rec.Entries, EntriesOmitted: rec.EntriesOmitted,
	}
}

func copyHeaders(dst, src http.Header, names []string) {
	for _, k := range names {
		if v := src.Values(k); len(v) > 0 {
			dst[http.CanonicalHeaderKey(k)] = append([]string(nil), v...)
		}
	}
}

func hexOf(b []byte) string {
	const digits = "0123456789abcdef"
	out := make([]byte, 2*len(b))
	for i, c := range b {
		out[2*i], out[2*i+1] = digits[c>>4], digits[c&15]
	}
	return string(out)
}
