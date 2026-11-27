package filexfer

import (
	"compress/gzip"
	"crypto/sha256"
	"errors"
	"hash"
	"io"
	"path"
	"strconv"
	"strings"
	"sync"

	"github.com/klauspost/compress/zstd"

	"github.com/patrick-1984/ai-minion/internal/proto"
)

// Content codings of a data-leg body (BRD 5.5, NFR-4): zstd between Minion
// ends (level 1, window <= 8 MiB, frame checksum on), gzip (stdlib) for web
// folder downloads, identity for already compressed content and resumable
// single files. Decoders are bounded (NFR-6) and report a clean end only when
// the last frame or member ended with a verified checksum and nothing
// followed it.

// NewEncoder returns a writer that encodes into w with the coding enc. Close
// ends the last frame or member (it does not close w).
func NewEncoder(w io.Writer, enc string) (io.WriteCloser, error) {
	switch enc {
	case proto.EncodingIdentity:
		return nopWriteCloser{w}, nil
	case proto.EncodingGzip:
		return gzip.NewWriterLevel(w, gzip.BestSpeed)
	case proto.EncodingZstd:
		conc := 2
		if SmallDevice() {
			conc = 1
		}
		return zstd.NewWriter(w,
			zstd.WithEncoderLevel(zstd.SpeedFastest),
			zstd.WithWindowSize(proto.ZstdMaxEncoderWindow),
			zstd.WithEncoderConcurrency(conc),
			zstd.WithEncoderCRC(true),
			zstd.WithZeroFrames(true),
			zstd.WithLowerEncoderMem(SmallDevice()))
	}
	return nil, proto.NewError(proto.CodeInvalidArgument, "unknown content coding")
}

type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }

// NewDecoder returns a reader of the decoded content of r, which is coded with
// enc, bounded by l (zstd window and memory). The reader returns io.EOF only
// on a clean end: every zstd frame checksum or gzip CRC and ISIZE verified and
// no trailing bytes. A decoding failure is integrity_failed, a window or
// memory bound transfer_too_large; an error of r itself (a *proto.Error, a
// reset stream) is returned unchanged. Close releases the decoder; it does
// not close r.
func NewDecoder(r io.Reader, enc string, l Limits) (io.ReadCloser, error) {
	l = l.Normalize()
	src := &srcReader{r: r}
	switch enc {
	case proto.EncodingIdentity:
		return &codecReader{r: src, src: src}, nil
	case proto.EncodingGzip:
		zr, err := gzip.NewReader(src)
		if err != nil {
			return nil, mapCodecErr(err, src)
		}
		zr.Multistream(true)
		return &codecReader{r: zr, src: src, close: zr.Close}, nil
	case proto.EncodingZstd:
		zr, err := zstd.NewReader(src,
			zstd.WithDecoderConcurrency(1),
			zstd.WithDecoderMaxWindow(proto.ZstdMaxWindow),
			zstd.WithDecoderMaxMemory(l.ZstdMaxMemory),
			zstd.WithDecoderLowmem(SmallDevice()))
		if err != nil {
			return nil, internal("the zstd decoder could not start")
		}
		return &codecReader{r: zr, src: src, close: func() error { zr.Close(); return nil }}, nil
	}
	return nil, proto.NewError(proto.CodeInvalidArgument, "unknown content coding")
}

// srcReader remembers the last error of the coded source other than io.EOF.
type srcReader struct {
	r   io.Reader
	err error
}

func (s *srcReader) Read(p []byte) (int, error) {
	n, err := s.r.Read(p)
	if err != nil && !errors.Is(err, io.EOF) {
		s.err = err
	}
	return n, err
}

type codecReader struct {
	r     io.Reader
	src   *srcReader
	close func() error
	done  bool
}

func (c *codecReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	if err == nil || err == io.EOF {
		return n, err
	}
	return n, mapCodecErr(err, c.src)
}

func (c *codecReader) Close() error {
	if c.done || c.close == nil {
		return nil
	}
	c.done = true
	return c.close()
}

// mapCodecErr maps a decoder error to a proto error; an error of the source
// wins.
func mapCodecErr(err error, src *srcReader) error {
	if src.err != nil {
		return src.err
	}
	var pe *proto.Error
	if errors.As(err, &pe) {
		return pe
	}
	switch {
	case errors.Is(err, zstd.ErrWindowSizeExceeded), errors.Is(err, zstd.ErrDecoderSizeExceeded),
		errors.Is(err, zstd.ErrFrameSizeExceeded):
		return tooLarge("the content coding exceeds the decoder bounds")
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return integrity("the coded content is truncated")
	}
	return integrity("the coded content is not valid")
}

// ExpectEOF checks that r has nothing left: a read returns io.EOF and no
// byte. It is used after the end-of-archive marker or the last byte of a
// single file, so that trailing data is integrity_failed.
func ExpectEOF(r io.Reader) error {
	var b [1]byte
	for {
		n, err := r.Read(b[:])
		if n > 0 {
			return integrity("the content has trailing data")
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// WireReader counts and hashes the coded bytes of a data-leg body (the
// content of RFC 9530 Content-Digest) and refuses more than max of them
// (transfer_too_large).
type WireReader struct {
	m *meter
	h hash.Hash
}

// NewWireReader wraps a coded body; max <= 0 means the default bound of a
// transfer under DefaultLimits.
func NewWireReader(r io.Reader, max int64) *WireReader {
	if max <= 0 {
		max = DefaultLimits().MaxWire()
	}
	h := sha256.New()
	return &WireReader{m: newMeter(io.TeeReader(r, h), max, tooLarge("the transfer has more wire bytes than the limit")), h: h}
}

func (w *WireReader) Read(p []byte) (int, error) { return w.m.Read(p) }

// N returns the coded bytes read.
func (w *WireReader) N() int64 { return w.m.n }

// Sum returns the SHA-256 of the coded bytes read.
func (w *WireReader) Sum() []byte { return w.h.Sum(nil) }

// WireWriter counts and hashes the coded bytes a sender writes.
type WireWriter struct {
	w io.Writer
	h hash.Hash
	n int64
}

// NewWireWriter wraps the body writer of a sender.
func NewWireWriter(w io.Writer) *WireWriter { return &WireWriter{w: w, h: sha256.New()} }

func (w *WireWriter) Write(p []byte) (int, error) {
	n, err := w.w.Write(p)
	w.h.Write(p[:n])
	w.n += int64(n)
	return n, err
}

// N returns the coded bytes written.
func (w *WireWriter) N() int64 { return w.n }

// Sum returns the SHA-256 of the coded bytes written.
func (w *WireWriter) Sum() []byte { return w.h.Sum(nil) }

// ParseAcceptEncoding returns the known codings of an Accept-Encoding value
// with a q-value above zero, in the order given. "*" is not expanded.
func ParseAcceptEncoding(v string) []string {
	var out []string
	for _, part := range strings.Split(v, ",") {
		fields := strings.Split(part, ";")
		name := strings.ToLower(strings.TrimSpace(fields[0]))
		if !proto.ValidEncoding(name) {
			continue
		}
		ok := true
		for _, f := range fields[1:] {
			k, val, _ := strings.Cut(strings.TrimSpace(f), "=")
			if strings.EqualFold(strings.TrimSpace(k), "q") {
				if q, err := strconv.ParseFloat(strings.TrimSpace(val), 64); err != nil || q <= 0 {
					ok = false
				}
			}
		}
		if ok {
			out = append(out, name)
		}
	}
	return out
}

// Negotiate picks the coding of a body: the first of prefer that the
// Accept-Encoding value accept names; identity when none matches (identity is
// always acceptable in files.v1).
func Negotiate(accept string, prefer ...string) string {
	acc := ParseAcceptEncoding(accept)
	for _, p := range prefer {
		for _, a := range acc {
			if a == p {
				return p
			}
		}
	}
	return proto.EncodingIdentity
}

// compressedExt lists extensions of content that is compressed already.
var compressedExt = map[string]bool{
	".gz": true, ".tgz": true, ".zst": true, ".xz": true, ".bz2": true, ".lz": true, ".lz4": true,
	".7z": true, ".zip": true, ".rar": true, ".jar": true, ".apk": true, ".aab": true, ".whl": true,
	".jpg": true, ".jpeg": true, ".png": true, ".gif": true, ".webp": true, ".heic": true, ".avif": true,
	".mp3": true, ".mp4": true, ".m4a": true, ".m4v": true, ".mkv": true, ".mov": true, ".webm": true,
	".ogg": true, ".opus": true, ".flac": true, ".docx": true, ".xlsx": true, ".pptx": true,
	".odt": true, ".epub": true, ".br": true, ".dmg": true,
}

// ProbeBytes is the size of the head of a file that LooksCompressed reads.
const ProbeBytes = 64 << 10

var (
	probeOnce sync.Once
	probeEnc  *zstd.Encoder
)

// LooksCompressed reports whether a file is better sent with identity coding:
// its name has an extension of compressed content, or its first ProbeBytes
// (head) do not shrink by 5 % under zstd level 1.
func LooksCompressed(name string, head []byte) bool {
	if compressedExt[strings.ToLower(path.Ext(name))] {
		return true
	}
	if len(head) < 4<<10 {
		return false
	}
	if len(head) > ProbeBytes {
		head = head[:ProbeBytes]
	}
	probeOnce.Do(func() {
		probeEnc, _ = zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedFastest), zstd.WithEncoderConcurrency(1),
			zstd.WithWindowSize(1<<20))
	})
	if probeEnc == nil {
		return false
	}
	out := probeEnc.EncodeAll(head, make([]byte, 0, len(head)+64))
	return int64(len(out))*100 > int64(len(head))*95
}
