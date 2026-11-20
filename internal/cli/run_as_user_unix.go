//go:build !windows

package cli

import (
	"context"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"syscall"

	"github.com/patrick-1984/ai-minion/internal/worker/service"
)

// runAsUser runs binary with args as the target user (root drops to the
// user's IDs), with a minimal environment of that user. Its output goes to out.
func runAsUser(ctx context.Context, t service.Target, binary string, args []string, out io.Writer) error {
	c := commandAsUser(ctx, t, binary, args)
	c.Stdout, c.Stderr = out, out
	return c.Run()
}

func commandAsUser(ctx context.Context, t service.Target, binary string, args []string) *exec.Cmd {
	c := exec.CommandContext(ctx, binary, args...)
	var groups []uint32
	if u, err := user.LookupId(strconv.Itoa(t.UID)); err == nil {
		if ids, err := u.GroupIds(); err == nil {
			for _, g := range ids {
				if n, err := strconv.Atoi(g); err == nil {
					groups = append(groups, uint32(n))
				}
			}
		}
	}
	if os.Geteuid() == 0 && t.UID >= 0 {
		c.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: uint32(t.UID), Gid: uint32(t.GID), Groups: groups}}
	}
	c.Env = []string{"HOME=" + t.Home, "USER=" + t.User, "LOGNAME=" + t.User, "PATH=" + os.Getenv("PATH")}
	// The user's login Worker may be on the systemd runtime socket, while
	// their system service uses the fallback socket in /tmp. Try both.
	if runtime.GOOS == "linux" {
		dir := filepath.Join("/run/user", strconv.Itoa(t.UID))
		if info, err := os.Stat(dir); err == nil && info.IsDir() {
			c.Env = append(c.Env, "XDG_RUNTIME_DIR="+dir)
		}
	}
	c.Dir = t.Home
	return c
}
