//go:build !unix

package hubid

// syncDirOS: Windows has no directory sync; replace-rename is durable enough
// for the documented Windows preview.
func syncDirOS(string) error { return nil }
