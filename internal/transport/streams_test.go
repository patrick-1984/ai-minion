package transport

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/yamux"

	"github.com/patrick-1984/ai-minion/internal/proto"
)

const testWait = 5 * time.Second

// rawPeer is a plain yamux session standing in for the other end, so tests see
// the exact bytes our Session puts on a stream and can send arbitrary ones.
type rawPeer struct {
	ys *yamux.Session
}

func yamuxConfig() *yamux.Config {
	cfg := yamux.DefaultConfig()
	cfg.EnableKeepAlive = false
	cfg.LogOutput = io.Discard
	return cfg
}

// sessionAndRaw returns a Session with the given role and a raw yamux peer of
// the opposite role, connected through net.Pipe.
func sessionAndRaw(t *testing.T, role Role, opts SessionOptions) (*Session, *rawPeer) {
	t.Helper()
	a, b := net.Pipe()
	s, err := NewSession(a, role, opts)
	if err != nil {
		t.Fatal(err)
	}
	var ys *yamux.Session
	if role == RoleHub {
		ys, err = yamux.Client(b, yamuxConfig())
	} else {
		ys, err = yamux.Server(b, yamuxConfig())
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close(); _ = ys.Close() })
	return s, &rawPeer{ys: ys}
}

func acceptRaw(t *testing.T, p *rawPeer) *yamux.Stream {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), testWait)
	defer cancel()
	st, err := p.ys.AcceptStreamWithContext(ctx)
	if err != nil {
		t.Fatalf("raw peer accept: %v", err)
	}
	return st
}

func lenPrefix(n int) []byte {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], uint32(n))
	return b[:]
}

// wire returns the bytes that frame body must produce: 4-byte big-endian
// length followed by the JSON.
func wire(body string) []byte { return append(lenPrefix(len(body)), body...) }

func readN(t *testing.T, r io.Reader, n int) []byte {
	t.Helper()
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		t.Fatalf("read %d bytes: %v (got %x)", n, err, buf)
	}
	return buf
}

// expectEnded asserts that reading from ys ends (EOF or reset) soon.
func expectEnded(t *testing.T, ys *yamux.Stream) {
	t.Helper()
	_ = ys.SetReadDeadline(time.Now().Add(testWait))
	if _, err := io.Copy(io.Discard, ys); err != nil && !errors.Is(err, yamux.ErrConnectionReset) && !errors.Is(err, io.EOF) {
		t.Fatalf("stream was not reset: %v", err)
	}
}

func TestKindPrefixBytes(t *testing.T) {
	for _, tc := range []struct {
		k    Kind
		hex  []byte
		name string
	}{
		{KindOp, []byte{0x6f, 0x70}, "op"},
		{KindCall, []byte{0x63, 0x61}, "call"},
		{KindEvents, []byte{0x65, 0x76}, "ev"},
	} {
		p := tc.k.Prefix()
		if !bytes.Equal(p[:], tc.hex) {
			t.Errorf("%s prefix = %x, want %x", tc.name, p, tc.hex)
		}
		if tc.k.String() != tc.name {
			t.Errorf("name %q, want %q", tc.k.String(), tc.name)
		}
		got, ok := ParseKind(p)
		if !ok || got != tc.k {
			t.Errorf("ParseKind(%x) = %v, %v", p, got, ok)
		}
	}
	if p := KindCall.Prefix(); string(p[:]) != "ca" {
		t.Fatal("call kind must be the two bytes ca")
	}
	if KindOp.Opener() != RoleHub || KindCall.Opener() != RoleWorker || KindEvents.Opener() != RoleWorker {
		t.Fatal("wrong opener role")
	}
}

func TestParseKindRejectsEverythingElse(t *testing.T) {
	valid := map[[2]byte]bool{{'o', 'p'}: true, {'c', 'a'}: true, {'e', 'v'}: true}
	for a := 0; a < 256; a++ {
		for b := 0; b < 256; b++ {
			p := [2]byte{byte(a), byte(b)}
			if _, ok := ParseKind(p); ok != valid[p] {
				t.Fatalf("ParseKind(%x) ok=%v", p, ok)
			}
		}
	}
}

// Byte-level framing, outbound: exactly prefix + uint32 BE length + JSON.
func TestOpenedStreamsWriteExactBytes(t *testing.T) {
	for _, tc := range []struct {
		name   string
		role   Role
		open   func(*Session) (*Stream, error)
		prefix []byte
		body   string
	}{
		{"op", RoleHub, func(s *Session) (*Stream, error) { return s.OpenOp(context.Background()) },
			[]byte{0x6f, 0x70}, `{"type":"op.request","op":"snapshot"}`},
		{"ca", RoleWorker, func(s *Session) (*Stream, error) { return s.OpenCall(context.Background()) },
			[]byte{0x63, 0x61}, `{"type":"op.request","op":"pipe_send"}`},
		{"ev", RoleWorker, func(s *Session) (*Stream, error) { return s.OpenEvents(context.Background()) },
			[]byte{0x65, 0x76}, `{"type":"events.snapshot","seq":0}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, raw := sessionAndRaw(t, tc.role, SessionOptions{})
			st, err := tc.open(s)
			if err != nil {
				t.Fatal(err)
			}
			if err := st.WriteFrame(json.RawMessage(tc.body)); err != nil {
				t.Fatal(err)
			}
			rs := acceptRaw(t, raw)
			want := append(append([]byte{}, tc.prefix...), wire(tc.body)...)
			if got := readN(t, rs, len(want)); !bytes.Equal(got, want) {
				t.Fatalf("wire bytes\n got %x\nwant %x", got, want)
			}
			// The length is a big-endian uint32 of the JSON body only.
			if !bytes.Equal(want[2:6], []byte{0, 0, 0, byte(len(tc.body))}) {
				t.Fatalf("length field %x", want[2:6])
			}
			_ = st.Close()
			// Nothing but the frame was written: the stream ends cleanly.
			_ = rs.SetReadDeadline(time.Now().Add(testWait))
			if n, err := rs.Read(make([]byte, 1)); n != 0 || err != io.EOF {
				t.Fatalf("extra bytes after the frame: n=%d err=%v", n, err)
			}
		})
	}
}

// Byte-level framing, inbound: each valid prefix is accepted with its kind and
// the frame decodes.
func TestAcceptedStreamsReadExactBytes(t *testing.T) {
	for _, tc := range []struct {
		name   string
		role   Role
		prefix []byte
		kind   Kind
	}{
		{"op", RoleWorker, []byte{0x6f, 0x70}, KindOp},
		{"ca", RoleHub, []byte{0x63, 0x61}, KindCall},
		{"ev", RoleHub, []byte{0x65, 0x76}, KindEvents},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, raw := sessionAndRaw(t, tc.role, SessionOptions{})
			rs, err := raw.ys.OpenStream()
			if err != nil {
				t.Fatal(err)
			}
			body := `{"type":"op.result","ok":true}`
			if _, err := rs.Write(append(append([]byte{}, tc.prefix...), wire(body)...)); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), testWait)
			defer cancel()
			st, err := s.Accept(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if st.Kind() != tc.kind {
				t.Fatalf("kind %v, want %v", st.Kind(), tc.kind)
			}
			got, err := st.ReadFrame()
			if err != nil || string(got) != body {
				t.Fatalf("frame %q, %v", got, err)
			}
		})
	}
}

// Any other two-byte prefix is rejected and the stream reset.
func TestUnknownPrefixIsRejectedAndReset(t *testing.T) {
	for _, prefix := range []string{"zz", "OP", "ca"[:1] + "x", "\x00\x00", "ap", "pi", "ex"} {
		for _, role := range []Role{RoleHub, RoleWorker} {
			t.Run(role.String()+"/"+prefix, func(t *testing.T) {
				s, raw := sessionAndRaw(t, role, SessionOptions{})
				rs, err := raw.ys.OpenStream()
				if err != nil {
					t.Fatal(err)
				}
				if _, err := rs.Write(append([]byte(prefix), wire(`{"type":"op.request"}`)...)); err != nil {
					t.Fatal(err)
				}
				expectEnded(t, rs)
				ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
				defer cancel()
				if st, err := s.Accept(ctx); err == nil {
					t.Fatalf("Accept delivered a stream of kind %v for prefix %q", st.Kind(), prefix)
				}
			})
		}
	}
}

// The 4-byte string "call" must never be a valid prefix: its first two bytes
// read as "ca", so the next bytes decode as a length that exceeds the limit
// and the stream is reset.
func TestFourByteCallIsNotAKind(t *testing.T) {
	s, raw := sessionAndRaw(t, RoleHub, SessionOptions{})
	rs, err := raw.ys.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rs.Write(append([]byte("call"), wire(`{"type":"op.request"}`)...)); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), testWait)
	defer cancel()
	st, err := s.Accept(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, err = st.ReadFrame()
	var pe *proto.Error
	if !errors.As(err, &pe) || pe.Code != proto.CodeFrameTooLarge {
		t.Fatalf("got %v, want frame_too_large", err)
	}
	expectEnded(t, rs)
}

// A kind from the wrong side is reset: the Hub never receives op, the Worker
// never receives ca or ev.
func TestKindFromWrongSideIsReset(t *testing.T) {
	for _, tc := range []struct {
		name   string
		role   Role // role of the Session under test
		prefix string
	}{
		{"hub gets op", RoleHub, "op"},
		{"worker gets ca", RoleWorker, "ca"},
		{"worker gets ev", RoleWorker, "ev"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, raw := sessionAndRaw(t, tc.role, SessionOptions{})
			rs, err := raw.ys.OpenStream()
			if err != nil {
				t.Fatal(err)
			}
			if _, err := rs.Write(append([]byte(tc.prefix), wire(`{"type":"op.request"}`)...)); err != nil {
				t.Fatal(err)
			}
			expectEnded(t, rs)
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			if _, err := s.Accept(ctx); err == nil {
				t.Fatal("Accept delivered a stream from the wrong side")
			}
			// The session itself stays up.
			if err := s.Err(); err != nil {
				t.Fatalf("session ended: %v", err)
			}
		})
	}
}

func TestOpenStreamEnforcesSide(t *testing.T) {
	hub, _ := sessionAndRaw(t, RoleHub, SessionOptions{})
	worker, _ := sessionAndRaw(t, RoleWorker, SessionOptions{})
	ctx := context.Background()
	for _, k := range []Kind{KindCall, KindEvents, 0, 9} {
		if _, err := hub.OpenStream(ctx, k); !errors.Is(err, ErrWrongSide) {
			t.Errorf("hub opened kind %v: %v", k, err)
		}
	}
	for _, k := range []Kind{KindOp, 0, 9} {
		if _, err := worker.OpenStream(ctx, k); !errors.Is(err, ErrWrongSide) {
			t.Errorf("worker opened kind %v: %v", k, err)
		}
	}
	if hub.NumStreams() != 0 || worker.NumStreams() != 0 {
		t.Fatal("a refused open left a stream behind")
	}
}

func TestSilentStreamDoesNotBlockOthersAndTimesOut(t *testing.T) {
	s, raw := sessionAndRaw(t, RoleHub, SessionOptions{KindTimeout: 200 * time.Millisecond})
	silent, err := raw.ys.OpenStream() // never sends a prefix
	if err != nil {
		t.Fatal(err)
	}
	good, err := raw.ys.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := good.Write(append([]byte("ev"), wire(`{"type":"events.snapshot"}`)...)); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), testWait)
	defer cancel()
	st, err := s.Accept(ctx)
	if err != nil || st.Kind() != KindEvents {
		t.Fatalf("good stream blocked by the silent one: %v", err)
	}
	expectEnded(t, silent)
}

func TestOneBytePrefixThenCloseIsReset(t *testing.T) {
	s, raw := sessionAndRaw(t, RoleHub, SessionOptions{})
	rs, _ := raw.ys.OpenStream()
	_, _ = rs.Write([]byte("o"))
	_ = rs.Close()
	expectEnded(t, rs)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := s.Accept(ctx); err == nil {
		t.Fatal("a truncated prefix produced a stream")
	}
}

// ---------------------------------------------------------- frame limits

func padded(total int) string {
	const frame = `{"type":"x","pad":""}`
	return `{"type":"x","pad":"` + strings.Repeat("a", total-len(frame)) + `"}`
}

func TestStreamFrameAtLimitPassesAndAboveIsRefused(t *testing.T) {
	hub, worker := pairedSessions(t, SessionOptions{}, SessionOptions{})
	ctx, cancel := context.WithTimeout(context.Background(), 2*testWait)
	defer cancel()
	ws, err := worker.OpenCall(ctx)
	if err != nil {
		t.Fatal(err)
	}
	body := padded(proto.StreamFrameLimit)
	if len(body) != proto.StreamFrameLimit {
		t.Fatalf("test body is %d bytes", len(body))
	}
	errc := make(chan error, 1)
	go func() { errc <- ws.WriteFrame(json.RawMessage(body)) }()
	hs, err := hub.Accept(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got, err := hs.ReadFrame()
	if err != nil || len(got) != proto.StreamFrameLimit {
		t.Fatalf("frame at the limit: %d bytes, %v", len(got), err)
	}
	if err := <-errc; err != nil {
		t.Fatal(err)
	}

	// One byte more is refused before anything is written.
	err = ws.WriteFrame(json.RawMessage(padded(proto.StreamFrameLimit + 1)))
	var pe *proto.Error
	if !errors.As(err, &pe) || pe.Code != proto.CodeFrameTooLarge {
		t.Fatalf("oversized write: %v", err)
	}
}

func TestOversizedInboundFrameResetsStream(t *testing.T) {
	s, raw := sessionAndRaw(t, RoleHub, SessionOptions{})
	rs, _ := raw.ys.OpenStream()
	// Announce 1 MiB + 1: the receiver must reject on the header alone.
	hdr := append([]byte("ca"), lenPrefix(proto.StreamFrameLimit+1)...)
	if _, err := rs.Write(hdr); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), testWait)
	defer cancel()
	st, err := s.Accept(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, err = st.ReadFrame()
	var pe *proto.Error
	if !errors.As(err, &pe) || pe.Code != proto.CodeFrameTooLarge {
		t.Fatalf("got %v, want frame_too_large", err)
	}
	expectEnded(t, rs)
}

func TestMalformedInboundFrameResetsStream(t *testing.T) {
	for name, body := range map[string]string{
		"not json":   "hello world",
		"no type":    `{"a":1}`,
		"not an obj": `[1,2]`,
	} {
		t.Run(name, func(t *testing.T) {
			s, raw := sessionAndRaw(t, RoleHub, SessionOptions{})
			rs, _ := raw.ys.OpenStream()
			if _, err := rs.Write(append([]byte("ev"), wire(body)...)); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), testWait)
			defer cancel()
			st, err := s.Accept(ctx)
			if err != nil {
				t.Fatal(err)
			}
			_, err = st.ReadFrame()
			var pe *proto.Error
			if !errors.As(err, &pe) || pe.Code != proto.CodeInvalidArgument {
				t.Fatalf("got %v, want invalid_argument", err)
			}
			expectEnded(t, rs)
		})
	}
}

func TestReadFrameCleanEOF(t *testing.T) {
	hub, worker := pairedSessions(t, SessionOptions{}, SessionOptions{})
	ctx, cancel := context.WithTimeout(context.Background(), testWait)
	defer cancel()
	ws, _ := worker.OpenEvents(ctx)
	hs, err := hub.Accept(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_ = ws.Close()
	if _, err := hs.ReadFrame(); err != io.EOF {
		t.Fatalf("got %v, want io.EOF", err)
	}
}
