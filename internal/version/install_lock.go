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
	release, err := takeLock(real + ".update-lock")
	if err != nil {
		return err
	}
	defer release()
	// The installed binary is run only after the same trust checks the
	// replacement applies: nobody else may have planted it.
	if err := CheckTrustedExecutable(real); err != nil {
		return fmt.Errorf("refusing to update %s: %w", real, err)
	}
	if inst, ok := installedVersion(real); ok && inst.Compare(target) >= 0 && !o.AllowDowngrade {
		if cur, err := ParseVersion(o.Current); err != nil || inst.Compare(cur) != 0 {
			return fmt.Errorf("%s is already installed at %s (another update ran meanwhile); nothing was replaced", inst, real)
		}
	}
	return install()
}

// takeLock takes an exclusive operating-system lock (flock, LockFileEx) on p
// without waiting. The kernel drops it when the process ends, so a crashed
// update leaves no stale lock; the file itself stays.
func takeLock(p string) (func(), error) {
	f, err := os.OpenFile(p, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("cannot lock %s: %w", p, err)
	}
	if err := installLockNB(f); err != nil {
		_ = f.Close()
		if errors.Is(err, errInstallLockHeld) {
			return nil, ErrInstallBusy
		}
		return nil, fmt.Errorf("cannot lock %s: %w", p, err)
	}
	return func() { _ = installUnlock(f); _ = f.Close() }, nil
}

// errInstallLockHeld is what the platform lock code returns when another
// process holds the lock.
var errInstallLockHeld = errors.New("install lock held")

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
