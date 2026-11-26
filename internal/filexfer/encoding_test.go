package filexfer

import (
	"bytes"
	"compress/gzip"
	"crypto/rand"
	"errors"
	"io"
	"testing"

	"github.com/klauspost/compress/zstd"

	"github.com/patrick-1984/ai-minion/internal/proto"
)

func encode(t *testing.T, enc string, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w, err := NewEncoder(&buf, enc)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func decodeAll(enc string, wire []byte, l Limits) ([]byte, error) {
	r, err := NewDecoder(bytes.NewReader(wire), enc, l)
	if err != nil {
		return nil, err
	}
	defer func() { _ = r.Close() }()
	return io.ReadAll(r)
}

func TestCodingsRoundTrip(t *testing.T) {
	data := bytes.Repeat([]byte("minion file transfer "), 50000)
	for _, enc := range []string{proto.EncodingZstd, proto.EncodingGzip, proto.EncodingIdentity} {
		wire := encode(t, enc, data)
		got, err := decodeAll(enc, wire, DefaultLimits())
		if err != nil || !bytes.Equal(got, data) {
			t.Fatalf("%s: %v", enc, err)
		}
	}
	if _, err := NewEncoder(io.Discard, "br"); err == nil {
		t.Fatal("unknown coding accepted")
	}
}

func TestZstdRefusesTrailingAndTruncated(t *testing.T) {
	data := bytes.Repeat([]byte("abc"), 100000)
	wire := encode(t, proto.EncodingZstd, data)
	for name, bad := range map[string][]byte{
		"trailing byte":    append(append([]byte{}, wire...), 0x00),
		"trailing garbage": append(append([]byte{}, wire...), []byte("garbage!")...),
		"truncated frame":  wire[:len(wire)-3],
		"bit flip":         flip(wire, len(wire)/2),
	} {
		if _, err := decodeAll(proto.EncodingZstd, bad, DefaultLimits()); proto.CodeOf(err) != proto.CodeIntegrityFailed {
			t.Fatalf("%s: %v", name, err)
		}
	}
}

func flip(b []byte, i int) []byte {
	c := append([]byte{}, b...)
	c[i] ^= 0x10
	return c
}

// A frame that declares a window above 8 MiB is refused before it allocates.
func TestZstdBombLargeWindow(t *testing.T) {
	var buf bytes.Buffer
	w, err := zstd.NewWriter(&buf, zstd.WithWindowSize(64<<20), zstd.WithEncoderLevel(zstd.SpeedFastest), zstd.WithSingleSegment(false))
	if err != nil {
		t.Fatal(err)
	}
	// Incompressible, longer than 8 MiB so that the encoder keeps the large
	// window in the frame header.
	chunk := make([]byte, 1<<20)
	_, _ = rand.Read(chunk)
	for i := 0; i < 12; i++ {
		_, _ = w.Write(chunk)
	}
	_ = w.Close()
	_, err = decodeAll(proto.EncodingZstd, buf.Bytes(), DefaultLimits())
	if c := proto.CodeOf(err); c != proto.CodeTransferTooLarge {
		t.Fatalf("large window: %v", err)
	}
}

// A high-ratio stream is stopped by the decoded-bytes bound of the consumer
// (the archive meter), reading only a bounded amount.
func TestZstdBombHighRatio(t *testing.T) {
	var buf bytes.Buffer
	w, _ := NewEncoder(&buf, proto.EncodingZstd)
	zero := make([]byte, 1<<20)
	for i := 0; i < 256; i++ { // 256 MiB of zeros
		_, _ = w.Write(zero)
	}
	_ = w.Close()
	if buf.Len() > 1<<20 {
		t.Fatalf("unexpected ratio: %d wire bytes", buf.Len())
	}
	r, err := NewDecoder(bytes.NewReader(buf.Bytes()), proto.EncodingZstd, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	m := newMeter(r, 4<<20, tooLarge("too large"))
	n, err := io.Copy(io.Discard, m)
	if proto.CodeOf(err) != proto.CodeTransferTooLarge || n > 4<<20 {
		t.Fatalf("read %d: %v", n, err)
	}
}

func TestGzipRefusesBadCRCAndTrailing(t *testing.T) {
	data := bytes.Repeat([]byte("hello gzip "), 10000)
	wire := encode(t, proto.EncodingGzip, data)
	badCRC := append([]byte{}, wire...)
	badCRC[len(badCRC)-6] ^= 0xff // CRC32 is the 8 bytes before the end with ISIZE
	badSize := append([]byte{}, wire...)
	badSize[len(badSize)-1] ^= 0x01
	for name, bad := range map[string][]byte{
		"crc":       badCRC,
		"isize":     badSize,
		"trailing":  append(append([]byte{}, wire...), []byte("xx")...),
		"truncated": wire[:len(wire)-4],
		"empty":     {},
	} {
		if _, err := decodeAll(proto.EncodingGzip, bad, DefaultLimits()); proto.CodeOf(err) != proto.CodeIntegrityFailed {
			t.Fatalf("%s: %v", name, err)
		}
	}
	// A second gzip member is valid (RFC 1952 multi-member).
	two := append(append([]byte{}, wire...), wire...)
	got, err := decodeAll(proto.EncodingGzip, two, DefaultLimits())
	if err != nil || len(got) != 2*len(data) {
		t.Fatalf("multi-member: %v", err)
	}
	_ = gzip.ErrChecksum
}

type failingReader struct{ err error }

func (f failingReader) Read([]byte) (int, error) { return 0, f.err }

func TestDecoderKeepsSourceError(t *testing.T) {
	reset := errors.New("stream reset")
	src := io.MultiReader(bytes.NewReader(encode(t, proto.EncodingZstd, []byte("x"))[:5]), failingReader{reset})
	r, err := NewDecoder(src, proto.EncodingZstd, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadAll(r); !errors.Is(err, reset) {
		t.Fatalf("got %v", err)
	}
}

func TestWireReaderBound(t *testing.T) {
	w := NewWireReader(bytes.NewReader(make([]byte, 100)), 99)
	if _, err := io.ReadAll(w); proto.CodeOf(err) != proto.CodeTransferTooLarge {
		t.Fatalf("got %v", err)
	}
	w = NewWireReader(bytes.NewReader(make([]byte, 100)), 100)
	if _, err := io.ReadAll(w); err != nil || w.N() != 100 || len(w.Sum()) != 32 {
		t.Fatalf("exact bound: %v", err)
	}
}

func TestNegotiateAndProbe(t *testing.T) {
	if got := Negotiate("gzip, zstd;q=0.9", proto.EncodingZstd, proto.EncodingGzip); got != proto.EncodingZstd {
		t.Fatal(got)
	}
	if got := Negotiate("zstd;q=0, br", proto.EncodingZstd, proto.EncodingGzip); got != proto.EncodingIdentity {
		t.Fatal(got)
	}
	random := make([]byte, ProbeBytes)
	_, _ = rand.Read(random)
	if !LooksCompressed("data.bin", random) || LooksCompressed("data.txt", bytes.Repeat([]byte("a"), ProbeBytes)) {
		t.Fatal("probe")
	}
	if !LooksCompressed("photo.JPG", nil) {
		t.Fatal("extension")
	}
}

func TestExpectEOF(t *testing.T) {
	if ExpectEOF(bytes.NewReader(nil)) != nil || proto.CodeOf(ExpectEOF(bytes.NewReader([]byte{0}))) != proto.CodeIntegrityFailed {
		t.Fatal("ExpectEOF")
	}
}
