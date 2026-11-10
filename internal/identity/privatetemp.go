package identity

import "os"

// CreatePrivateTemp creates a new temporary file in dir (pattern as in
// os.CreateTemp) that is private from the moment it exists: mode 0600 on Unix,
// a protected owner-and-SYSTEM-only DACL supplied at creation on Windows. It is
// the exported form of the helper [WritePrivate] uses, for a caller that has to
// stage a secret file in two steps (write, then publish) instead of in one.
func CreatePrivateTemp(dir, pattern string) (*os.File, error) {
	return createPrivateTemp(dir, pattern)
}
