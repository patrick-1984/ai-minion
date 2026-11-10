//go:build !unix

package cli

import (
	"context"
	"fmt"
)

// lockFile is not available without advisory file locks. The server check
// needs $TMUX, which only exists where tmux runs (unix), so this is never
// reached on such systems; it refuses instead of probing unlocked.
func lockFile(context.Context, string) (func(), error) {
	return nil, fmt.Errorf("the tmux server check needs file locks, which this system does not provide")
}
