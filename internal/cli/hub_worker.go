package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/patrick-1984/ai-minion/internal/ops"
)

func init() {
	Register(func(root *cobra.Command) {
		hub := hubCommand(root)
		worker := &cobra.Command{Use: "worker", Short: "Manage the Workers of a Hub"}
		hub.AddCommand(worker)
		worker.AddCommand(newHubWorkerApprove(defaultOpsDeps()))
		worker.AddCommand(newHubWorkerRepairPairing(defaultOpsDeps()))
	})
}

// newHubWorkerApprove returns `minion hub worker approve <fingerprint|id>`
// (F-02): a new Worker waits for approval, and this approves it. The Hub is
// reached like every Hub operation (MINION_HUB_URL and MINION_API_KEY_FILE, or
// the --hub-* flags). A Worker id is approved directly (manage or operate on
// the Worker); a fingerprint needs a workers_list lookup first (also read).
func newHubWorkerApprove(deps opsDeps) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "approve <fingerprint|id>",
		Short: "Approve a pending Worker",
		Long: `Approve a Worker that enrolled and waits for approval.

A Worker that enrolls (invitation key, pairing code or infrastructure key) is
pending: it may connect, but the Hub sends it no operations and refuses its
calls until its owner or an admin approves it. Name the Worker by the
fingerprint of its key (52 lowercase base32 characters, shown in the Hub and
in the Worker's log) or by its id (a UUID). Compare the fingerprint, name and
source address printed by "minion workers_list" or the Hub's Workers page with
what you expect before you approve.

Permissions. Approving by id sends only the approval, so it needs the manage
(or operate) action on that Worker and nothing else: an admin API key with
manage alone is enough. Approving by fingerprint first looks the Worker up with
"workers_list", so that form also needs the read action; the key must be able
to read the Worker. Automation that knows the id of the Worker it started
should use the id.`,
		Args:          cobra.ExactArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runHubWorkerApprove(cmd, deps, args[0])
		},
	}
	cmd.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return &usageError{err} })
	addHubFlags(cmd.Flags())
	return cmd
}

func runHubWorkerApprove(cmd *cobra.Command, deps opsDeps, ref string) error {
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	ref = strings.TrimSpace(ref)
	byID := ops.ValidUUIDv7(ref)
	if !byID && !validFingerprintArg(ref) {
		return usagef("%q is neither a Worker id (a UUID) nor a fingerprint (52 lowercase base32 characters)", ref)
	}
	approve, ok := ops.Lookup(ops.OpWorkerApprove)
	if !ok {
		return fmt.Errorf("the catalog has no %s", ops.OpWorkerApprove)
	}
	backend, _, err := pickBackend(cmd, deps, approve)
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	if byID {
		// The id names the Worker: approve it directly. No list lookup, so a
		// key with manage but without read can do it.
		args := ops.WorkerApproveArgs{WriteArgs: ops.WriteArgs{RequestID: deps.newID()}, WorkerID: ref}
		body, _ := json.Marshal(args)
		raw, err := backend.Call(ctx, ops.OpWorkerApprove, json.RawMessage(body))
		if err != nil {
			return failure(cmd, printer{w: out}, approve, args.RequestID, err)
		}
		var res ops.WriteResult
		if err := json.Unmarshal(raw, &res); err != nil || res.Outcome != ops.OutcomeDelivered {
			return fmt.Errorf("the Hub did not confirm the approval (outcome %q); list the Workers to see the current state", res.Outcome)
		}
		_, err = fmt.Fprintf(out, "Approved Worker %s.\n", ref)
		return err
	}
	raw, err := backend.Call(ctx, ops.OpWorkersList, json.RawMessage(`{"all":true}`))
	if err != nil {
		return fmt.Errorf("list the Workers (approving by fingerprint needs the read action; approving by id does not): %w", err)
	}
	var list ops.WorkersListResult
	if err := json.Unmarshal(raw, &list); err != nil {
		return fmt.Errorf("read the Workers list: %w", err)
	}
	var found *ops.WorkerInfo
	for i := range list.Workers {
		w := &list.Workers[i]
		if w.Fingerprint == ref {
			found = w
			break
		}
	}
	if found == nil {
		return fmt.Errorf("no Worker with that fingerprint is visible to this key")
	}
	if !found.Pending {
		_, err := fmt.Fprintf(out, "Worker %s (%s) is already approved.\n", found.Name, found.ID)
		return err
	}
	args := ops.WorkerApproveArgs{
		WriteArgs: ops.WriteArgs{RequestID: deps.newID()}, WorkerID: found.ID, Fingerprint: found.Fingerprint,
	}
	body, _ := json.Marshal(args)
	raw, err = backend.Call(ctx, ops.OpWorkerApprove, json.RawMessage(body))
	if err != nil {
		return failure(cmd, printer{w: out}, approve, args.RequestID, err)
	}
	var res ops.WriteResult
	if err := json.Unmarshal(raw, &res); err != nil || res.Outcome != ops.OutcomeDelivered {
		return fmt.Errorf("the Hub did not confirm the approval (outcome %q); list the Workers to see the current state", res.Outcome)
	}
	_, err = fmt.Fprintf(out, "Approved Worker %s (%s), fingerprint %s, enrolled from %s.\n",
		found.Name, found.ID, found.Fingerprint, orDash(found.EnrollAddr))
	return err
}

func validFingerprintArg(s string) bool {
	if len(s) != 52 {
		return false
	}
	for _, r := range s {
		if (r < 'a' || r > 'z') && (r < '2' || r > '7') {
			return false
		}
	}
	return true
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// newHubWorkerRepairPairing returns `minion hub worker repair-pairing
// <fingerprint|id>` (design hub-identity-v2 12.3): the operation
// worker_pairing_reissue for a Worker whose pairing the Hub committed but the
// Worker never recorded, because its code expired before it could resume.
func newHubWorkerRepairPairing(deps opsDeps) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "repair-pairing <fingerprint|id>",
		Short: "Re-issue pairing for a Worker that never connected",
		Long: `Re-issue pairing for a Worker that never connected.

When the pairing code expired after the Hub accepted it but before the Worker
recorded the result, the Hub knows the Worker and the Worker knows no Hub. This
opens a 10-minute window in which the next new pairing code of that Worker,
entered by its owner or an administrator under Add worker, completes the
pairing for the same Worker (same id, name and approval state). Then run
"minion worker pair" on the Worker, or wait for its new code.

Only the Worker's owner and administrators may do this, and only for a Worker
that has never connected to the Hub; any other Worker, or one that was removed,
answers conflict. Name the Worker by its id (a UUID) or by the fingerprint of
its key (that form first lists the Workers, so it also needs the read action).`,
		Args:          cobra.ExactArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runHubWorkerRepairPairing(cmd, deps, args[0])
		},
	}
	cmd.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return &usageError{err} })
	addHubFlags(cmd.Flags())
	return cmd
}

func runHubWorkerRepairPairing(cmd *cobra.Command, deps opsDeps, ref string) error {
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	ref = strings.TrimSpace(ref)
	byID := ops.ValidUUIDv7(ref)
	if !byID && !validFingerprintArg(ref) {
		return usagef("%q is neither a Worker id (a UUID) nor a fingerprint (52 lowercase base32 characters)", ref)
	}
	op, ok := ops.Lookup(ops.OpWorkerPairingReissue)
	if !ok {
		return fmt.Errorf("the catalog has no %s", ops.OpWorkerPairingReissue)
	}
	backend, _, err := pickBackend(cmd, deps, op)
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	id := ref
	if !byID {
		raw, err := backend.Call(ctx, ops.OpWorkersList, json.RawMessage(`{"all":true}`))
		if err != nil {
			return fmt.Errorf("list the Workers (naming a Worker by fingerprint needs the read action; by id it does not): %w", err)
		}
		var list ops.WorkersListResult
		if err := json.Unmarshal(raw, &list); err != nil {
			return fmt.Errorf("read the Workers list: %w", err)
		}
		id = ""
		for _, w := range list.Workers {
			if w.Fingerprint == ref {
				id = w.ID
				break
			}
		}
		if id == "" {
			return fmt.Errorf("no Worker with that fingerprint is visible to this key")
		}
	}
	args := ops.WorkerPairingReissueArgs{WriteArgs: ops.WriteArgs{RequestID: deps.newID()}, WorkerID: id}
	body, _ := json.Marshal(args)
	raw, err := backend.Call(ctx, ops.OpWorkerPairingReissue, json.RawMessage(body))
	if err != nil {
		return failure(cmd, printer{w: out}, op, args.RequestID, err)
	}
	var res ops.WriteResult
	if err := json.Unmarshal(raw, &res); err != nil || res.Outcome != ops.OutcomeDelivered {
		return fmt.Errorf("the Hub did not confirm the re-issue (outcome %q)", res.Outcome)
	}
	_, err = fmt.Fprintf(out, "Pairing re-issued for Worker %s for 10 minutes. Run \"minion worker pair\" on the Worker (or wait for its new code) and enter the new code in the Hub (Add worker).\n", id)
	return err
}
