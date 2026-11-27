//go:build !unix

package agent

import (
	"errors"
	"os"
	"time"

	"github.com/patrick-1984/ai-minion/internal/filexfer"
)

func identityOf(os.FileInfo) (filexfer.Identity, bool) { return filexfer.Identity{}, false }

// fxStat is the stub of the Unix entry facts: no file transfer end exists on
// this platform in v1 (files_root_unknown), so nothing calls it.
type fxStat struct {
	size  int64
	mtime time.Time
	nlink uint64
	dev   uint64
	ino   uint64
}

func (s fxStat) isDir() bool     { return false }
func (s fxStat) isReg() bool     { return false }
func (s fxStat) isSymlink() bool { return false }

var errNoFiles = errors.New("file transfer is not available on this platform")

func lstatAt(*os.File, string) (fxStat, error) { return fxStat{}, errNoFiles }

func freeSpace(int) (int64, int64, error) { return 0, 0, errNoFiles }
