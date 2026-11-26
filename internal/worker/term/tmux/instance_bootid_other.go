//go:build !linux

package tmux

// readBootID returns "" on a system without a kernel boot id file: its tmux
// sessions carry no instance id until a boot identity is measured for it (K1).
func readBootID() string { return "" }
