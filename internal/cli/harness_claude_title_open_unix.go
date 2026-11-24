//go:build unix

package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// openClaudeTranscript opens a Claude Code transcript for reading only when it
// is <profile>/projects/<project>/<session id>.jsonl beneath the verified
// profile directory, walking down from that directory one component at a time
// without following a symbolic link (so a link cannot lead out of the
// profile), and only when it is a regular file: it is opened non-blocking, so
// a FIFO or a device in its place cannot hang the callback. It returns the
// file and its identity (device and inode).
func openClaudeTranscript(profile, path, session string) (*os.File, string, error) {
	root, project, err := claudeTranscriptParts(profile, path, session)
	if err != nil {
		return nil, "", err
	}
	dir, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
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
	fd, err := unix.Openat(dir, filepath.Base(path), unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	_ = unix.Close(dir)
	if err != nil {
		return nil, "", err
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		_ = unix.Close(fd)
		return nil, "", err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG {
		_ = unix.Close(fd)
		return nil, "", errors.New("the transcript is not a regular file")
	}
	id := strconv.FormatUint(uint64(st.Dev), 10) + ":" + strconv.FormatUint(st.Ino, 10) //nolint:unconvert // Dev differs in width per platform
	return os.NewFile(uintptr(fd), path), id, nil
}

// errClaudeTranscript refuses a transcript path that is not
// <profile>/projects/<project>/<session id>.jsonl.
var errClaudeTranscript = errors.New("not a transcript of this Claude profile")

// claudeTranscriptParts checks the shape of a transcript path beneath the
// verified profile directory and returns that directory (as configured or
// with its symbolic links resolved, whichever the path starts with) and the
// project directory name.
func claudeTranscriptParts(profile, path, session string) (string, string, error) {
	if profile == "" || !filepath.IsAbs(profile) || session == "" || strings.ContainsAny(session, `/\`) ||
		!filepath.IsAbs(path) || filepath.Clean(path) != path || filepath.Base(path) != session+".jsonl" {
		return "", "", errClaudeTranscript
	}
	projectDir := filepath.Dir(path)
	project, projects := filepath.Base(projectDir), filepath.Dir(projectDir)
	if project == "." || project == ".." || project == string(filepath.Separator) || filepath.Base(projects) != "projects" {
		return "", "", errClaudeTranscript
	}
	under := filepath.Dir(projects)
	for _, p := range []string{filepath.Clean(profile), resolvedDir(profile)} {
		if p != "" && p == under {
			return p, project, nil
		}
	}
	return "", "", errClaudeTranscript
}

func resolvedDir(dir string) string {
	r, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return ""
	}
	return r
}
