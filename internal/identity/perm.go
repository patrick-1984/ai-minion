package identity

import (
	"os"
	"path/filepath"
)

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
func WritePrivate(path string, data []byte) error {
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
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	syncDir(dir)
	return nil
}
