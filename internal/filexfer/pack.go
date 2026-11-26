package filexfer

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"time"

	"github.com/patrick-1984/ai-minion/internal/proto"
)

// Restricted pax profile (BRD 5.5): typeflags TypeReg and TypeDir only; pax
// records only path, size and mtime (Go's archive/tar writes a record only
// when the ustar fields cannot hold the value); no linkpath, uname, gname,
// xattr, sparse or GNU extensions; the ustar mode written as 0644 or 0755 and
// ignored on read; directory names end in "/". The last entry is the manifest
// at proto.ManifestName.

// copyBufSize is the copy unit of file content (bounded memory, NFR-1).
const copyBufSize = 256 << 10

// TarWriter writes a restricted pax archive.
type TarWriter struct {
	tw  *tar.Writer
	buf []byte
}

// NewTarWriter returns a writer of a restricted pax archive into w (usually
// the encoder of the body).
func NewTarWriter(w io.Writer) *TarWriter {
	return &TarWriter{tw: tar.NewWriter(w), buf: make([]byte, copyBufSize)}
}

func paxTime(t time.Time) time.Time {
	if t.IsZero() || t.Before(time.Unix(0, 0)) {
		return time.Unix(0, 0)
	}
	return t
}

// WriteDir writes a directory entry.
func (t *TarWriter) WriteDir(rel string, mtime time.Time) error {
	return t.tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: rel + "/", Mode: 0o755,
		ModTime: paxTime(mtime), Format: tar.FormatPAX})
}

// WriteFile writes a regular file entry of exactly size bytes read from r and
// returns their SHA-256. Fewer bytes than size is ErrSourceChanged; bytes
// beyond size are not read.
func (t *TarWriter) WriteFile(rel string, size int64, mtime time.Time, r io.Reader) ([32]byte, error) {
	if err := t.tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: rel, Size: size, Mode: 0o644,
		ModTime: paxTime(mtime), Format: tar.FormatPAX}); err != nil {
		return [32]byte{}, err
	}
	h := sha256.New()
	n, err := io.CopyBuffer(io.MultiWriter(t.tw, h), io.LimitReader(r, size), t.buf)
	if err != nil {
		return [32]byte{}, err
	}
	if n != size {
		return [32]byte{}, ErrSourceChanged
	}
	var sum [32]byte
	h.Sum(sum[:0])
	return sum, nil
}

// Close writes the manifest entry and the end-of-archive marker. It does not
// close the underlying writer.
func (t *TarWriter) Close(m Manifest) error {
	b, err := m.Canonical()
	if err != nil {
		return err
	}
	if err := t.tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: proto.ManifestName, Size: int64(len(b)),
		Mode: 0o644, ModTime: time.Unix(0, 0), Format: tar.FormatPAX}); err != nil {
		return err
	}
	if _, err := t.tw.Write(b); err != nil {
		return err
	}
	return t.tw.Close()
}

// PackResult is the outcome of PackDir.
type PackResult struct {
	Manifest Manifest
	Stats    WalkStats
}

// PackDir walks the folder base beneath root (Walk) and writes it to w as a
// restricted pax archive with the manifest last. w is the encoder of the body
// (NewEncoder); the caller closes the encoder afterwards and sends the
// trailers: the manifest digest (Manifest.Digest) and the Content-Digest of
// the coded bytes (WireWriter). A file that changes while it is read fails the
// transfer with ErrSourceChanged.
func PackDir(ctx context.Context, w io.Writer, root Root, base string, opts WalkOptions) (PackResult, error) {
	tw := NewTarWriter(w)
	var m Manifest
	lim := opts.Limits.Normalize()
	stats, err := Walk(ctx, root, base, opts, true, func(e Entry, f *os.File) error {
		if e.Type == proto.FileTypeDir {
			if err := tw.WriteDir(e.Path, e.MTime); err != nil {
				return err
			}
			return m.Add(proto.ManifestEntry{Path: e.Path, Type: proto.FileTypeDir}, lim.MaxManifestBytes)
		}
		before, err := fstatFile(f)
		if err != nil {
			return internal("a file cannot be read")
		}
		sum, err := tw.WriteFile(e.Path, e.Size, e.MTime, f)
		if err != nil {
			return err
		}
		after, err := fstatFile(f)
		if err != nil || after.Size != before.Size || !after.MTime.Equal(before.MTime) || !after.CTime.Equal(before.CTime) {
			return ErrSourceChanged
		}
		return m.Add(proto.ManifestEntry{Path: e.Path, Size: e.Size, SHA256: hex.EncodeToString(sum[:]), Type: proto.FileTypeFile},
			lim.MaxManifestBytes)
	})
	if err != nil {
		return PackResult{Stats: stats}, err
	}
	if err := tw.Close(m); err != nil {
		return PackResult{Stats: stats}, err
	}
	return PackResult{Manifest: m, Stats: stats}, nil
}

// CopyFile copies exactly size bytes from r to w (the raw body of a single
// file, or a range of it) and returns their SHA-256. Fewer bytes is
// ErrSourceChanged.
func CopyFile(w io.Writer, r io.Reader, size int64) ([32]byte, error) {
	h := sha256.New()
	buf := make([]byte, proto.LegChunkBytes)
	n, err := io.CopyBuffer(io.MultiWriter(w, h), io.LimitReader(r, size), buf)
	if err != nil {
		return [32]byte{}, err
	}
	if n != size {
		return [32]byte{}, ErrSourceChanged
	}
	var sum [32]byte
	h.Sum(sum[:0])
	return sum, nil
}
