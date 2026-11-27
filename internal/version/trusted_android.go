//go:build android

package version

import (
	"os"
	"syscall"
)

func platformTrustedUID(uid uint32) bool { return androidTrustedOwner(uid) }

func platformTrustedGroupWrite(fi os.FileInfo) bool {
	st, ok := fi.Sys().(*syscall.Stat_t)
	return ok && androidTrustedGroupWrite(st.Uid, st.Gid, fi.Mode().Perm())
}
