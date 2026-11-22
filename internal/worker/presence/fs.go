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

const (
	renameAttempts = 40
	renameDelay    = 25 * time.Millisecond
)

// renameWithRetry renames old over dst. On Windows a reader that holds dst
// open (without FILE_SHARE_DELETE) makes the rename fail with a sharing
// violation; the rename is retried, bounded to about one second, while the
// error is of that kind. On Unix it never retries.
func renameWithRetry(old, dst string) error {
	var err error
	for i := 0; i < renameAttempts; i++ {
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

var errFuture = errors.New("the record is dated in the future")
