package service

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// ErrBinaryDigest is returned when the bytes copied for the service do not
// hash to the verified digest.
var ErrBinaryDigest = errors.New("the service binary does not match the verified release")

// maxServiceBinarySize bounds the copy (the release binary is far smaller).
const maxServiceBinarySize = 512 << 20

// copyServiceBinary copies src next to dst (so the copy inherits the DACL of
// the directory) and renames it over dst. With wantSHA256 the bytes are hashed
// while they are copied, and a copy that does not match is discarded before
// dst is touched: src may be in a directory the user can change, and only the
// copied bytes count. A dst that still runs (a service that did not stop, a
// console Worker started from it) cannot be deleted on Windows but can be
// renamed: it is moved to dst.old first and removed when it no longer runs.
func copyServiceBinary(src, dst, wantSHA256 string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	if fi, err := in.Stat(); err != nil {
		return err
	} else if !fi.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", src)
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".minion-install-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	done := false
	defer func() {
		if !done {
			_ = tmp.Close()
			_ = os.Remove(tmpName)
		}
	}()
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(in, maxServiceBinarySize+1))
	if err != nil {
		return err
	}
	if n > maxServiceBinarySize {
		return fmt.Errorf("%s is too large for a minion binary", src)
	}
	if wantSHA256 != "" {
		if got := hex.EncodeToString(h.Sum(nil)); got != wantSHA256 {
			return fmt.Errorf("%w: %s has sha256 %s, the verified release has %s; nothing was installed", ErrBinaryDigest, src, got, wantSHA256)
		}
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	old := dst + ".old"
	if _, err := os.Lstat(dst); err == nil {
		if err := os.Remove(old); err != nil && !os.IsNotExist(err) {
			_ = os.Rename(old, fmt.Sprintf("%s-%d", old, time.Now().UnixNano()))
		}
		if err := os.Rename(dst, old); err != nil {
			return err
		}
	}
	if err := os.Rename(tmpName, dst); err != nil {
		_ = os.Rename(old, dst)
		return err
	}
	done = true
	_ = os.Remove(old) // fails while it still runs; a later install removes it
	return nil
}
