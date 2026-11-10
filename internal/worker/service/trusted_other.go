//go:build !(linux || darwin || freebsd)

package service

import (
	"fmt"
	"os"
	"path/filepath"
)

// walkTrusted is the portable walk for platforms without descriptor-relative
// system calls here. "worker up" changes ownership only as root on Unix, so
// this path serves Windows, where every symbolic link in the path is refused
// and nothing is given to another user.
func walkTrusted(spec DirSpec) (*os.File, error) {
	vol := filepath.VolumeName(spec.Path)
	p := vol + string(filepath.Separator)
	for _, part := range splitPath(spec.Path[len(vol):]) {
		p = filepath.Join(p, part)
		st, err := os.Lstat(p)
		if isNotExist(err) && spec.Create {
			if err = os.Mkdir(p, spec.Mode); err != nil {
				return nil, err
			}
			if st, err = os.Lstat(p); err != nil {
				return nil, err
			}
		}
		if err != nil {
			return nil, err
		}
		if st.Mode()&os.ModeSymlink != 0 {
			return nil, errLink(p)
		}
		if !st.IsDir() {
			return nil, errNotDirectory(p)
		}
	}
	f, err := os.Open(spec.Path)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", spec.Path, err)
	}
	return f, nil
}
