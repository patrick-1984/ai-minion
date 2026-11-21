package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/worker/store"
	"github.com/spf13/cobra"
)

func newConfigUsageCmd(deps workerConfigDeps) *cobra.Command {
	var accept, asJSON bool
	var expectWorker, expectHub, expectOwner string
	cmd := &cobra.Command{Use: "usage", Short: "Review usage monitoring for this Worker and its pinned Hub", Long: ops.UsageDisclosure, Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		ce, err := deps.env(cmd)
		if err != nil {
			return err
		}
		defer ce.release()
		st, err := store.OpenContext(cmd.Context(), ce.stateDir)
		if err != nil {
			return err
		}
		defer func() { _ = st.Close() }()
		enrollment, err := st.LoadEnrollment(cmd.Context())
		if errors.Is(err, store.ErrNotEnrolled) {
			if asJSON && !accept {
				return json.NewEncoder(ce.out).Encode(map[string]any{"enrolled": false, "accepted": false, "required": false, "worker_id": "", "hub_fingerprint": "", "owner": ""})
			}
			return fmt.Errorf("enroll this Worker and connect to its Hub before accepting usage monitoring")
		}
		if err != nil {
			return err
		}
		fp, err := identity.FingerprintOf(enrollment.HubPub)
		if err != nil {
			return err
		}
		if accept && ((expectWorker != "" && expectWorker != enrollment.WorkerID) || (expectOwner != "" && expectOwner != enrollment.Owner) || (expectHub != "" && !strings.EqualFold(expectHub, fp.String()))) {
			return errors.New("the enrollment changed; review the current Hub, Worker and owner before accepting")
		}
		if asJSON {
			granted, err := st.UsageRegistration(cmd.Context(), enrollment.WorkerID)
			if err != nil {
				return err
			}
			if accept {
				if err := st.AcceptUsageRegistration(cmd.Context(), enrollment.HubPub, enrollment.WorkerID, enrollment.Owner); err != nil {
					return err
				}
				granted = true
			}
			required, err := st.UsageRequired(cmd.Context(), enrollment.WorkerID)
			if err != nil {
				return err
			}
			return json.NewEncoder(ce.out).Encode(map[string]any{"owner": enrollment.Owner, "enrolled": true, "accepted": granted, "required": required, "worker_id": enrollment.WorkerID, "hub_fingerprint": fp.String()})
		}
		if err := writef(ce.out, "%s\nHub fingerprint: %s\nWorker: %s\nOwner: %s\n", ops.UsageDisclosure, fp, enrollment.WorkerID, enrollment.Owner); err != nil {
			return err
		}
		if accept {
			if err := st.AcceptUsageRegistration(cmd.Context(), enrollment.HubPub, enrollment.WorkerID, enrollment.Owner); err != nil {
				return err
			}
			return writef(ce.out, "Usage monitoring accepted for this enrollment.\n")
		}
		granted, err := st.UsageRegistration(cmd.Context(), enrollment.WorkerID)
		if err != nil {
			return err
		}
		if granted {
			return writef(ce.out, "Usage monitoring is accepted for this enrollment.\n")
		}
		return writef(ce.out, "Usage monitoring is pending. To accept, run minion worker config usage --accept with the same state directory.\n")
	}}
	addNamedSettingFlags(cmd, deps.reg(), config.KeyStateDir)
	cmd.Flags().BoolVar(&accept, "accept", false, "Accept usage monitoring for the current Hub enrollment")
	cmd.Flags().StringVar(&expectWorker, "expect-worker-id", "", "Accept only this reviewed Worker id")
	cmd.Flags().StringVar(&expectHub, "expect-hub-fingerprint", "", "Accept only this reviewed Hub fingerprint")
	cmd.Flags().StringVar(&expectOwner, "expect-owner", "", "Accept only this reviewed owner")
	cmd.Flags().BoolVar(&asJSON, "json", false, "Print machine-readable usage registration status")
	cmd.AddCommand(newUsageRecordCmd(deps), newUsageIntegrateCmd(deps), newPrepareUsageCmd(deps))
	return cmd
}
