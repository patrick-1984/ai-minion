package identity

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// BackupSuffix is appended to a file's name for the copy of the bytes
// WriteReplacing displaced.
const BackupSuffix = ".minion-backup"

// ErrChangedDuringWrite is returned by WriteReplacing when the file changed
// between the moment it was read and the moment it would be replaced. Nothing
// was written.
var ErrChangedDuringWrite = errors.New("the file changed while it was being replaced")

// ConflictError reports that WriteReplacing replaced a file and then found
// content other than its own: a concurrent writer saved in between. Nothing
// is restored automatically; Backup holds the exact bytes that were replaced.
type ConflictError struct{ Path, Backup string }

func (e *ConflictError) Error() string {
	return fmt.Sprintf("%s was changed by another program while it was being replaced; a copy of the replaced content is at %s", e.Path, e.Backup)
}

// maxReplacedFile bounds what WriteReplacing copies into its backup.
const maxReplacedFile = 8 << 20

func readRegularBounded(path string) ([]byte, bool, error) {
	f, err := openNoBlock(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxReplacedFile {
		return nil, false, errors.New("expected a regular, bounded file")
	}
	raw, err := io.ReadAll(io.LimitReader(f, maxReplacedFile+1))
	if err != nil || len(raw) > maxReplacedFile {
		return nil, false, errors.New("the file could not be read within its size limit")
	}
	return raw, true, nil
}

// WriteReplacing is WritePrivateIf for a file another program also writes (a
// user's settings): before the file is replaced, an exact copy of its bytes is
// kept in path+BackupSuffix (private, replacing a previous backup). After the
// replacement the file is read again; when it is not what was written, a
// *ConflictError names the backup. Nothing is restored automatically.
func WriteReplacing(path string, data []byte, check func() error) error {
	return writePrivate(path, data, check, true)
}

// WritePrivate writes data to path atomically and leaves the file readable by
// its owner only. It is the one helper for identity keys and Worker state
// files (Story 2.3). The data is written to a temporary file in the same
// directory, synced, then renamed over path, so a reader sees either the old
// or the new content.
//
// The temporary file is private from the moment it exists, before any secret
// byte is written: on Unix os.CreateTemp creates it with mode 0600; on Windows
// it is created with a protected owner-and-SYSTEM-only DACL supplied at
// creation (createPrivateTemp), so there is no window in which another
// principal could open a handle under the directory's inherited ACL.
func WritePrivate(path string, data []byte) error { return WritePrivateIf(path, data, nil) }

// WritePrivateIf is WritePrivate that calls check after the new content is
// staged and synced, immediately before it replaces path. A check error
// abandons the write and leaves path untouched; it narrows the window in
// which a concurrent writer of the same file could be overwritten.
func WritePrivateIf(path string, data []byte, check func() error) error {
	return writePrivate(path, data, check, false)
}

func writePrivate(path string, data []byte, check func() error, keepBackup bool) error {
	dir := filepath.Dir(path)
	tmp, err := createPrivateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := func(err error) error {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		return cleanup(err)
	}
	if err := tmp.Sync(); err != nil {
		return cleanup(err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	var displaced []byte
	var existed bool
	if keepBackup {
		if displaced, existed, err = readRegularBounded(path); err != nil {
			_ = os.Remove(tmpName)
			return err
		}
		if existed {
			if err := WritePrivate(path+BackupSuffix, displaced); err != nil {
				_ = os.Remove(tmpName)
				return fmt.Errorf("keep a copy of %s: %w", path, err)
			}
		}
	}
	if check != nil {
		if err := check(); err != nil {
			_ = os.Remove(tmpName)
			return err
		}
	}
	if keepBackup {
		// The copy must be of the bytes that are about to be replaced.
		again, ok, err := readRegularBounded(path)
		if err != nil || ok != existed || !bytes.Equal(again, displaced) {
			_ = os.Remove(tmpName)
			return ErrChangedDuringWrite
		}
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	syncDir(dir)
	if keepBackup {
		if now, ok, err := readRegularBounded(path); err != nil || !ok || !bytes.Equal(now, data) {
			return &ConflictError{Path: path, Backup: path + BackupSuffix}
		}
	}
	return nil
}

// OpenPrivateAppend opens path for appending and creates it private (the user
// and SYSTEM on Windows, mode 0600 elsewhere) when it does not exist. An
// existing file that is not private (see [CheckPrivateFile]) is removed and
// created again, so that a log never ends up readable by another account
// through an old, permissive ACL. The caller closes the file.
func OpenPrivateAppend(path string) (*os.File, error) {
	for range 2 {
		f, err := openAppendPrivate(path)
		if err != nil {
			return nil, err
		}
		cerr := CheckPrivateFile(f)
		if cerr == nil {
			return f, nil
		}
		_ = f.Close()
		if err := os.Remove(path); err != nil {
			return nil, fmt.Errorf("%s is not private (%v) and cannot be replaced: %w", path, cerr, err)
		}
	}
	return nil, fmt.Errorf("cannot create %s as a private file", path)
}
