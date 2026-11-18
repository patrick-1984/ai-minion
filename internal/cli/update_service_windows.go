//go:build windows

package cli

import "github.com/patrick-1984/ai-minion/internal/worker/service"

// defaultServiceUpdateGuard looks at the Worker's Windows service (query
// rights only) and applies [serviceUpdateGuard]. A service that cannot be
// queried is treated as absent: the update itself still fails on a binary
// this process cannot write.
func defaultServiceUpdateGuard(exe string) (refuse, note string) {
	st, err := service.QueryWindowsService()
	if err != nil {
		return "", ""
	}
	return serviceUpdateGuard(st, exe, service.IsElevated())
}
