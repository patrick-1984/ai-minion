package version

import (
	"os"
	"testing"
)

func TestAndroidSystemDirectoriesAreTrusted(t *testing.T) {
	if !androidTrustedOwner(1000) || androidTrustedOwner(1001) || androidTrustedOwner(10010) {
		t.Fatal("only the Android system user is a trusted platform owner")
	}
	cases := []struct {
		uid, gid uint32
		perm     uint32
		want     bool
	}{
		{1000, 1000, 0o771, true},  // /data, /data/user, /data/data
		{1000, 1000, 0o773, false}, // writable by others
		{1000, 1001, 0o771, false}, // another group can write
		{1001, 1000, 0o771, false}, // another owner
	}
	for _, c := range cases {
		if got := androidTrustedGroupWrite(c.uid, c.gid, osMode(c.perm)); got != c.want {
			t.Errorf("uid %d gid %d mode %04o = %v, want %v", c.uid, c.gid, c.perm, got, c.want)
		}
	}
}

func osMode(p uint32) os.FileMode { return os.FileMode(p) }
