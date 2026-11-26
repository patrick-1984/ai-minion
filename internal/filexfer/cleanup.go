package filexfer

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/patrick-1984/ai-minion/internal/proto"
)

// Cleanup of staging files (BRD 5.8): a Stager removes its staged files on
// Abort; files left by a crash are found through the staging log, a list of
// the directories where the Worker staged, kept in its state directory, and
// removed at start-up.

// CleanStaleParts removes, in the directory dir, the staging files of
// transferID (any age; "" for none) and every staging file whose change time
// (set when it was staged; its mtime is the sender's) is older than maxAge.
// Only regular files whose name starts with proto.StagingPrefix are removed.
// It returns the number of removed files.
func CleanStaleParts(dir *os.File, transferID string, maxAge time.Duration, now time.Time) (int, error) {
	if !sysSupported {
		return 0, errUnsupported()
	}
	if _, err := dir.Seek(0, 0); err != nil {
		return 0, internal("a staging folder cannot be listed")
	}
	removed := 0
	for {
		names, err := dir.Readdirnames(1024)
		for _, name := range names {
			if !strings.HasPrefix(name, proto.StagingPrefix) {
				continue
			}
			mine := transferID != "" && strings.HasPrefix(name, proto.StagingPrefix+transferID+"-")
			si, serr := statAt(dir, name)
			if serr != nil || si.Type != typeFile {
				continue
			}
			if !mine && now.Sub(si.CTime) < maxAge {
				continue
			}
			if withFd(dir, func(fd int) error { return sysUnlinkAt(fd, name) }) == nil {
				removed++
			}
		}
		if err != nil || len(names) == 0 {
			break
		}
	}
	return removed, nil
}

// StagingRecord is one entry of the staging log: an absolute directory where
// a transfer staged files, its identity and the transfer.
type StagingRecord struct {
	Dir        string    `json:"dir"`
	Dev        uint64    `json:"dev"`
	Ino        uint64    `json:"ino"`
	TransferID string    `json:"transfer_id"`
	At         time.Time `json:"at"`
}

// maxStagingRecords bounds the log; the oldest records go first.
const maxStagingRecords = 1000

// StagingLog is the list of staging directories in the Worker's state
// directory (file "file-staging.json", mode 0600). Paths in it never leave
// the Worker.
type StagingLog struct {
	path string
	mu   sync.Mutex
}

// NewStagingLog returns the log in stateDir.
func NewStagingLog(stateDir string) *StagingLog {
	return &StagingLog{path: filepath.Join(stateDir, "file-staging.json")}
}

func (l *StagingLog) load() ([]StagingRecord, error) {
	b, err := os.ReadFile(l.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var recs []StagingRecord
	if err := json.Unmarshal(b, &recs); err != nil {
		return nil, nil // a damaged log is replaced
	}
	return recs, nil
}

func (l *StagingLog) save(recs []StagingRecord) error {
	if len(recs) > maxStagingRecords {
		recs = recs[len(recs)-maxStagingRecords:]
	}
	b, err := json.Marshal(recs)
	if err != nil {
		return err
	}
	tmp := l.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, l.path)
}

// Add records a staging directory before the first file is staged there.
func (l *StagingLog) Add(rec StagingRecord) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	recs, err := l.load()
	if err != nil {
		return err
	}
	return l.save(append(recs, rec))
}

// Done drops the records of a transfer whose stager published or aborted.
func (l *StagingLog) Done(transferID string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	recs, err := l.load()
	if err != nil {
		return err
	}
	out := recs[:0]
	for _, r := range recs {
		if r.TransferID != transferID {
			out = append(out, r)
		}
	}
	return l.save(out)
}

// Sweep runs at start-up, before any transfer: for every record it opens the
// directory (one component at a time, no symbolic link), checks that it is
// still the recorded directory (device and inode), removes the record's
// staging files and every staging file older than maxAge there, and drops
// the record. It returns the number of removed files.
func (l *StagingLog) Sweep(maxAge time.Duration, now time.Time) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	recs, err := l.load()
	if err != nil {
		return 0, err
	}
	removed := 0
	for _, r := range recs {
		dir, err := sysOpenRootDir(r.Dir)
		if err != nil {
			continue
		}
		st, err := fstatFile(dir)
		if err == nil && st.Type == typeDir && st.Dev == r.Dev && st.Ino == r.Ino {
			n, _ := CleanStaleParts(dir, r.TransferID, maxAge, now)
			removed += n
		}
		_ = dir.Close()
	}
	return removed, l.save(nil)
}

// openAbsNoFollow opens an absolute path one component at a time with
// openat, which the platform files supply without following symbolic links.
func openAbsNoFollow(abs string, openat func(dirfd int, name string) (int, error), openRoot func() (int, error), closeFd func(int) error) (*os.File, error) {
	if !filepath.IsAbs(abs) || strings.Contains(abs, "\x00") {
		return nil, internal("the staging folder is not an absolute path")
	}
	fd, err := openRoot()
	if err != nil {
		return nil, err
	}
	for _, name := range strings.Split(strings.Trim(filepath.ToSlash(abs), "/"), "/") {
		if name == "" || name == "." {
			continue
		}
		if name == ".." {
			_ = closeFd(fd)
			return nil, internal("the staging folder path has a parent element")
		}
		next, err := openat(fd, name)
		_ = closeFd(fd)
		if err != nil {
			return nil, err
		}
		fd = next
	}
	return os.NewFile(uintptr(fd), abs), nil
}
