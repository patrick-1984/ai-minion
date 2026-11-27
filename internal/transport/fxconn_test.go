package transport

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/yamux"

	"github.com/patrick-1984/ai-minion/internal/proto"
)

const testLegToken = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

var filesOpts = SessionOptions{Files: true}

func TestFilesKindRefusedWithoutFeature(t *testing.T) {
	hub, worker := pairedSessions(t, SessionOptions{}, SessionOptions{})
	ctx := context.Background()
	if _, err := hub.OpenStream(ctx, KindFiles); !errors.Is(err, ErrFilesNotNegotiated) {
		t.Fatalf("hub opened fx: %v", err)
	}
	if _, err := worker.OpenLeg(ctx); !errors.Is(err, ErrFilesNotNegotiated) {
		t.Fatalf("worker opened fx: %v", err)
	}
	if hub.NumStreams() != 0 || worker.NumStreams() != 0 {
		t.Fatal("a refused open left a stream behind")
	}
	if hub.Files() || hub.Pacer() != nil {
		t.Fatal("a session without files.v1 must have no legs and no pacer")
	}
}

// A peer that did not negotiate files.v1 resets an inbound fx stream like an
// unknown kind; the session survives and other streams go on.
func TestInboundFilesRefusedWithoutFeatureSessionSurvives(t *testing.T) {
	for _, role := range []Role{RoleHub, RoleWorker} {
		t.Run(role.String(), func(t *testing.T) {
			s, raw := sessionAndRaw(t, role, SessionOptions{})
			ys, err := raw.ys.OpenStream()
			if err != nil {
				t.Fatal(err)
			}
			if _, err := ys.Write([]byte("fxGET / HTTP/1.1\r\n\r\n")); err != nil {
				t.Fatal(err)
			}
			expectEnded(t, ys)
			ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
			defer cancel()
			if st, err := s.Accept(ctx); err == nil {
				t.Fatalf("fx stream was delivered: %v", st.Kind())
			}
			if err := s.Err(); err != nil {
				t.Fatalf("session ended: %v", err)
			}
		})
	}
}

// Both ends enabled, but the peer is older or did not enable files: the leg
// ends at once, the HTTP client gets an error, the session survives and the
// leg slot is free again.
func TestLegToPeerWithoutFeatureFailsCleanly(t *testing.T) {
	hub, worker := pairedSessions(t, filesOpts, SessionOptions{})
	ctx, cancel := context.WithTimeout(context.Background(), testWait)
	defer cancel()
	for i := 0; i < proto.MaxActiveLegsPerConn+1; i++ {
		c, err := hub.OpenLeg(ctx)
		if err != nil {
			t.Fatalf("leg %d: %v", i, err)
		}
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, LegURL(proto.FilesLegPath("x")), nil)
		resp, err := (&http.Client{Transport: NewLegTransport(c)}).Do(req)
		if err == nil {
			_ = resp.Body.Close()
			t.Fatal("request over a refused leg succeeded")
		}
		_ = c.Close()
	}
	op, err := hub.OpenOp(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := op.WriteFrame(map[string]any{"type": "op.request"}); err != nil {
		t.Fatal(err)
	}
	got, err := worker.Accept(ctx)
	if err != nil || got.Kind() != KindOp {
		t.Fatalf("op after refused legs: %v", err)
	}
	if hub.Err() != nil || worker.Err() != nil {
		t.Fatal("a session ended")
	}
}

func TestLegLimitOutbound(t *testing.T) {
	hub, _ := pairedSessions(t, filesOpts, filesOpts)
	ctx := context.Background()
	var legs []*LegConn
	for i := 0; i < proto.MaxActiveLegsPerConn; i++ {
		c, err := hub.OpenLeg(ctx)
		if err != nil {
			t.Fatal(err)
		}
		legs = append(legs, c)
	}
	if _, err := hub.OpenLeg(ctx); !errors.Is(err, ErrTooManyLegs) {
		t.Fatalf("third leg: %v", err)
	}
	_ = legs[0].Close()
	c, err := hub.OpenLeg(ctx)
	if err != nil {
		t.Fatalf("leg after a close: %v", err)
	}
	_ = c.Close()
	_ = legs[1].Close()
}

func TestLegLimitInbound(t *testing.T) {
	s, raw := sessionAndRaw(t, RoleWorker, filesOpts)
	ctx, cancel := context.WithTimeout(context.Background(), testWait)
	defer cancel()
	var accepted []*Stream
	for i := 0; i < proto.MaxActiveLegsPerConn; i++ {
		ys, err := raw.ys.OpenStream()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := ys.Write([]byte("fx")); err != nil {
			t.Fatal(err)
		}
		st, err := s.Accept(ctx)
		if err != nil || st.Kind() != KindFiles {
			t.Fatalf("accept %d: %v", i, err)
		}
		accepted = append(accepted, st)
	}
	extra, err := raw.ys.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := extra.Write([]byte("fx")); err != nil {
		t.Fatal(err)
	}
	expectEnded(t, extra)
	if s.Err() != nil {
		t.Fatal("session ended")
	}
	accepted[0].Reset()
	again, err := raw.ys.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := again.Write([]byte("fx")); err != nil {
		t.Fatal(err)
	}
	if st, err := s.Accept(ctx); err != nil || st.Kind() != KindFiles {
		t.Fatalf("accept after a reset: %v", err)
	}
}

// serveOne accepts the next stream on s and serves it with h; the returned
// channel gets ServeLeg's error.
func serveOne(t *testing.T, ctx context.Context, s *Session, h http.Handler) <-chan error {
	t.Helper()
	ch := make(chan error, 1)
	go func() {
		st, err := s.Accept(ctx)
		if err != nil {
			ch <- err
			return
		}
		if st.Kind() != KindFiles {
			ch <- errors.New("not an fx stream")
			return
		}
		ch <- ServeLeg(ctx, NewLegServer(h), NewLegConn(st))
	}()
	return ch
}

func pattern(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*7 + i/251)
	}
	return b
}

// Pull shape: the Hub opens the leg to the target Worker and GETs the content;
// the Worker answers with a chunked body and the digest trailers.
func TestLegGetWithTrailers(t *testing.T) {
	hub, worker := pairedSessions(t, filesOpts, filesOpts)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	body := pattern(5<<20 + 123)
	sum := sha256.Sum256(body)
	id := "0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a5b"

	served := serveOne(t, ctx, worker, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, ok := proto.ParseFilesLegPath(r.URL.Path)
		if r.Method != http.MethodGet || !ok || got != id || r.Header.Get(proto.HeaderLegToken) != testLegToken {
			http.Error(w, "bad leg", http.StatusForbidden)
			return
		}
		w.Header().Set("Trailer", proto.HeaderContentDigest+", "+proto.HeaderManifestDigest)
		w.Header().Set("Content-Type", proto.ContentTypeFile)
		w.Header().Set("Content-Encoding", proto.EncodingIdentity)
		w.WriteHeader(http.StatusOK)
		for p := body; len(p) > 0; {
			n := min(len(p), 100_000)
			if _, err := w.Write(p[:n]); err != nil {
				return
			}
			p = p[n:]
		}
		w.Header().Set(proto.HeaderContentDigest, proto.FormatDigestField(sum[:]))
		w.Header().Set(proto.HeaderManifestDigest, proto.FormatDigestField(sum[:]))
	}))

	c, err := hub.OpenLeg(ctx)
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, LegURL(proto.FilesLegPath(id)), nil)
	req.Header.Set(proto.HeaderLegToken, testLegToken)
	req.Header.Set("Accept-Encoding", proto.EncodingIdentity)
	resp, err := (&http.Client{Transport: NewLegTransport(c)}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	h := sha256.New()
	n, err := io.Copy(h, resp.Body)
	_ = resp.Body.Close()
	if err != nil || n != int64(len(body)) {
		t.Fatalf("body %d bytes: %v", n, err)
	}
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Encoding") != proto.EncodingIdentity {
		t.Fatalf("status %d, encoding %q", resp.StatusCode, resp.Header.Get("Content-Encoding"))
	}
	if got, ok := proto.ParseDigestField(resp.Trailer.Get(proto.HeaderContentDigest)); !ok || !bytes.Equal(got, h.Sum(nil)) {
		t.Fatalf("Content-Digest trailer %q does not match", resp.Trailer.Get(proto.HeaderContentDigest))
	}
	if resp.Trailer.Get(proto.HeaderManifestDigest) == "" {
		t.Fatal("manifest digest trailer missing")
	}
	if err := <-served; err != nil {
		t.Fatalf("ServeLeg: %v", err)
	}
	waitFor(t, "leg slots free", func() bool { return len(hub.legs) == 0 && len(worker.legs) == 0 })
}

// Push and caller-leg shape: the Worker opens the leg to the Hub and PUTs a
// chunked body with request trailers; the Hub answers 201 with a receipt.
func TestLegPutWithRequestTrailers(t *testing.T) {
	hub, worker := pairedSessions(t, filesOpts, filesOpts)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	body := pattern(3<<20 + 7)
	sum := sha256.Sum256(body)

	served := serveOne(t, ctx, hub, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || r.ContentLength != -1 {
			http.Error(w, "want chunked PUT", http.StatusBadRequest)
			return
		}
		h := sha256.New()
		if _, err := io.Copy(h, r.Body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		got, ok := proto.ParseDigestField(r.Trailer.Get(proto.HeaderContentDigest))
		if !ok || !bytes.Equal(got, h.Sum(nil)) {
			http.Error(w, "digest mismatch", http.StatusUnprocessableEntity)
			return
		}
		w.Header().Set("Content-Type", proto.ContentTypeReceipt)
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"outcome":"delivered"}`)
	}))

	c, err := worker.OpenLeg(ctx)
	if err != nil {
		t.Fatal(err)
	}
	pr, pw := io.Pipe()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPut, LegURL(proto.FilesLegPath("id")), pr)
	req.ContentLength = -1
	req.Header.Set(proto.HeaderLegToken, testLegToken)
	req.Header.Set("Content-Type", proto.ContentTypeFile)
	req.Trailer = http.Header{proto.HeaderContentDigest: nil}
	go func() {
		for p := body; len(p) > 0; {
			n := min(len(p), 70_000)
			if _, err := pw.Write(p[:n]); err != nil {
				return
			}
			p = p[n:]
		}
		req.Trailer.Set(proto.HeaderContentDigest, proto.FormatDigestField(sum[:]))
		_ = pw.Close()
	}()
	resp, err := (&http.Client{Transport: NewLegTransport(c)}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	receipt, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusCreated || string(receipt) != `{"outcome":"delivered"}` {
		t.Fatalf("status %d: %s", resp.StatusCode, receipt)
	}
	if err := <-served; err != nil {
		t.Fatalf("ServeLeg: %v", err)
	}
}

// One leg carries one request: a second dial of the transport fails, and the
// server never reads a pipelined second request.
func TestLegCarriesOneRequest(t *testing.T) {
	hub, worker := pairedSessions(t, filesOpts, filesOpts)
	ctx, cancel := context.WithTimeout(context.Background(), testWait)
	defer cancel()
	var mu sync.Mutex
	calls := 0
	served := serveOne(t, ctx, worker, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		_, _ = io.WriteString(w, "ok")
	}))
	c, err := hub.OpenLeg(ctx)
	if err != nil {
		t.Fatal(err)
	}
	two := "GET /a HTTP/1.1\r\nHost: x\r\n\r\nGET /b HTTP/1.1\r\nHost: x\r\n\r\n"
	if _, err := c.Write([]byte(two)); err != nil {
		t.Fatal(err)
	}
	_ = c.SetReadDeadline(time.Now().Add(testWait))
	out, _ := io.ReadAll(c)
	if strings.Count(string(out), "HTTP/1.1 200") != 1 || !strings.Contains(string(out), "Connection: close") {
		t.Fatalf("answer: %q", out)
	}
	if err := <-served; err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Fatalf("handler ran %d times", calls)
	}

	c2, err := hub.OpenLeg(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c2.Close() }()
	tr := NewLegTransport(c2)
	if _, err := tr.DialContext(ctx, "tcp", LegHost+":80"); err != nil {
		t.Fatal(err)
	}
	if _, err := tr.DialContext(ctx, "tcp", LegHost+":80"); !errors.Is(err, errLegUsed) {
		t.Fatalf("second dial: %v", err)
	}
}

// Cancelling the client's request resets the leg: the server's handler sees
// its writes fail and ServeLeg returns within a second (NFR-7).
func TestLegCancelStopsTheOtherEnd(t *testing.T) {
	hub, worker := pairedSessions(t, filesOpts, filesOpts)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	handlerDone := make(chan error, 1)
	served := serveOne(t, ctx, worker, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		chunk := pattern(proto.LegChunkBytes)
		for {
			if _, err := w.Write(chunk); err != nil {
				handlerDone <- err
				return
			}
		}
	}))
	c, err := hub.OpenLeg(ctx)
	if err != nil {
		t.Fatal(err)
	}
	rctx, rcancel := context.WithCancel(ctx)
	req, _ := http.NewRequestWithContext(rctx, http.MethodGet, LegURL("/x"), nil)
	resp, err := (&http.Client{Transport: NewLegTransport(c)}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.CopyN(io.Discard, resp.Body, 1<<20); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	rcancel()
	_ = resp.Body.Close()
	select {
	case err := <-served:
		if err != nil {
			t.Fatalf("ServeLeg: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("server end did not stop")
	}
	if err := <-handlerDone; err == nil {
		t.Fatal("handler write did not fail")
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("other end stopped after %v", d)
	}
	if hub.Err() != nil || worker.Err() != nil {
		t.Fatal("a session ended")
	}
}

// ServeLeg's context ending abandons the leg.
func TestServeLegContextEndsLeg(t *testing.T) {
	hub, worker := pairedSessions(t, filesOpts, filesOpts)
	ctx, cancel := context.WithTimeout(context.Background(), testWait)
	defer cancel()
	sctx, scancel := context.WithCancel(ctx)
	entered := make(chan struct{})
	served := serveOne(t, sctx, worker, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-r.Context().Done()
	}))
	c, err := hub.OpenLeg(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	go func() {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, LegURL("/x"), nil)
		if resp, err := (&http.Client{Transport: NewLegTransport(c)}).Do(req); err == nil {
			_ = resp.Body.Close()
		}
	}()
	<-entered
	scancel()
	select {
	case err := <-served:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("ServeLeg: %v", err)
		}
	case <-time.After(testWait):
		t.Fatal("ServeLeg did not return")
	}
}

// frameRecorder wraps the raw peer's end of the pipe and records the payload
// length of every yamux data frame the Session sends (header: version, type,
// flags, stream id, length; type 0 is data and its payload follows).
type frameRecorder struct {
	net.Conn
	mu      sync.Mutex
	pending []byte
	skip    int
	frames  []int
}

func (r *frameRecorder) Read(p []byte) (int, error) {
	n, err := r.Conn.Read(p)
	r.mu.Lock()
	defer r.mu.Unlock()
	b := p[:n]
	for len(b) > 0 {
		if r.skip > 0 {
			k := min(r.skip, len(b))
			r.skip -= k
			b = b[k:]
			continue
		}
		k := min(12-len(r.pending), len(b))
		r.pending = append(r.pending, b[:k]...)
		b = b[k:]
		if len(r.pending) < 12 {
			break
		}
		if r.pending[1] == 0 { // data frame
			length := int(binary.BigEndian.Uint32(r.pending[8:12]))
			if length > 0 {
				r.frames = append(r.frames, length)
			}
			r.skip = length
		}
		r.pending = r.pending[:0]
	}
	return n, err
}

func (r *frameRecorder) dataFrames() []int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.frames)
}

// Writes of a leg are cut into frames of at most proto.LegChunkBytes. The
// frames are observed on the wire: a read of the peer's yamux stream returns
// whatever its receive buffer holds, which can be several frames at once.
func TestLegWritesAreChunked(t *testing.T) {
	a, b := net.Pipe()
	s, err := NewSession(a, RoleHub, filesOpts)
	if err != nil {
		t.Fatal(err)
	}
	rec := &frameRecorder{Conn: b}
	ys, err := yamux.Client(rec, yamuxConfig())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close(); _ = ys.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), testWait)
	defer cancel()
	c, err := s.OpenLeg(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	st := acceptRaw(t, &rawPeer{ys: ys})
	if got := readN(t, st, 2); string(got) != "fx" {
		t.Fatalf("prefix %q", got)
	}
	before := len(rec.dataFrames())
	big := pattern(200 << 10)
	done := make(chan error, 1)
	go func() { _, err := c.Write(big); done <- err }()
	got := make([]byte, len(big))
	if _, err := io.ReadFull(st, got); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, big) {
		t.Fatal("the peer read other bytes than were written")
	}
	frames := rec.dataFrames()[before:]
	total := 0
	for _, n := range frames {
		if n > proto.LegChunkBytes {
			t.Fatalf("a data frame of %d bytes: larger than a chunk (frames %v)", n, frames)
		}
		total += n
	}
	if total != len(big) || len(frames) < (len(big)+proto.LegChunkBytes-1)/proto.LegChunkBytes {
		t.Fatalf("%d data frames with %d bytes for %d bytes written: %v", len(frames), total, len(big), frames)
	}
}

// inFlight returns how many bytes a sender can write on a fresh stream of kind
// k whose reader has read one byte and then stops.
func inFlight(t *testing.T, hub, worker *Session, k Kind) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), testWait)
	defer cancel()
	st, err := hub.OpenStream(ctx, k)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Reset()
	if _, err := st.Write([]byte{1}); err != nil {
		t.Fatal(err)
	}
	in, err := worker.Accept(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Reset()
	one := make([]byte, 1)
	if _, err := io.ReadFull(in, one); err != nil { // the read sends the window update
		t.Fatal(err)
	}
	_ = st.SetWriteDeadline(time.Now().Add(500 * time.Millisecond))
	total := 0
	chunk := make([]byte, 32<<10)
	for {
		n, err := st.Write(chunk)
		total += n
		if err != nil {
			break
		}
	}
	return total
}

// The receive window grows to MaxStreamWindow on a session with data legs (a
// sender can then have that much unread data in flight), and stays at the
// yamux default on a session without them.
func TestStreamWindowGrowsToMaxOnlyWithFiles(t *testing.T) {
	hub, worker := pairedSessions(t, filesOpts, filesOpts)
	if got := inFlight(t, hub, worker, KindFiles); got < MaxStreamWindow-(32<<10) || got > MaxStreamWindow+(32<<10) {
		t.Fatalf("fx: %d bytes in flight, want about %d", got, MaxStreamWindow)
	}
	if got := inFlight(t, hub, worker, KindOp); got < MaxStreamWindow-(32<<10) {
		t.Fatalf("op on a files session: %d bytes in flight", got)
	}
	hub2, worker2 := pairedSessions(t, SessionOptions{}, SessionOptions{})
	if got := inFlight(t, hub2, worker2, KindOp); got > DefaultStreamWindow+(32<<10) {
		t.Fatalf("op without files: %d bytes in flight, want at most about %d", got, DefaultStreamWindow)
	}
}

func TestLegConnAddrAndAfterClose(t *testing.T) {
	hub, _ := pairedSessions(t, filesOpts, filesOpts)
	c, err := hub.OpenLeg(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if c.LocalAddr().Network() != "fx" || !strings.HasPrefix(c.RemoteAddr().String(), "fx:") {
		t.Fatalf("addr %v", c.LocalAddr())
	}
	_ = c.Close()
	_ = c.Close()
	if _, err := c.Write([]byte("x")); err == nil {
		t.Fatal("write after close")
	}
	if _, err := c.Read(make([]byte, 1)); err == nil {
		t.Fatal("read after close")
	}
	select {
	case <-c.Done():
	default:
		t.Fatal("Done not closed")
	}
	l := NewLegListener(c)
	if _, err := l.Accept(); err != nil {
		t.Fatal("the first accept hands out the conn even when closed")
	}
	if _, err := l.Accept(); err == nil {
		t.Fatal("second accept of a closed leg")
	}
}
