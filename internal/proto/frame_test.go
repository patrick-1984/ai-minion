package proto

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"unicode/utf8"
)

type testFrame struct {
	Type string `json:"type"`
	Data string `json:"data,omitempty"`
}

// frameOfBodyLen returns a typed JSON frame whose encoded body is exactly n bytes.
func frameOfBodyLen(t *testing.T, n int) testFrame {
	t.Helper()
	base, _ := json.Marshal(testFrame{Type: "x", Data: "a"})
	pad := n - len(base)
	if pad < 0 {
		t.Fatalf("n=%d too small", n)
	}
	return testFrame{Type: "x", Data: "a" + strings.Repeat("a", pad)}
}

// rawBody frames body with its true length.
func rawBody(body string) []byte { return raw(uint32(len(body)), body) }

func raw(n uint32, body string) []byte {
	b := make([]byte, 4, 4+len(body))
	binary.BigEndian.PutUint32(b, n)
	return append(b, body...)
}

func TestFrameWireFormatAndRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	in := testFrame{Type: TypeHello, Data: "h\u00e9llo \u2713"}
	if err := WriteFrame(&buf, HandshakeFrameLimit, in); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(in)
	want := append([]byte{0, 0, 0, byte(len(body))}, body...)
	if !bytes.Equal(buf.Bytes(), want) {
		t.Fatalf("wire bytes = %x, want %x", buf.Bytes(), want)
	}
	got, err := ReadFrame(&buf, HandshakeFrameLimit)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, body) {
		t.Fatalf("body = %s, want %s", got, body)
	}
	var out testFrame
	if err := DecodeFrame(got, &out); err != nil || out != in {
		t.Fatalf("decode = %+v, %v", out, err)
	}
	if ty, err := FrameType(got); err != nil || ty != TypeHello {
		t.Fatalf("FrameType = %q, %v", ty, err)
	}
	if _, err := ReadFrame(&buf, HandshakeFrameLimit); err != io.EOF {
		t.Fatalf("after last frame: %v, want io.EOF", err)
	}
}

func TestFrameLimitsAreExported(t *testing.T) {
	if HandshakeFrameLimit != 64*1024 || StreamFrameLimit != 1024*1024 {
		t.Fatalf("limits = %d, %d", HandshakeFrameLimit, StreamFrameLimit)
	}
}

func TestFrameBoundaries(t *testing.T) {
	const limit = 128
	tests := []struct {
		name    string
		bodyLen int
		wantErr error
	}{
		{"minimum typed object", len(`{"type":"x"}`), nil},
		{"limit-1", limit - 1, nil},
		{"limit", limit, nil},
		{"limit+1", limit + 1, ErrFrameTooLarge},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := testFrame{Type: "x"}
			if tc.bodyLen > len(`{"type":"x"}`) {
				f = frameOfBodyLen(t, tc.bodyLen)
			}
			var buf bytes.Buffer
			err := WriteFrame(&buf, limit, f)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("write err = %v, want %v", err, tc.wantErr)
			}
			if tc.wantErr != nil {
				if buf.Len() != 0 {
					t.Fatalf("rejected write still wrote %d bytes", buf.Len())
				}
				return
			}
			if buf.Len() != 4+tc.bodyLen {
				t.Fatalf("wrote %d bytes, want %d", buf.Len(), 4+tc.bodyLen)
			}
			if _, err := ReadFrame(&buf, limit); err != nil {
				t.Fatalf("read: %v", err)
			}
		})
	}
}

func TestReadFrameRejectsOversizeWithoutReadingBody(t *testing.T) {
	// The body is absent: if the reader tried to read it, it would hit EOF
	// instead of frame_too_large.
	r := &countingReader{r: bytes.NewReader(raw(HandshakeFrameLimit+1, ""))}
	_, err := ReadFrame(r, HandshakeFrameLimit)
	if !errors.Is(err, ErrFrameTooLarge) || CodeOf(err) != CodeFrameTooLarge {
		t.Fatalf("err = %v, want frame_too_large", err)
	}
	if r.n != 4 {
		t.Fatalf("read %d bytes, want only the 4-byte length", r.n)
	}
	// Largest possible length value.
	_, err = ReadFrame(bytes.NewReader(raw(0xFFFFFFFF, "")), StreamFrameLimit)
	if !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("err = %v", err)
	}
}

type countingReader struct {
	r io.Reader
	n int
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += n
	return n, err
}

func TestReadFrameTruncationAndMalformed(t *testing.T) {
	good := `{"type":"x"}`
	tests := []struct {
		name string
		in   []byte
		want error
	}{
		{"empty stream", nil, io.EOF},
		{"truncated length 1 byte", []byte{0}, io.ErrUnexpectedEOF},
		{"truncated length 3 bytes", []byte{0, 0, 0}, io.ErrUnexpectedEOF},
		{"length but no body", raw(uint32(len(good)), ""), io.ErrUnexpectedEOF},
		{"truncated body", raw(uint32(len(good)), good[:5]), io.ErrUnexpectedEOF},
		{"zero length", raw(0, ""), ErrInvalidArgument},
		{"not an object", rawBody("[]"), ErrInvalidArgument},
		{"not json", rawBody("abc"), ErrInvalidArgument},
		{"truncated json", rawBody(`{"typ`), ErrInvalidArgument},
		{"no type", rawBody("{}"), ErrInvalidArgument},
		{"empty type", rawBody(`{"type":""}`), ErrInvalidArgument},
		{"type not a string", rawBody(`{"type":1}`), ErrInvalidArgument},
		{"type null", rawBody(`{"type":null}`), ErrInvalidArgument},
		{"invalid UTF-8 inside a string", rawBody("{\"type\":\"x\",\"d\":\"\xff\xfe\"}"), ErrInvalidArgument},
		{"upper-case TYPE only", rawBody(`{"TYPE":"x"}`), ErrInvalidArgument},
		{"mixed-case Type only", rawBody(`{"Type":"x"}`), ErrInvalidArgument},
		{"duplicate type", rawBody(`{"type":"ping","type":"x"}`), ErrInvalidArgument},
		{"data after the object", rawBody(`{"type":"x"} {}`), ErrInvalidArgument},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ReadFrame(bytes.NewReader(tc.in), StreamFrameLimit)
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestWriteFrameRejectsNonFrames(t *testing.T) {
	for name, v := range map[string]any{
		"no type":                      struct{ A int }{1},
		"array":                        []int{1},
		"string":                       "x",
		"nil":                          nil,
		"empty type":                   testFrame{},
		"unencodable":                  map[string]any{"type": "x", "c": make(chan int)},
		"invalid UTF-8 in raw message": json.RawMessage("{\"type\":\"x\",\"d\":\"\xff\xfe\"}"),
		"upper-case TYPE only":         json.RawMessage(`{"TYPE":"x"}`),
	} {
		t.Run(name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := WriteFrame(&buf, StreamFrameLimit, v); !errors.Is(err, ErrInvalidArgument) {
				t.Fatalf("err = %v, want invalid_argument", err)
			}
			if buf.Len() != 0 {
				t.Fatal("wrote bytes")
			}
		})
	}
}

type errWriter struct{}

func (errWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestWriteFramePropagatesWriterError(t *testing.T) {
	if err := WriteFrame(errWriter{}, StreamFrameLimit, testFrame{Type: "x"}); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("err = %v", err)
	}
}

func TestWriteFrameAcceptsRawMessage(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteFrame(&buf, StreamFrameLimit, json.RawMessage(`{"type":"ping"}`)); err != nil {
		t.Fatal(err)
	}
	got, err := ReadFrame(&buf, StreamFrameLimit)
	if err != nil || string(got) != `{"type":"ping"}` {
		t.Fatalf("%s %v", got, err)
	}
}

func TestFrameTypeIsExactAndTolerantOfWhitespace(t *testing.T) {
	for _, tc := range []struct {
		name, body, want string
	}{
		{"conflicting case variant is an unknown field", `{"type":"ping","TYPE":"pong"}`, "ping"},
		{"case variant before the real key", `{"Type":"pong","type":"ping"}`, "ping"},
		{"leading whitespace", " \n\t{\"type\":\"ping\"}", "ping"},
		{"trailing whitespace", "{\"type\":\"ping\"} \r\n", "ping"},
		{"pretty-printed", "{\n  \"type\": \"ping\",\n  \"n\": 1e309\n}", "ping"},
		{"escaped key", `{"\u0074ype":"ping"}`, "ping"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := FrameType([]byte(tc.body))
			if err != nil || got != tc.want {
				t.Fatalf("FrameType = %q, %v, want %q", got, err, tc.want)
			}
			// ReadFrame accepts it and returns the received bytes untouched.
			b, err := ReadFrame(bytes.NewReader(raw(uint32(len(tc.body)), tc.body)), StreamFrameLimit)
			if err != nil || string(b) != tc.body {
				t.Fatalf("ReadFrame = %q, %v", b, err)
			}
		})
	}
}

func TestSeveralFramesInSequence(t *testing.T) {
	var buf bytes.Buffer
	for _, ty := range []string{TypeOpRequest, TypeOpProgress, TypeOpResult} {
		if err := WriteFrame(&buf, StreamFrameLimit, testFrame{Type: ty}); err != nil {
			t.Fatal(err)
		}
	}
	for _, want := range []string{TypeOpRequest, TypeOpProgress, TypeOpResult} {
		b, err := ReadFrame(&buf, StreamFrameLimit)
		if err != nil {
			t.Fatal(err)
		}
		if ty, _ := FrameType(b); ty != want {
			t.Fatalf("type = %q, want %q", ty, want)
		}
	}
}

func FuzzReadFrame(f *testing.F) {
	f.Add(raw(12, `{"type":"x"}`))
	f.Add(raw(0, ""))
	f.Add([]byte{0xff, 0xff, 0xff, 0xff})
	f.Add(rawBody("{}"))
	f.Add([]byte{0, 0})
	f.Add(rawBody("{\"type\":\"x\",\"d\":\"\xff\xfe\"}"))
	f.Add(rawBody(`{"type":"ping","TYPE":"pong","a":1}`))
	f.Add(rawBody(" \n{\"type\":\"x\",\"a\":[]}"))
	f.Add(rawBody(`{"type":"x","extra":1e309}`))
	f.Add(rawBody(`{"type":"x","d":"` + strings.Repeat("<", 1000) + `"}`))
	f.Fuzz(func(t *testing.T, data []byte) {
		const limit = 4096
		body, err := ReadFrame(bytes.NewReader(data), limit)
		if err != nil {
			return
		}
		if len(body) > limit {
			t.Fatalf("returned %d bytes over limit", len(body))
		}
		if ty, err := FrameType(body); err != nil || ty == "" {
			t.Fatalf("accepted frame without type: %q %v", body, err)
		}
		if !utf8.Valid(body) {
			t.Fatalf("accepted frame body is not valid UTF-8: %q", body)
		}
		// Whatever was accepted must be re-writable and read back equivalently.
		// Re-encoding may expand the body (encoding/json escapes <, > and &, up
		// to 6x), so the size limit is only enforced on the original wire frame
		// above and the rewrite gets headroom.
		const rewriteLimit = 6 * limit
		var buf bytes.Buffer
		if err := WriteFrame(&buf, rewriteLimit, body); err != nil {
			t.Fatalf("rewrite: %v", err)
		}
		again, err := ReadFrame(&buf, rewriteLimit)
		if err != nil || !jsonEqual(again, body) {
			t.Fatalf("round trip differs: %q vs %q (%v)", again, body, err)
		}
	})
}

// jsonEqual compares two JSON documents structurally. Numbers are kept as
// their literal text (UseNumber) so that values outside the float64 range,
// such as 1e309, compare fine.
func jsonEqual(a, b []byte) bool {
	x, okx := decodeNumbers(a)
	y, oky := decodeNumbers(b)
	return okx && oky && string(mustMarshal(x)) == string(mustMarshal(y))
}

func decodeNumbers(b []byte) (any, bool) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var v any
	return v, dec.Decode(&v) == nil
}

func mustMarshal(v any) []byte { b, _ := json.Marshal(v); return b }
