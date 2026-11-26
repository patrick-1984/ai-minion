//go:build !linux && !darwin

package filexfer

import (
	"io"
	"os"
)

// Windows (and every other OS) has no transfer end in v1 (BRD 3.2, 5.3): the
// pane folder cannot be proven, so every root opener answers
// files_root_unknown and the probe reports ResolveUnavailable.

const unprobedMode = ResolveUnavailable

var (
	defaultSystemFolders     []string
	defaultSystemDeepFolders []string
)

func identityOfInfo(os.FileInfo) (Identity, bool) { return Identity{}, false }

// OpenPaneRoot answers files_root_unknown on this OS.
func OpenPaneRoot(int, string, RootOptions) (*Root, error) { return nil, errUnsupported() }

// OpenCwdRoot answers files_root_unknown on this OS.
func OpenCwdRoot(RootOptions) (*Root, error) { return nil, errUnsupported() }

// OpenDirRoot answers files_root_unknown on this OS.
func OpenDirRoot(string, RootOptions) (*Root, error) { return nil, errUnsupported() }

// The root policy is shared with the Unix builds; this build only refuses.
var (
	_ = (*Floor).checkRoot
	_ = effectiveMode
)

// Close marks the root closed; this OS never opens one.
func (r *Root) Close() error {
	if r != nil {
		r.closed.Store(true)
		r.mnt = 0
	}
	return nil
}

// Open answers files_root_unknown on this OS.
func (r *Root) Open(string, int, uint32) (*os.File, error) { return nil, errUnsupported() }

// OpenDir answers files_root_unknown on this OS.
func (r *Root) OpenDir(string) (*os.File, error) { return nil, errUnsupported() }

// OpenFile answers files_root_unknown on this OS.
func (r *Root) OpenFile(string) (*os.File, error) { return nil, errUnsupported() }

// OpenParent answers files_root_unknown on this OS.
func (r *Root) OpenParent(string) (*os.File, string, error) { return nil, "", errUnsupported() }

// Encloses answers files_root_unknown on this OS.
func (r *Root) Encloses(*os.File) (bool, error) { return false, errUnsupported() }

func probeHost() (ProbeResult, bool) {
	return ProbeResult{Mode: ResolveUnavailable, Reason: "file transfer is not available on this OS in this version"}, true
}

func probeChildChecks(w io.Writer) { _, _ = io.WriteString(w, "unsupported\n") }
