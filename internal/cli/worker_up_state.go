package cli

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/worker/service"
)

// maxConfigBytes is the size limit of the Worker config file (config.LoadFile
// enforces the same limit on the copy it reads).
const maxConfigBytes = 1 << 20

// upState is the Worker state directory of "worker up".
//
// When the command runs as root for another user, that user controls the
// directory tree: they can replace any directory of it, or of the path to it,
// by a link to another account between two steps. The state is then opened
// once through [service.OpenTrustedDir], the whole ancestry checked, and every
// later step works through that handle (config file, ownership). The config
// file is edited in a private scratch copy, because [config.ConfigFile] works
// on paths, and written back through the handle. Otherwise the user is the
// caller and the state is plain paths.
type upState struct {
	dir  string
	root *os.Root // nil unless root works for another user
	t    service.Target
	ch   service.Chowner
	tmp  string // scratch directory of the config copy
}

func openUpState(dir string, t service.Target, owned bool, deps workerUpDeps) (*upState, error) {
	st := &upState{dir: dir, t: t}
	if !owned {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("create the state directory: %w", err)
		}
		return st, nil
	}
	st.ch = deps.chown
	if st.ch == nil {
		st.ch = nopChowner{}
	}
	root, err := service.OpenTrustedDir(service.DirSpec{
		Path: dir, Create: true, Mode: 0o700, UID: t.UID, GID: t.GID,
		Chown: st.ch, LinkOwner: deps.service.LinkOwner,
	})
	if err != nil {
		return nil, fmt.Errorf("open the state directory: %w", err)
	}
	st.root = root
	return st, nil
}

func (s *upState) close() {
	if s.root != nil {
		_ = s.root.Close()
	}
	if s.tmp != "" {
		_ = os.RemoveAll(s.tmp)
	}
}

func (s *upState) configPath() string { return filepath.Join(s.dir, workerConfigName) }

// load reads the config file of the state.
func (s *upState) load() (*config.ConfigFile, error) {
	if s.root == nil {
		return config.LoadFile(s.configPath())
	}
	scratch, err := s.scratch()
	if err != nil {
		return nil, err
	}
	copyPath := filepath.Join(scratch, workerConfigName)
	if err := os.Remove(copyPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	info, err := s.root.Lstat(workerConfigName)
	if errors.Is(err, fs.ErrNotExist) {
		return config.LoadFile(copyPath)
	}
	if err != nil {
		return nil, fmt.Errorf("config file: %w", err)
	}
	if info.Mode()&fs.ModeSymlink != 0 {
		return nil, fmt.Errorf("%s is a symbolic link; refusing to follow it while running as root", s.configPath())
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("config file %q is not a regular file", s.configPath())
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("config file %q has mode %04o; it may hold secrets, run: chmod 600 %s",
			s.configPath(), info.Mode().Perm(), s.configPath())
	}
	src, err := s.root.Open(workerConfigName)
	if err != nil {
		return nil, fmt.Errorf("config file: %w", err)
	}
	defer func() { _ = src.Close() }()
	dst, err := os.OpenFile(copyPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	_, err = io.Copy(dst, io.LimitReader(src, maxConfigBytes+1))
	if cerr := dst.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return nil, fmt.Errorf("config file: %w", err)
	}
	f, err := config.LoadFile(copyPath)
	if err != nil {
		return nil, fmt.Errorf("%w (copy of %s)", err, s.configPath())
	}
	return f, nil
}

// save writes file, which load returned, back to the state and returns the
// path of the saved config file.
func (s *upState) save(file *config.ConfigFile) (string, error) {
	if err := file.Save(); err != nil {
		return "", fmt.Errorf("save the settings: %w", err)
	}
	if s.root == nil {
		return file.Path(), nil
	}
	data, err := os.ReadFile(file.Path())
	if err != nil {
		return "", fmt.Errorf("save the settings: %w", err)
	}
	if err := service.WriteFileIn(s.root, workerConfigName, string(data), config.FileMode, s.t.UID, s.t.GID, s.ch); err != nil {
		return "", fmt.Errorf("save the settings: %w", err)
	}
	return s.configPath(), nil
}

// give gives the state directory and everything in it to the target user.
func (s *upState) give() error {
	if s.root == nil {
		return nil
	}
	if err := service.ChownTree(s.root, s.t.UID, s.t.GID, s.ch); err != nil {
		return fmt.Errorf("give the state directory to %s: %w", s.t.User, err)
	}
	return nil
}

func (s *upState) scratch() (string, error) {
	if s.tmp == "" {
		d, err := os.MkdirTemp("", "minion-up-")
		if err != nil {
			return "", err
		}
		s.tmp = d
	}
	return s.tmp, nil
}

// nopChowner changes nothing.
type nopChowner struct{}

func (nopChowner) ChownDir(*os.File, int, int) error       { return nil }
func (nopChowner) Lchown(*os.Root, string, int, int) error { return nil }
