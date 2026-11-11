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
