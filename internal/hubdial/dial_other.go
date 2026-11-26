//go:build !android

package hubdial

// Dial returns next unchanged (nil stays nil): the override exists only in
// the Android build.
func Dial(next DialFunc) DialFunc { return next }
