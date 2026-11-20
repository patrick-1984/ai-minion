//go:build !windows

package cli

import (
	"context"
	"io"
	"os"
	"os/exec"
	"os/user"
	"strconv"
	"syscall"

	"github.com/patrick-1984/ai-minion/internal/worker/service"
)

// runAsUser runs binary with args as the target user (root drops to the
// user's IDs), with a minimal environment of that user. Its output goes to out.
func runAsUser(ctx context.Context, t service.Target, binary string, args []string, out io.Writer) error {
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
	c.Dir = t.Home
	c.Stdout, c.Stderr = out, out
	return c.Run()
}
