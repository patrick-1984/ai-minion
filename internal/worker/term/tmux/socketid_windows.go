//go:build windows

package tmux

import "errors"

// socketIdentity is not available for psmux, which cannot keep layouts.
func socketIdentity(string) (string, error) {
	return "", errors.New("no socket identity on Windows")
}
