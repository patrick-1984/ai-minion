//go:build !windows

package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/worker/agent"
)

// The owner runs the enrollment hook, so SQLite, its auxiliary files and
// the private key are opened with the same credentials as the future Worker.
// Stdout carries one request byte only if the hook actually needs a key;
// human output goes to stderr. No token is in the child's argv or environment.
func runOwnerHooks(ctx context.Context, owner *ownerSetup, s agent.Settings, in io.Reader, out io.Writer) error {
	hub, err := config.ResolveWorkerHub(s.Resolver)
	if err != nil {
		return err
	}
	given, _, err := config.InviteKeyGiven(s.Resolver)
	if err != nil {
		return err
	}
	keyArg := ""
	if given {
		keyArg = "-"
	}
	args := []string{"worker", "pre-start-owner", "--state-dir", s.StateDir, "--name", s.Name,
		"--server", hub.Server, "--hub-fingerprint", hub.Fingerprint, "--invite-key", keyArg}
	cmd := commandAsUser(ctx, owner.Target, owner.Binary, args)
	request, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	defer request.Close()
	input, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	defer input.Close()
	cmd.Stderr = out
	if err := cmd.Start(); err != nil {
		return err
	}
	var signal [1]byte
	_, readErr := io.ReadFull(request, signal[:])
	switch {
	case readErr == nil && signal[0] == '?' && given:
		var key config.InviteKeyInput
		key, err = config.ResolveWorkerInviteKeyContext(ctx, s.Resolver, in)
		if err == nil {
			_, err = fmt.Fprintln(input, key.Value)
		}
	case readErr != nil && !errors.Is(readErr, io.EOF):
		err = readErr
	case readErr == nil:
		err = errors.New("unexpected request from the enrollment helper")
	}
	_ = input.Close()
	waitErr := cmd.Wait()
	if err != nil {
		return err
	}
	if waitErr != nil {
		return fmt.Errorf("pre-start hooks as %s failed: %w; no service was installed or changed", owner.Target.User, waitErr)
	}
	return nil
}

// requestInvite notifies the parent on the first stdin read. The hook checks
// saved enrollment, pause and forgotten pins before it ever makes that read.
type requestInvite struct{ requested bool }

func (r *requestInvite) Read(p []byte) (int, error) {
	if !r.requested {
		if _, err := os.Stdout.Write([]byte{'?'}); err != nil {
			return 0, err
		}
		r.requested = true
	}
	return os.Stdin.Read(p)
}

func init() {
	Register(func(root *cobra.Command) {
		cmd := &cobra.Command{Use: "pre-start-owner", Hidden: true, Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
			res := &config.Resolver{Role: config.ScopeWorker, Flags: changedFlags{cmd: cmd, reg: config.Default}, LookupEnv: os.LookupEnv}
			file, err := loadConfigFile(res, loadWorkerConfig)
			if err != nil {
				return err
			}
			res.File = file
			core, err := config.ResolveWorkerCore(res)
			if err != nil {
				return err
			}
			logger, err := commandLogger(cmd, res, cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			s := agent.Settings{WorkerCore: core, Resolver: res, InviteStdin: &requestInvite{}, Logger: logger}
			return runUpHooks(cmdContext(cmd), workerUpDeps{}, s, cmd.ErrOrStderr())
		}}
		addNamedSettingFlags(cmd, config.Default, config.KeyStateDir, config.KeyName, config.KeyServer, config.KeyHubFingerprint, config.KeyInviteKey)
		workerCommand(root).AddCommand(cmd)
	})
}
