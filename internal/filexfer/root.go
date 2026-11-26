// Package filexfer is the transfer engine of files.v1 (BRD minion-file-transfer
// v2): confinement (this file, root_*, resolve_*, identity.go, probe.go) and
// formats and commit (path, walk, pack, unpack, stage, publish).
//
// Confinement in one paragraph: a transfer end never works with absolute
// paths. It opens a Root, a pinned and verified directory handle of the
// pane's folder (OpenPaneRoot), of the process's current folder
// (OpenCwdRoot) or of a given folder (OpenDirRoot), and every later access is
// relative to that handle: Root.Open, Root.OpenParent and Root.OpenDir resolve
// a checked wire path beneath it without following any symlink and without
// crossing a mount, on Linux and Android with openat2(RESOLVE_BENEATH |
// RESOLVE_NO_SYMLINKS | RESOLVE_NO_XDEV | RESOLVE_NO_MAGICLINKS) or a
// per-component walk with mount-id checks, on macOS with a per-component
// O_NOFOLLOW_ANY walk with device checks. Opening a root also checks it against
// the Floor (system folders, home, the Worker state directory and the
// sensitive folders, all by identity) and walks its ancestry to find
// sensitive and push-protected parents. Windows has no transfer end in v1:
// every root opener answers files_root_unknown.
package filexfer

import (
	"errors"
	"io/fs"
	"sync/atomic"

	"github.com/patrick-1984/ai-minion/internal/proto"
)

// ResolveMode selects how paths are resolved beneath a root.
type ResolveMode uint8

const (
	// ResolveDefault uses the process default (SetDefaultResolveMode, set
	// from the probe result by ApplyProbe).
	ResolveDefault ResolveMode = iota
	// ResolveOpenat2 resolves with one openat2 call (Linux 5.6 and later).
	ResolveOpenat2
	// ResolveWalk resolves one component at a time from held directory
	// handles with O_NOFOLLOW and a mount check per component (Linux with
	// statx STATX_MNT_ID, macOS with st_dev and O_NOFOLLOW_ANY).
	ResolveWalk
	// ResolveUnavailable means no safe resolution is available: every root
	// opener answers files_root_unknown.
	ResolveUnavailable
)

// String names the mode as the probe reports it.
func (m ResolveMode) String() string {
	switch m {
	case ResolveDefault:
		return "default"
	case ResolveOpenat2:
		return "openat2"
	case ResolveWalk:
		return "walk"
	case ResolveUnavailable:
		return "unavailable"
	}
	return "invalid"
}

// defaultMode is the process default; see SetDefaultResolveMode.
var defaultMode atomic.Uint32

func init() { defaultMode.Store(uint32(unprobedMode)) }

// SetDefaultResolveMode sets the mode that roots opened with ResolveDefault
// use. A Worker sets it once from its probe (ApplyProbe). Before that the
// default is ResolveWalk on macOS and ResolveUnavailable on Linux and Android
// (openat2 and statx are not called in the process before the probe proved
// that they do not kill it).
func SetDefaultResolveMode(m ResolveMode) {
	if m == ResolveDefault {
		m = unprobedMode
	}
	defaultMode.Store(uint32(m))
}

// DefaultResolveMode returns the process default mode.
func DefaultResolveMode() ResolveMode { return ResolveMode(defaultMode.Load()) }

func effectiveMode(m ResolveMode) ResolveMode {
	if m == ResolveDefault {
		return DefaultResolveMode()
	}
	return m
}

// RootOptions are the options of the root openers.
type RootOptions struct {
	// Floor is the identity floor the root is checked against. Nil means no
	// floor check, which only tests may use; every Worker and CLI end passes
	// its floor.
	Floor *Floor
	// Mode is the resolution mode; ResolveDefault uses the process default.
	Mode ResolveMode
	// Confine, when set, is a directory the root must lie in: a pane folder
	// at or below Confine is the root, any other pane folder gives Confine
	// itself as the root (the Android rule, BRD 5.4). ConfineLabel is the
	// display name of Confine ("Minion files").
	Confine      string
	ConfineLabel string
}

// Refusal is a confinement refusal: a proto code (files_root_unknown,
// files_root_refused, path_outside_root, file_type_refused, path_protected,
// path_sensitive), the skip reason of one entry when it concerns one entry
// (proto.Skip*), a message that names no absolute path, and the cause.
type Refusal struct {
	Code proto.Code
	Skip string
	Msg  string
	Err  error
}

func (e *Refusal) Error() string {
	if e.Msg == "" {
		return string(e.Code)
	}
	return e.Msg
}

// Unwrap returns the cause (an errno, or nil).
func (e *Refusal) Unwrap() error { return e.Err }

// Proto returns the wire error of the refusal.
func (e *Refusal) Proto() *proto.Error { return proto.NewError(e.Code, e.Msg) }

func refuse(code proto.Code, skip, msg string, cause error) *Refusal {
	return &Refusal{Code: code, Skip: skip, Msg: msg, Err: cause}
}

func rootUnknown(msg string, cause error) *Refusal {
	return refuse(proto.CodeFilesRootUnknown, "", msg, cause)
}

func rootRefused(msg string) *Refusal {
	return refuse(proto.CodeFilesRootRefused, "", msg, nil)
}

// ProtoError maps an error of this package to its wire error: a Refusal to its
// code, a *proto.Error to itself, a missing entry to not_found, an existing
// entry to conflict, anything else to internal. The message never names a
// path.
func ProtoError(err error) *proto.Error {
	if err == nil {
		return nil
	}
	var r *Refusal
	if errors.As(err, &r) {
		return r.Proto()
	}
	var pe *proto.Error
	if errors.As(err, &pe) {
		return pe
	}
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return proto.NewError(proto.CodeNotFound, "no such file or folder in the folder of the session")
	case errors.Is(err, fs.ErrExist):
		return proto.NewError(proto.CodeConflict, "the destination exists")
	}
	return proto.NewError(proto.CodeInternal, "file system error")
}

// Root is a pinned, verified directory handle: the root of one end of a
// transfer. It stays valid when the folder is renamed or the pane changes
// directory. A Root is safe for concurrent use; Close it when the transfer
// ends.
type Root struct {
	fd            int
	id            Identity
	mnt           uint64 // Linux mount id, 0 when unknown
	mode          ResolveMode
	display       string
	pushProtected bool
	floor         *Floor
	closed        atomic.Bool
}

// Identity returns the root directory's identity.
func (r *Root) Identity() Identity { return r.id }

// Display returns the display root: "~/..." under home, the confine label
// ("Minion files/...") under Confine, the absolute path elsewhere. It is for
// results and audit; it is never used to open anything.
func (r *Root) Display() string { return r.display }

// Mode returns the resolution mode of the root (never ResolveDefault).
func (r *Root) Mode() ResolveMode { return r.mode }

// Floor returns the floor the root was checked against (nil in tests).
func (r *Root) Floor() *Floor { return r.floor }

// PushProtected reports whether the root lies inside a push-protected folder
// (".git/hooks", ".claude", ".github/workflows", ... or the Worker state
// directory, BRD FR-3): nothing may be pushed into it.
func (r *Root) PushProtected() bool { return r.pushProtected }

// CheckPush returns a path_protected refusal when the root is push-protected.
func (r *Root) CheckPush() error {
	if r.pushProtected {
		return refuse(proto.CodePathProtected, proto.SkipProtected,
			"the folder of the session is inside a protected folder; nothing can be pushed into it", nil)
	}
	return nil
}

// Fd returns the root's directory descriptor for *at system calls of the
// publish step. It is owned by the Root: do not close it, and do not use it
// after Close. On Linux it is an O_PATH descriptor.
func (r *Root) Fd() int { return r.fd }
