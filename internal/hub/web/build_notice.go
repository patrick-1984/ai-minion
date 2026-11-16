package web

import (
	"sync/atomic"

	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/version"
)

// BuildView is the version block at the bottom of the left rail: the Hub's
// release for everyone and, for administrators, "vX → vY" when the daily
// check found a newer release in the channel.
type BuildView struct {
	Version string
	Update  *UpdateNotice
}

// UpdateNotice is the second line of the version block.
type UpdateNotice struct{ From, To, URL string }

// UpdateSource returns the newest release in the channel, its page and
// whether it is newer than the running Hub. It never blocks on the network.
type UpdateSource func() (latest, url string, newer bool)

var updateSource atomic.Pointer[UpdateSource]

// SetUpdateSource installs the release check (nil removes it).
func SetUpdateSource(f UpdateSource) {
	if f == nil {
		updateSource.Store(nil)
		return
	}
	updateSource.Store(&f)
}

// buildView is the version block for a role.
func buildView(role authz.Role) BuildView {
	v := BuildView{Version: version.Version}
	if version.Version == "dev" {
		v.Version = "dev (" + version.Commit + ")"
	}
	if role != authz.RoleAdmin {
		return v
	}
	if p := updateSource.Load(); p != nil {
		if latest, url, newer := (*p)(); newer {
			v.Update = &UpdateNotice{From: version.Version, To: latest, URL: url}
		}
	}
	return v
}
