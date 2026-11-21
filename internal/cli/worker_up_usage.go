package cli

import (
	"errors"
	"fmt"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/worker/store"
	"github.com/spf13/cobra"
)

// prepareUpUsage honors only the flag on this invocation. It never becomes
// an environment or persistent worker-run setting.
func prepareUpUsage(cmd *cobra.Command, deps workerUpDeps, res *config.Resolver, stateDir string, owner *ownerSetup) error {
	yes, _ := cmd.Flags().GetBool("accept-usage")
	if !yes {
		return nil
	}
	hub, err := config.ResolveWorkerHub(res)
	if err != nil {
		return err
	}
	if hub.Server == "" {
		return errors.New("--accept-usage needs a Hub enrollment or pairing destination")
	}
	out := cmd.OutOrStdout()
	if _, err := fmt.Fprintln(out, ops.UsageDisclosure); err != nil {
		return err
	}
	if owner != nil {
		run := deps.setupDeps().runAsUser
		if run == nil {
			run = runAsUser
		}
		args := []string{"worker", "config", "usage", "prepare-enrollment", hub.Server, "--state-dir", owner.StateDir}
		if hub.Fingerprint != "" {
			args = append(args, "--expected-pin", hub.Fingerprint)
		}
		return run(cmdContext(cmd), owner.Target, owner.Binary, args, out)
	}
	s, err := store.OpenContext(cmdContext(cmd), stateDir)
	if err != nil {
		return err
	}
	defer func() { _ = s.Close() }()
	return s.PrepareUsageIntent(cmdContext(cmd), hub.Server, hub.Fingerprint)
}

func newPrepareUsageCmd(deps workerConfigDeps) *cobra.Command {
	var pin string
	cmd := &cobra.Command{Use: "prepare-enrollment <server>", Short: "Stage a one-time consent for this Hub enrollment", Hidden: true, Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		env, err := deps.env(cmd)
		if err != nil {
			return err
		}
		defer env.release()
		st, err := store.OpenContext(cmd.Context(), env.stateDir)
		if err != nil {
			return err
		}
		defer func() { _ = st.Close() }()
		return st.PrepareUsageIntent(cmd.Context(), args[0], pin)
	}}
	addNamedSettingFlags(cmd, deps.reg(), config.KeyStateDir)
	cmd.Flags().StringVar(&pin, "expected-pin", "", "Public fingerprint supplied for this Hub")
	return cmd
}
