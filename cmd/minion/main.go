// Command minion is the single binary of Minion Hub and Minion Worker. It
// contains only main; all code lives in internal/ (AD-1).
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/patrick-1984/ai-minion/internal/cli"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := cli.Run(ctx, os.Args, os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}
