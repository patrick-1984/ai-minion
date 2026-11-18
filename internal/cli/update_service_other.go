//go:build !windows

package cli

// defaultServiceUpdateGuard is nil outside Windows: there is no Windows
// service whose binary an update could miss.
var defaultServiceUpdateGuard func(exe string) (refuse, note string)
