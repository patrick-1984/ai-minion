package version

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
)

// maxBinarySize bounds the unpacked binary so that a hostile archive cannot
// fill the disk. A release binary is a few tens of megabytes.
const maxBinarySize = 512 << 20

// BinaryName returns the file name of the minion binary inside an archive and
// in the cache for an OS.
func BinaryName(goos string) string {
	if goos == "windows" {
		return "minion.exe"
	}
	return "minion"
}

// extractBinary copies the minion executable out of a release archive (.tar.gz
// or .zip, chosen by the archive name) into dst, and returns its SHA-256. The
// archive is the already open file whose digest the caller verified: it is read
// through that handle and never reopened by path, so the bytes extracted are
// the bytes that were verified. Only the one regular file is read; entry names
// are never used as paths.
func extractBinary(archive *os.File, archiveName, goos, dst string) (sum string, err error) {
	want := BinaryName(goos)
	fi, err := archive.Stat()
	if err != nil {
		return "", err
	}
	size := fi.Size()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o700)
	if err != nil {
		return "", err
	}
	defer func() {
		if cerr := out.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			_ = os.Remove(dst)
		}
	}()
	if err := checkNewFile(out); err != nil {
		return "", err
	}
	h := sha256.New()
	w := io.MultiWriter(out, h)

	switch {
	case strings.HasSuffix(archiveName, ".tar.gz"):
		err = extractTarGz(io.NewSectionReader(archive, 0, size), want, w)
	case strings.HasSuffix(archiveName, ".zip"):
		err = extractZip(archive, size, want, w)
	default:
		err = fmt.Errorf("unsupported archive type %q", archiveName)
	}
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

var errNoBinary = errors.New("the archive does not contain the minion binary")

func copyLimited(w io.Writer, r io.Reader) error {
	n, err := io.Copy(w, io.LimitReader(r, maxBinarySize+1))
	if err != nil {
		return err
	}
	if n > maxBinarySize {
		return errors.New("the minion binary in the archive is too large")
	}
	if n == 0 {
		return errors.New("the minion binary in the archive is empty")
	}
	return nil
}

func extractTarGz(r io.Reader, want string, w io.Writer) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return fmt.Errorf("cannot read the archive: %w", err)
	}
	defer func() { _ = gz.Close() }()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return errNoBinary
		}
		if err != nil {
			return fmt.Errorf("cannot read the archive: %w", err)
		}
		if hdr.Typeflag == tar.TypeReg && path.Base(hdr.Name) == want {
			return copyLimited(w, tr)
		}
	}
}

func extractZip(r io.ReaderAt, size int64, want string, w io.Writer) error {
	zr, err := zip.NewReader(r, size)
	if err != nil {
		return fmt.Errorf("cannot read the archive: %w", err)
	}
	for _, zf := range zr.File {
		if zf.FileInfo().Mode().IsRegular() && path.Base(zf.Name) == want {
			rc, err := zf.Open()
			if err != nil {
				return fmt.Errorf("cannot read the archive: %w", err)
			}
			defer func() { _ = rc.Close() }()
			return copyLimited(w, rc)
		}
	}
	return errNoBinary
}
