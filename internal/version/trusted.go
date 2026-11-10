package version

import (
	"errors"
	"fmt"
)

// The update cache is public data (release archives, signed metadata) whose
// authenticity comes from signatures, so its content needs no secrecy. What it
// needs is that nobody who is less trusted than the user running the update can
// replace a file or a directory between the moment the updater verified it and
// the moment it is used. The trust model is therefore about who can write or
// rename: the cache root and every directory on the way to it must be owned by
// root (administrative accounts on Windows) or by the user that runs the
// update, and no one else may be able to add, remove or rename entries (see
// checkTrustedDir). The files in it are opened once, without following links,
// and checked on the open handle (see openTrusted). A different principal that
// shares the same account is trusted by design and out of scope.
//
// On Windows the objects the updater creates (staging directory, archive,
// executable, temporary copies) inherit their access rights from the directory
// they are created in, so the trusted-directory check also covers the ACEs
// that are inherit-only on that directory, and every new file is checked on its
// handle before it receives bytes.
//
// The shared kernel rule (AD-1) keeps this package from importing
// internal/identity, whose CheckPrivateDir has the same Unix algorithm, so the
// small platform part lives here (trusted_unix.go, trusted_windows.go).

// ErrUntrustedCache means the update cache directory, or a file in it, could
// be replaced by a user that is less trusted than the one running the update.
// Nothing in it is used and nothing is changed.
var ErrUntrustedCache = errors.New("the update cache is not safe to use")

func errUntrusted(msg string) error { return fmt.Errorf("%w: %s", ErrUntrustedCache, msg) }

const untrustedGuidance = "Use a directory that only you (or root) can write, below directories that only you or root can write: " +
	"set MINION_UPDATE_CACHE_DIR or --update-cache-dir to one, for example a directory under your home with mode 0700"
