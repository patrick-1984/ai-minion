package presence

import (
	"errors"
	"fmt"
	"io"
	"os"
	"time"
)

// renameFile, renameRetryable and renameSleep are the seams of
// [renameWithRetry]; tests replace them.
var (
	renameFile      = os.Rename
	renameRetryable = platformRenameRetryable
	renameSleep     = time.Sleep
)

// removeFile, removeRetryable and removeSleep are the seams of
// [removeWithRetry]; tests replace them.
var (
	removeFile      = os.Remove
	removeRetryable = platformRenameRetryable
	removeSleep     = time.Sleep
)

const (
	removeAttempts = 20
	removeDelay    = 25 * time.Millisecond
)

// removeWithRetry unlinks path. A missing file is success. On Windows a reader
// that holds the file open without FILE_SHARE_DELETE makes the unlink fail with
// a sharing violation; it is retried with a bounded backoff (about a second).
// A caller that still gets a retryable error keeps the removal pending.
func removeWithRetry(path string) error {
	var err error
	delay := removeDelay
	for i := 0; i < removeAttempts; i++ {
		err = removeFile(path)
		if err == nil || isNotExist(err) {
			return nil
		}
		if !removeRetryable(err) {
			return err
		}
		removeSleep(delay)
		if delay < 8*removeDelay {
			delay *= 2
		}
	}
	return fmt.Errorf("remove %s: %w after %d attempts: %w", path, errStillInUse, removeAttempts, err)
}

const (
	renameAttempts = 40
	renameDelay    = 25 * time.Millisecond
)

// renameWithRetry renames old over dst. On Windows a reader that holds dst
// open (without FILE_SHARE_DELETE) makes the rename fail with a sharing
// violation; the rename is retried, bounded to about one second, while the
// error is of that kind. On Unix it never retries. gate (when not nil) is
// asked immediately before every attempt, so that a condition that ended while
// a reader held dst stops the publication ([errGateClosed]).
func renameWithRetry(old, dst string, gate func() bool) error {
	var err error
	for i := 0; i < renameAttempts; i++ {
		if gate != nil && !gate() {
			return errGateClosed
		}
		if err = renameFile(old, dst); err == nil {
			return nil
		}
		if !renameRetryable(err) {
			return err
		}
		renameSleep(renameDelay)
	}
	return fmt.Errorf("replace %s: still in use after %d attempts: %w", dst, renameAttempts, err)
}

// readBounded reads at most max bytes from f and fails when there is more.
func readBounded(f io.Reader, max int64) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, fmt.Errorf("larger than %d bytes", max)
	}
	return b, nil
}

func isNotExist(err error) bool { return errors.Is(err, os.ErrNotExist) }

// errGateClosed: the caller's condition no longer held just before the rename.
var errGateClosed = errors.New("write abandoned")

// errStillInUse marks a removal that a reader still blocks.
var errStillInUse = errors.New("still in use")

var errFuture = errors.New("the record is dated in the future")
