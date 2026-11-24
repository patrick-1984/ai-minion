//go:build !unix

package cli

import (
	"errors"
	"os"
)

// openClaudeTranscript is not supported here: the harness pane binding the
// session name needs is Unix-only, so no transcript is ever opened.
func openClaudeTranscript(_, _, _ string) (*os.File, string, error) {
	return nil, "", errors.New("transcripts are not read on this platform")
}
