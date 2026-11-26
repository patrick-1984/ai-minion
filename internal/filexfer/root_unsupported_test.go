//go:build !linux && !darwin

package filexfer

import (
	"context"
	"errors"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/proto"
)

func TestUnsupportedRefuses(t *testing.T) {
	check := func(name string, err error) {
		t.Helper()
		var r *Refusal
		if !errors.As(err, &r) || r.Code != proto.CodeFilesRootUnknown {
			t.Errorf("%s: %v, want files_root_unknown", name, err)
		}
	}
	_, err := OpenPaneRoot(1, `C:\x`, RootOptions{})
	check("OpenPaneRoot", err)
	_, err = OpenCwdRoot(RootOptions{})
	check("OpenCwdRoot", err)
	_, err = OpenDirRoot(`C:\x`, RootOptions{})
	check("OpenDirRoot", err)
	if r := Probe(context.Background(), ProbeCommand{Path: "x"}, ""); r.Mode != ResolveUnavailable || r.Reason == "" {
		t.Errorf("probe = %+v", r)
	}
}
