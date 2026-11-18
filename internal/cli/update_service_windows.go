//go:build windows

package cli

import "github.com/patrick-1984/ai-minion/internal/worker/service"

// defaultServiceUpdateGuard looks at the Worker's Windows service (query
// rights only) and applies [serviceUpdateGuard]. The protected binary is
// recognised by its path, also when the service cannot be queried.
func defaultServiceUpdateGuard(exe string) (refuse, note string) {
	st, err := service.QueryWindowsService()
	if err != nil {
		st = service.WindowsServiceState{}
	}
	protected, _ := service.ServiceBinaryLocation()
	return serviceUpdateGuard(st, exe, protected, service.IsElevated())
}
