package version

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// lockStale is when an install lock left by a crashed update is taken over.
const lockStale = 30 * time.Minute

// ErrInstallBusy reports another update of the same binary in progress.
var ErrInstallBusy = errors.New("another minion update of this binary is in progress")

// installLocked runs install under one lock per binary, shared by
// "minion update" and the Worker's remote update. Under the lock it reads the
// version of the binary that is installed now: when another update installed
// the target or a newer release meanwhile, nothing is replaced (no stale
// downgrade). A binary that cannot report its version is replaced as before.
func installLocked(o Options, target SemVer, install func() error) error {
	real, err := filepath.EvalSymlinks(o.ExecPath)
	if err != nil {
		real = o.ExecPath
	}
	lock := real + ".update-lock"
	if err := takeLock(lock); err != nil {
		return err
	}
	defer func() { _ = os.Remove(lock) }()
	if inst, ok := installedVersion(real); ok && inst.Compare(target) >= 0 && !o.AllowDowngrade {
		if cur, err := ParseVersion(o.Current); err != nil || inst.Compare(cur) != 0 {
			return fmt.Errorf("%s is already installed at %s (another update ran meanwhile); nothing was replaced", inst, real)
		}
	}
	return install()
}

func takeLock(p string) error {
	for i := 0; i < 2; i++ {
		f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			_, _ = fmt.Fprintf(f, "%d\n", os.Getpid())
			return f.Close()
		}
		if !os.IsExist(err) {
			return fmt.Errorf("cannot lock %s: %w", p, err)
		}
		if fi, serr := os.Stat(p); serr == nil && time.Since(fi.ModTime()) > lockStale {
			_ = os.Remove(p)
			continue
		}
		return ErrInstallBusy
	}
	return ErrInstallBusy
}

var versionLineRE = regexp.MustCompile(`(?m)^version: (v\S+)$`)

// installedVersion runs "<exe> version" and parses its "version:" line.
func installedVersion(exe string) (SemVer, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, exe, "version").Output()
	if err != nil {
		return SemVer{}, false
	}
	m := versionLineRE.FindStringSubmatch(strings.ReplaceAll(string(out), "\r", ""))
	if m == nil {
		return SemVer{}, false
	}
	v, err := ParseVersion(m[1])
	return v, err == nil
}
