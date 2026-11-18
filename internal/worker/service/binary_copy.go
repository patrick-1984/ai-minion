package service

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/patrick-1984/ai-minion/internal/version"
)

// ErrBinaryDigest is returned when the bytes copied for the service do not
// hash to the verified digest.
var ErrBinaryDigest = errors.New("the service binary does not match the verified release")

// maxServiceBinarySize bounds the copy (the release binary is far smaller).
const maxServiceBinarySize = 512 << 20

// copyServiceBinary copies src next to dst (so the copy inherits the DACL of
// the directory) and renames it over dst, under the lock that "minion update"
// and the Worker's remote update take for dst. With wantSHA256 the bytes are
// hashed while they are copied, and a copy that does not match is discarded
// before dst is touched: src may be in a directory the user can change, and
// only the copied bytes count. A dst that still runs (a service that did not
// stop, a console Worker started from it) cannot be deleted on Windows but
// can be renamed: it is moved to a scratch name first and removed when it no
// longer runs. dst.old, the previous release that the remote update keeps for
// its crash rollback, is never touched.
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
	dir := filepath.Dir(dst)
	release, err := version.LockBinary(dst)
	if err != nil {
		return err
	}
	defer release()
	removeReplaced(dir)
	tmp, err := os.CreateTemp(dir, ".minion-install-*")
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
	aside := ""
	if _, err := os.Lstat(dst); err == nil {
		aside = filepath.Join(dir, fmt.Sprintf("%s%d", replacedPrefix, time.Now().UnixNano()))
		if err := os.Rename(dst, aside); err != nil {
			return err
		}
	}
	if err := os.Rename(tmpName, dst); err != nil {
		if aside != "" {
			_ = os.Rename(aside, dst)
		}
		return err
	}
	done = true
	if aside != "" {
		_ = os.Remove(aside) // fails while it still runs; a later install removes it
	}
	return nil
}

// replacedPrefix names a replaced service binary that still ran.
const replacedPrefix = ".minion-replaced-"

// removeReplaced removes replaced binaries that no longer run.
func removeReplaced(dir string) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), replacedPrefix) {
			_ = os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}
