//go:build !linux

package cli

import (
	"context"
	"errors"
	"io"
)

func installLoginWorker(context.Context, string, string, io.Writer) error {
	return errors.New("shell login startup is supported on Linux; use minion worker up --foreground")
}

func migrateLoginStartup(context.Context, string) (loginMigration, error) {
	return loginMigration{}, nil
}

func startLoginWorker(context.Context) (bool, error) { return false, errNotInstalled }

func markLoginStopped() error { return nil }
func clearLoginStopped()      {}
func loginStopped() bool      { return false }
