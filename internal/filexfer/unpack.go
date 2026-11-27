package filexfer

import (
	"archive/tar"
	"bytes"
	"errors"
	"io"
	"strings"
	"time"

	"github.com/patrick-1984/ai-minion/internal/proto"
)

// allowedPAX are the only pax records of the restricted profile.
var allowedPAX = map[string]bool{"path": true, "size": true, "mtime": true}

// Unpack reads a restricted pax archive from the decoded body r (NewDecoder)
// and stages every entry with st, a Stager of kind dir. It returns the
// manifest of the archive (its last entry) after checking: only TypeReg and
// TypeDir entries with valid paths, no other pax records than path, size and
// mtime, no link names, owner names or GNU extensions, header blocks within
// MaxEntryHeaderBytes per entry, at most st's limits of entries and bytes,
// decoded bytes within Limits.MaxDecoded, the manifest last and equal to the
// received entries, the end-of-archive marker (two zero blocks) present and
// nothing after it. The caller then checks the trailers and calls st.Verify.
// Any failure is a *proto.Error (integrity_failed, transfer_too_large,
// path_outside_root, or the Check hook's error); the staged files stay for
// Abort.
func Unpack(r io.Reader, st *Stager) (Manifest, error) {
	lim := st.opts.Limits
	m := newMeter(r, lim.MaxDecoded(), tooLarge("the archive is larger than the limit of this transfer"))
	headerErr := tooLarge("an archive entry header is larger than the limit")
	tr := tar.NewReader(m)
	var prevSize int64
	for {
		pad := (blockSize - prevSize%blockSize) % blockSize
		m.setBudget(pad+MaxEntryHeaderBytes, headerErr)
		before := m.n
		hdr, err := tr.Next()
		if err == io.EOF {
			// archive/tar also reports io.EOF for a stream that ends at an
			// entry boundary without the marker: require both zero blocks.
			if m.n-before != pad+2*blockSize {
				return Manifest{}, integrity("the archive has no end-of-archive marker")
			}
			return Manifest{}, integrity("the archive has no manifest")
		}
		if err != nil {
			return Manifest{}, archiveErr(err, m)
		}
		if err := checkHeader(hdr); err != nil {
			return Manifest{}, err
		}
		if hdr.Name == proto.ManifestName {
			man, err := readManifest(tr, m, hdr, lim)
			if err != nil {
				return Manifest{}, err
			}
			if err := sameEntries(st.Received(), man.Entries); err != nil {
				return Manifest{}, err
			}
			return man, endOfArchive(tr, m, hdr.Size)
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			name := strings.TrimSuffix(hdr.Name, "/")
			if err := st.StageDir(name, hdr.ModTime); err != nil {
				return Manifest{}, err
			}
			prevSize = 0
		case tar.TypeReg:
			if strings.HasSuffix(hdr.Name, "/") {
				return Manifest{}, integrity("an archive file name ends in a slash")
			}
			m.setBudget(hdr.Size, tooLarge("an archive entry is larger than its header"))
			if _, err := st.StageFile(hdr.Name, hdr.Size, hdr.ModTime, tr); err != nil {
				return Manifest{}, archiveErr(err, m)
			}
			prevSize = hdr.Size
		}
	}
}

// checkHeader enforces the restricted pax profile on one header.
func checkHeader(h *tar.Header) error {
	if h.Typeflag != tar.TypeReg && h.Typeflag != tar.TypeDir {
		return typeRefused("the archive has an entry that is not a regular file or folder")
	}
	if h.Linkname != "" {
		return typeRefused("the archive has a link entry")
	}
	if h.Format&tar.FormatGNU != 0 {
		return integrity("the archive uses GNU extensions")
	}
	for k := range h.PAXRecords {
		if !allowedPAX[k] {
			return integrity("the archive has a pax record outside the restricted profile")
		}
	}
	if h.Uname != "" || h.Gname != "" || h.Devmajor != 0 || h.Devminor != 0 {
		return integrity("the archive has owner or device fields")
	}
	if h.Size < 0 || (h.Typeflag == tar.TypeDir && h.Size != 0) {
		return integrity("the archive has an entry with an invalid size")
	}
	return nil
}

func readManifest(tr *tar.Reader, m *meter, hdr *tar.Header, lim Limits) (Manifest, error) {
	if hdr.Typeflag != tar.TypeReg {
		return Manifest{}, integrity("the manifest entry is not a file")
	}
	if hdr.Size > lim.MaxManifestBytes {
		return Manifest{}, tooLarge("the manifest of this transfer is larger than the limit")
	}
	m.setBudget(hdr.Size, tooLarge("the manifest entry is larger than its header"))
	var buf bytes.Buffer
	n, err := io.Copy(&buf, tr)
	if err != nil {
		return Manifest{}, archiveErr(err, m)
	}
	if n != hdr.Size {
		return Manifest{}, integrity("the manifest is truncated")
	}
	return ParseManifest(buf.Bytes(), lim)
}

// endOfArchive requires that the manifest entry is followed by the
// end-of-archive marker and that the decoded stream ends there.
func endOfArchive(tr *tar.Reader, m *meter, prevSize int64) error {
	pad := (blockSize - prevSize%blockSize) % blockSize
	m.setBudget(pad+2*blockSize, integrity("the archive has data after the manifest"))
	before := m.n
	_, err := tr.Next()
	if err == nil {
		return integrity("the archive has an entry after the manifest")
	}
	if err != io.EOF {
		return archiveErr(err, m)
	}
	if m.n-before != pad+2*blockSize {
		return integrity("the archive has no end-of-archive marker")
	}
	m.setBudget(-1, nil)
	return ExpectEOF(m)
}

// archiveErr maps an error of archive/tar or of the source.
func archiveErr(err error, m *meter) error {
	if m.err != nil {
		return m.err
	}
	if m.srcErr != nil {
		var pe *proto.Error
		if errors.As(m.srcErr, &pe) {
			return pe
		}
		return m.srcErr
	}
	var pe *proto.Error
	if errors.As(err, &pe) {
		return pe
	}
	if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
		return integrity("the archive is truncated")
	}
	return integrity("the archive is not a valid restricted pax archive")
}

// ReceiveFile stages a single-file transfer: exactly size bytes from the
// decoded body r (NewDecoder) and then the end of r (trailing bytes are
// integrity_failed). st is a Stager of kind file. It returns the SHA-256 of
// the content; the caller builds the expected manifest with
// SingleFileManifest, checks the trailers (Repr-Digest against the returned
// sum, Minion-Manifest-Digest) and calls st.Verify.
func ReceiveFile(r io.Reader, st *Stager, size int64, mtime time.Time) ([32]byte, error) {
	sum, err := st.StageFile("", size, mtime, r)
	if err != nil {
		return [32]byte{}, err
	}
	if err := ExpectEOF(r); err != nil {
		return [32]byte{}, err
	}
	return sum, nil
}
