//go:build windows

package config

// syncDirFn: NTFS makes the rename durable; a directory cannot be opened for
// syncing. Tests replace it.
var syncDirFn = func(string) error { return nil }
