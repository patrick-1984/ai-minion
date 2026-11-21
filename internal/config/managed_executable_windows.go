package config

// Windows uses its own terminal host and does not execute a tmux override.
func managedExecutable(path string) (string, error) { return path, nil }
