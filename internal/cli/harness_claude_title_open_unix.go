//go:build unix

package cli

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/patrick-1984/ai-minion/internal/identity"
)

// openClaudeTranscript opens a Claude Code transcript for reading only when it
// is <profile>/projects/<project>/<session id>.jsonl beneath the exporter's
// profile directory. The directory is resolved once and held open: the
// manifest is verified through it (the same manifest the callback runs under,
// read without following a link, private and regular), and the transcript is
// opened relative to it, one component at a time without following a symbolic
// link (so neither a link nor a retargeted profile alias can lead elsewhere),
// non-blocking (a FIFO or a device in its place cannot hang the callback) and
// only when it is a regular file. It returns the file and its identity
// (device and inode).
func openClaudeTranscript(profile claudeProfile, path, session string) (*os.File, string, error) {
	project, err := claudeTranscriptProject(profile.dir, path, session)
	if err != nil || profile.manifest == nil {
		return nil, "", errClaudeTranscript
	}
	dir, err := unix.Open(profile.dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, "", err
	}
	if err := verifyClaudeManifestAt(dir, profile.manifest); err != nil {
		_ = unix.Close(dir)
		return nil, "", err
	}
	for _, name := range []string{"projects", project} {
		next, err := unix.Openat(dir, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		_ = unix.Close(dir)
		if err != nil {
			return nil, "", err
		}
		dir = next
	}
	f, st, err := openRegularAt(dir, filepath.Base(path))
	_ = unix.Close(dir)
	if err != nil {
		return nil, "", err
	}
	id := strconv.FormatUint(uint64(st.Dev), 10) + ":" + strconv.FormatUint(st.Ino, 10) //nolint:unconvert // Dev differs in width per platform
	return f, id, nil
}

// openRegularAt opens name in dir for reading without following a link,
// non-blocking, and only when it is a regular file.
func openRegularAt(dir int, name string) (*os.File, unix.Stat_t, error) {
	var st unix.Stat_t
	fd, err := unix.Openat(dir, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, st, err
	}
	if err := unix.Fstat(fd, &st); err != nil {
		_ = unix.Close(fd)
		return nil, st, err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG {
		_ = unix.Close(fd)
		return nil, st, errors.New("not a regular file")
	}
	return os.NewFile(uintptr(fd), name), st, nil
}

// verifyClaudeManifestAt checks that the directory dir holds the manifest
// want: private, regular, bounded and of the same grant and Worker state.
func verifyClaudeManifestAt(dir int, want *claudeUsageIntegration) error {
	f, st, err := openRegularAt(dir, claudeUsageManifest)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	if st.Size > maxClaudeSettings {
		return errors.New("the Claude Code usage manifest is too large")
	}
	if err := identity.CheckPrivateFile(f); err != nil {
		return errors.New("the Claude Code usage manifest must be private")
	}
	raw, err := io.ReadAll(io.LimitReader(f, maxClaudeSettings+1))
	if err != nil || len(raw) > maxClaudeSettings {
		return errors.New("the Claude Code usage manifest could not be read")
	}
	m, err := parseClaudeUsageManifest(raw)
	if err != nil || m.Grant != want.Grant || m.StateDir != want.StateDir {
		return errors.New("the Claude profile no longer holds this manifest")
	}
	return nil
}

// errClaudeTranscript refuses a transcript path that is not
// <profile>/projects/<project>/<session id>.jsonl.
var errClaudeTranscript = errors.New("not a transcript of this Claude profile")

// claudeTranscriptProject checks the shape of a transcript path beneath the
// profile directory (as configured or with its symbolic links resolved,
// whichever the path starts with) and returns the project directory name.
func claudeTranscriptProject(profile, path, session string) (string, error) {
	if profile == "" || !filepath.IsAbs(profile) || session == "" || strings.ContainsAny(session, `/\`) ||
		!filepath.IsAbs(path) || filepath.Clean(path) != path || filepath.Base(path) != session+".jsonl" {
		return "", errClaudeTranscript
	}
	projectDir := filepath.Dir(path)
	project, projects := filepath.Base(projectDir), filepath.Dir(projectDir)
	if project == "." || project == ".." || project == string(filepath.Separator) || filepath.Base(projects) != "projects" {
		return "", errClaudeTranscript
	}
	under := filepath.Dir(projects)
	for _, p := range []string{filepath.Clean(profile), resolvedDir(profile)} {
		if p != "" && p == under {
			return project, nil
		}
	}
	return "", errClaudeTranscript
}

func resolvedDir(dir string) string {
	r, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return ""
	}
	return r
}
