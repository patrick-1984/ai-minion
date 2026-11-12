package hubid

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/patrick-1984/ai-minion/internal/identity"
)

func (s *Service) path(name string) string { return filepath.Join(s.dir, name) }

func (s *Service) exists(name string) bool {
	_, err := os.Lstat(s.path(name))
	return err == nil
}

func (s *Service) syncDir() error {
	if s.hooks.SyncDir != nil {
		return s.hooks.SyncDir(s.dir)
	}
	return syncDirOS(s.dir)
}

// syncAfterRename syncs the directory after a rename or an unlink (D6). The
// obligation is recorded before the attempt and cleared only by a success, so
// a failure leaves it pending for the next identity request.
func (s *Service) syncAfterRename() error {
	s.syncPending = true
	if err := s.syncDir(); err != nil {
		return err
	}
	s.syncPending = false
	return nil
}

func (s *Service) rename(from, to string) error {
	if s.hooks.Rename != nil {
		return s.hooks.Rename(s.path(from), s.path(to))
	}
	return os.Rename(s.path(from), s.path(to))
}

// writeFileDurable writes data to name by D1 (temporary file, sync, rename:
// identity.WritePrivate) and then syncs the directory (D2).
func (s *Service) writeFileDurable(name string, data []byte) error {
	if err := identity.WritePrivate(s.path(name), data); err != nil {
		return err
	}
	return s.syncDir()
}

// readSealed reads a sealed root file (at most maxFileRead bytes; a larger
// file is cut and so can never match a recorded hash).
func (s *Service) readFile(name string) ([]byte, error) {
	f, err := os.Open(s.path(name))
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return io.ReadAll(io.LimitReader(f, maxFileRead+1))
}

// fileSHA returns the SHA-256 of a sealed file, or nil when it is missing or
// unreadable.
func (s *Service) fileSHA(name string) []byte {
	b, err := s.readFile(name)
	if err != nil {
		return nil
	}
	return sha(b)
}

// remove deletes a file that never signed or served anything. After a
// simulated crash it does nothing.
func (s *Service) remove(name string) {
	if !s.alive() {
		return
	}
	if err := os.Remove(s.path(name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		s.log.Error("hub identity: could not remove a stale file", "file", s.path(name), "error", err)
	}
}

// shred overwrites a plain root file with zeros where the file system allows,
// unlinks it and syncs the directory (rule C; best effort on every file system).
func (s *Service) shred(name string) error {
	p := s.path(name)
	if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() {
		if f, err := os.OpenFile(p, os.O_WRONLY, 0); err == nil {
			_, _ = f.Write(make([]byte, fi.Size()))
			_ = f.Sync()
			_ = f.Close()
		}
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return s.syncAfterRename()
}

// keyPub reads a plain key file and returns its public key.
//
// The seed never lives in an identity.Key (whose closure cannot be wiped): it
// is read into a wipeable buffer, turned into a destroyable signer to derive
// the public key, and both are wiped and destroyed before this returns.
func (s *Service) keyPub(name string) ([]byte, error) {
	pub, err := identity.PlainKeyPublic(s.path(name), s.hooks.Signer)
	if err != nil {
		return nil, err
	}
	return pub, nil
}

func failClosed(format string, a ...any) error {
	return newErr(CodeFailClosed, fmt.Sprintf(format, a...))
}
