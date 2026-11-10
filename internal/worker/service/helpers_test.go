package service

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func contains(s, sub string) bool { return strings.Contains(s, sub) }

// recChowner records the full path of everything it is asked to give away and
// changes nothing. When uid is not negative it checks the new owner.
type recChowner struct {
	t        *testing.T
	uid, gid int
	got      []string
	// onLchown, when set, runs after a name was recorded.
	onLchown func(root *os.Root, name string)
}

func (r *recChowner) check(p string, uid, gid int) {
	r.t.Helper()
	if r.uid >= 0 && (uid != r.uid || gid != r.gid) {
		r.t.Errorf("chown %s to %d:%d, want %d:%d", p, uid, gid, r.uid, r.gid)
	}
}

func (r *recChowner) ChownDir(d *os.File, uid, gid int) error {
	r.check(d.Name(), uid, gid)
	r.got = append(r.got, d.Name())
	return nil
}

func (r *recChowner) Lchown(root *os.Root, name string, uid, gid int) error {
	p := filepath.Join(root.Name(), name)
	r.check(p, uid, gid)
	r.got = append(r.got, p)
	if r.onLchown != nil {
		r.onLchown(root, name)
	}
	return nil
}
