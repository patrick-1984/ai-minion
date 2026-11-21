package config

import "strings"

// pendingJoinKey is transaction metadata in config.json. It is deliberately
// outside the settings registry: a flag or the environment must not set it.
const pendingJoinKey = "_pending_join"

// SetPendingJoin tracks a configure-only join until its Hub settings are saved.
// Only the public Hub URL and fingerprint are kept here.
func (f *ConfigFile) SetPendingJoin(server, fingerprint string) {
	f.Set(pendingJoinKey, fingerprint+"\n"+server)
}

// ClearPendingJoin cancels recovery and reports whether there was a marker.
func (f *ConfigFile) ClearPendingJoin() bool {
	_, set, _ := f.Lookup(pendingJoinKey)
	f.Delete(pendingJoinKey)
	return set
}

// PendingJoin reads the join marker when no server was explicitly set. An
// explicitly empty server keeps a Worker in Solo mode.
func PendingJoin(res *Resolver) (server, fingerprint string, ok bool) {
	if res == nil || res.File == nil {
		return "", "", false
	}
	set, err := res.Resolve(KeyServer)
	if err != nil || set.Source != SourceDefault {
		return "", "", false
	}
	marker, ok, err := res.File.Lookup(pendingJoinKey)
	if !ok || err != nil {
		return "", "", false
	}
	fingerprint, server, ok = strings.Cut(marker, "\n")
	return server, fingerprint, ok && server != "" && fingerprint != ""
}
