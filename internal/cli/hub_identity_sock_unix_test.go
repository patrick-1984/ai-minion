//go:build unix

package cli

import (
	"io/fs"
	"testing"
)

// F5: root may use the socket of the Hub account; the owner must still be the
// Hub's uid and the socket must not be group- or other-writable.
func TestValidateSocketOwner(t *testing.T) {
	sock := fs.ModeSocket | 0o600
	cases := []struct {
		name               string
		mode               fs.FileMode
		sockUID, hub, euid int
		ok                 bool
	}{
		{"the Hub account itself", sock, 1000, 1000, 1000, true},
		{"root uses the socket of the Hub account", sock, 1000, 1000, 0, true},
		{"root Hub, root user", sock, 0, 0, 0, true},
		{"another user", sock, 1000, 1000, 1001, false},
		{"socket owned by a stranger, caller root", sock, 1001, 1000, 0, false},
		{"socket owned by root in a Hub account directory", sock, 0, 1000, 0, false},
		{"group-writable", fs.ModeSocket | 0o620, 1000, 1000, 0, false},
		{"other-writable", fs.ModeSocket | 0o602, 1000, 1000, 1000, false},
		{"not a socket", 0o600, 1000, 1000, 1000, false},
	}
	for _, c := range cases {
		if err := validateSocketOwner(c.mode, c.sockUID, c.hub, c.euid); (err == nil) != c.ok {
			t.Errorf("%s: err = %v, want ok=%v", c.name, err, c.ok)
		}
	}
}
