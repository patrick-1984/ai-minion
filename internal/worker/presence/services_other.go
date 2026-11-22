//go:build !linux

package presence

func serviceStateDirs(string, int) []string { return nil }
