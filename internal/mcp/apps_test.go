package mcp

import (
	"testing"

	"github.com/patrick-1984/ai-minion/internal/ops"
)

// TestAppInstallToolIsAWriteTool: app_install is a tool only on a server that
// allows writes (and then the allow gate decides each call, like every write
// tool); app_install_status is a read tool.
func TestAppInstallToolIsAWriteTool(t *testing.T) {
	has := func(list []tool, name string) (tool, bool) {
		for _, tl := range list {
			if tl.op.Name == name {
				return tl, true
			}
		}
		return tool{}, false
	}
	ro, err := buildTools(DefaultOps(), false)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := has(ro, ops.OpAppInstall); ok {
		t.Fatal("the read-only server offers app_install")
	}
	if st, ok := has(ro, ops.OpAppInstallStatus); !ok || !st.def.Annotations.ReadOnlyHint {
		t.Fatal("app_install_status is not a read tool")
	}
	rw, err := buildTools(DefaultOps(), true)
	if err != nil {
		t.Fatal(err)
	}
	in, ok := has(rw, ops.OpAppInstall)
	if !ok || in.def.Annotations.ReadOnlyHint || !in.op.ChangesState() {
		t.Fatal("app_install is not a write tool with writes allowed")
	}
}
