//go:build !linux && !darwin

package agentdetect

// ProcessBirth returns no identity on hosts lacking a process lifetime reader.
func ProcessBirth(_ int) string { return "" }
