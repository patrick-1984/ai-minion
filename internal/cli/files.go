package cli

// `minion files ls|pull|push` (files.v1, BRD FR-20, WP8). These replace the
// generic derived commands of file_list, file_pull and file_push (which stay
// as hidden aliases of the same name), because a transfer is more than one
// request: the bytes move on a data leg after the admission.
//
// Two caller ends:
//   - without a Hub configured the local Worker is the caller end: the
//     command must run inside a session of that Worker, whose folder is the
//     local root; the Worker moves the bytes and reports progress;
//   - with a Hub configured (MINION_HUB_URL and MINION_API_KEY_FILE) this
//     process is the caller end: its current folder is the local root
//     (filexfer.OpenCwdRoot with the identity floor, so a CLI started in the
//     home folder is refused) and the data leg goes to the Hub's endpoint
//     with the API key, which must name file_read (pull, ls) or file_write
//     (push).
//
// Exit status: 0 delivered, 3 failed_not_sent, 4 partial or unknown, 1 error,
// 2 usage. Local refusals (a path outside the folder, the folder refused)
// happen before anything is sent.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"runtime"
	"time"

	"github.com/spf13/cobra"

	"github.com/patrick-1984/ai-minion/internal/client"
	"github.com/patrick-1984/ai-minion/internal/filexfer"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/worker/agent"
)

func init() {
	Register(func(root *cobra.Command) {
		addFilesCommands(root, defaultOpsDeps())
	})
}

// replacedByFiles reports whether the generic derived command of op is
// replaced by the files commands. file_receipt keeps a hidden derived
// command: only a receiving end sends it.
func replacedByFiles(op string) bool { return ops.IsFileOp(op) }

// aliasShort is the help line of a command: the catalog summary for the
// hidden alias of the op's own name.
func aliasShort(use, own string) string {
	if op, ok := ops.Lookup(use); ok {
		return op.Summary
	}
	return own
}

func addFilesCommands(root *cobra.Command, deps opsDeps) {
	files := &cobra.Command{
		Use:   "files",
		Short: "List, pull and push files of a session's folder on another Worker",
		Long: "List a session's folder and copy files or folders between the folder of this session (or, with a Hub configured, the current folder) and a session's folder on another Worker.\n\n" +
			"Paths are relative to the folders; nothing outside them is read or written. Protected names (.git/hooks, .ssh, agent configuration, ...) are never written and sensitive names (.ssh, keys, ...) never sent.\n" +
			"Exit status: 0 delivered, 3 failed_not_sent, 4 partial or unknown, 1 error, 2 usage.",
		Args:          noArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	files.AddCommand(newFilesLs(deps, "ls", false), newFilesPull(deps, "pull", false), newFilesPush(deps, "push", false))
	root.AddCommand(files)
	// The catalog names stay as hidden aliases (minion file_pull ...).
	root.AddCommand(newFilesLs(deps, ops.OpFileList, true), newFilesPull(deps, ops.OpFilePull, true), newFilesPush(deps, ops.OpFilePush, true))
}

func filesCommonFlags(cmd *cobra.Command) {
	cmd.Flags().String("target", "", "the session as worker:session (required; the Worker id or name and the session name)")
	cmd.Flags().Bool(flagJSON, false, "print the result as JSON (one line)")
	addHubFlags(cmd.Flags())
}

// filesTarget reads --target: required and naming a Worker.
func filesTarget(cmd *cobra.Command) (ops.Target, error) {
	v, _ := cmd.Flags().GetString("target")
	if v == "" {
		return ops.Target{}, usagef("--target worker:session is required")
	}
	t, err := ParseTargetFlag(v)
	if err != nil {
		return ops.Target{}, usagef("--target: %v", err)
	}
	if t.WorkerID == "" {
		return ops.Target{}, usagef("--target must name the Worker (worker:session): a transfer copies between two Workers")
	}
	return t, nil
}

// pathArg returns the path from the argument or the --path flag.
func pathArg(cmd *cobra.Command, args []string, required bool) (string, error) {
	p, _ := cmd.Flags().GetString("path")
	if len(args) > 1 {
		return "", usagef("only one path is taken")
	}
	if len(args) == 1 {
		if p != "" && p != args[0] {
			return "", usagef("give the path once (argument or --path)")
		}
		p = args[0]
	}
	if p == "" && required {
		return "", usagef("a path is required")
	}
	return p, nil
}

func filesCtx(cmd *cobra.Command) context.Context {
	if ctx := cmd.Context(); ctx != nil {
		return ctx
	}
	return context.Background()
}

func newFilesLs(deps opsDeps, use string, hidden bool) *cobra.Command {
	cmd := &cobra.Command{
		Use: use + " [path]", Short: aliasShort(use, "List one folder of a session's folder (paginated)"), Hidden: hidden,
		Args: cobra.MaximumNArgs(1), SilenceUsage: true, SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			t, err := filesTarget(cmd)
			if err != nil {
				return err
			}
			p, err := pathArg(cmd, args, false)
			if err != nil {
				return err
			}
			cursor, _ := cmd.Flags().GetString("cursor")
			limit, _ := cmd.Flags().GetInt("limit")
			a := ops.FileListArgs{Target: t, Path: p, Cursor: cursor, Limit: limit}
			if err := a.Validate(); err != nil {
				return usagef("%v", err)
			}
			op, _ := ops.Lookup(ops.OpFileList)
			backend, _, err := pickBackend(cmd, deps, op)
			if err != nil {
				return err
			}
			raw, err := backend.Call(filesCtx(cmd), ops.OpFileList, a)
			if err != nil {
				return failure(cmd, printer{w: cmd.OutOrStdout()}, op, "", err)
			}
			return printList(cmd, raw)
		},
	}
	cmd.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return &usageError{err} })
	filesCommonFlags(cmd)
	cmd.Flags().String("path", "", "the folder to list, relative to the session's folder (default: the folder itself)")
	cmd.Flags().String("cursor", "", "the next_cursor of the previous page")
	cmd.Flags().Int("limit", 0, fmt.Sprintf("entries per page (default %d, at most %d)", proto.FileListDefaultLimit, proto.FileListMaxLimit))
	return cmd
}

func printList(cmd *cobra.Command, raw json.RawMessage) error {
	if asJSON, _ := cmd.Flags().GetBool(flagJSON); asJSON {
		_, err := fmt.Fprintln(cmd.OutOrStdout(), string(raw))
		return err
	}
	var res ops.FileListResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return errors.New("the result of file_list cannot be read")
	}
	w := cmd.OutOrStdout()
	for _, e := range res.Entries {
		line := fmt.Sprintf("%-7s %12d  %s  %s", e.Type, e.Size, time.Unix(e.MTime, 0).UTC().Format("2006-01-02 15:04"), safeText(e.Name))
		if e.SkippedReason != "" {
			line += "  (not transferable: " + e.SkippedReason + ")"
		}
		_, _ = fmt.Fprintln(w, line)
	}
	if res.NextCursor != "" {
		_, _ = fmt.Fprintf(w, "more: --cursor %s\n", res.NextCursor)
	}
	return nil
}

// safeText replaces control characters of a name for the terminal.
func safeText(s string) string {
	b := []rune(s)
	for i, r := range b {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) {
			b[i] = '?'
		}
	}
	return string(b)
}

func newFilesPull(deps opsDeps, use string, hidden bool) *cobra.Command {
	cmd := &cobra.Command{
		Use: use + " <path>", Short: aliasShort(use, "Copy a file or folder from a session's folder into this folder"), Hidden: hidden,
		Args: cobra.MaximumNArgs(1), SilenceUsage: true, SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			t, err := filesTarget(cmd)
			if err != nil {
				return err
			}
			p, err := pathArg(cmd, args, true)
			if err != nil {
				return err
			}
			to, _ := cmd.Flags().GetString("to")
			ow, _ := cmd.Flags().GetBool("overwrite")
			a := ops.FilePullArgs{Target: t, Path: p, To: to, Overwrite: ow}
			return runTransfer(cmd, deps, ops.OpFilePull, a, func(ctx context.Context, c agent.FileCaller) (ops.FileTransferResult, error) {
				return agent.CallerPull(ctx, c, a)
			}, func(c agent.FileCaller) error { _, err := c.CheckPullLocal(a); return err })
		},
	}
	cmd.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return &usageError{err} })
	filesCommonFlags(cmd)
	cmd.Flags().String("path", "", "the file or folder to pull, relative to the session's folder")
	cmd.Flags().String("to", "", "the destination, relative to this folder (default: the last element of the path)")
	cmd.Flags().Bool("overwrite", false, "replace an existing regular file (without it an existing name is a conflict)")
	return cmd
}

func newFilesPush(deps opsDeps, use string, hidden bool) *cobra.Command {
	cmd := &cobra.Command{
		Use: use + " <path>", Short: aliasShort(use, "Copy a file or folder from this folder into a session's folder"), Hidden: hidden,
		Args: cobra.MaximumNArgs(1), SilenceUsage: true, SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			t, err := filesTarget(cmd)
			if err != nil {
				return err
			}
			p, err := pathArg(cmd, args, true)
			if err != nil {
				return err
			}
			as, _ := cmd.Flags().GetString("as")
			ow, _ := cmd.Flags().GetBool("overwrite")
			a := ops.FilePushArgs{Target: t, Path: p, As: as, Overwrite: ow}
			return runTransfer(cmd, deps, ops.OpFilePush, a, func(ctx context.Context, c agent.FileCaller) (ops.FileTransferResult, error) {
				return agent.CallerPush(ctx, c, a)
			}, nil)
		},
	}
	cmd.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return &usageError{err} })
	filesCommonFlags(cmd)
	cmd.Flags().String("path", "", "the file or folder to push, relative to this folder")
	cmd.Flags().String("as", "", "the destination, relative to the session's folder (default: the last element of the path)")
	cmd.Flags().Bool("overwrite", false, "replace an existing regular file (without it an existing name is a conflict)")
	return cmd
}

// fileLegBackend is the Hub backend's data-leg port (client.HubBackend).
type fileLegBackend interface {
	FileLeg(path string) (*http.Client, string, http.Header, error)
}

// runTransfer runs a pull or push with the backend of the command and prints
// the final result. check runs the local checks that need no root and no
// network (the destination path forms) for the local Worker case.
func runTransfer(cmd *cobra.Command, deps opsDeps, opName string, args any,
	run func(context.Context, agent.FileCaller) (ops.FileTransferResult, error), check func(agent.FileCaller) error) error {
	ctx := filesCtx(cmd)
	op, _ := ops.Lookup(opName)
	if check != nil {
		// The path forms first, before any backend or network (FR-4).
		if err := check(agent.FileCaller{Root: nil}); err != nil {
			if pe := protoOf(err); pe == nil || pe.Code == proto.CodePathOutsideRoot {
				return transferError(err)
			}
		}
	}
	backend, local, err := pickBackend(cmd, deps, op)
	if err != nil {
		return err
	}
	progress := progressPrinter(cmd)
	var res ops.FileTransferResult
	if local {
		raw, err := backend.Stream(ctx, opName, args, func(el json.RawMessage) error {
			var p agent.FileProgress
			if json.Unmarshal(el, &p) == nil {
				progress(p)
			}
			return nil
		})
		if err != nil {
			return failure(cmd, printer{w: cmd.OutOrStdout()}, op, "", err)
		}
		if err := json.Unmarshal(raw, &res); err != nil {
			return errors.New("the result of the transfer cannot be read")
		}
	} else {
		res, err = restTransfer(ctx, backend, run, progress)
		if err != nil {
			return transferError(err)
		}
	}
	return printTransfer(cmd, res)
}

// restTransfer runs a transfer with this process as the caller end.
func restTransfer(ctx context.Context, backend client.Backend, run func(context.Context, agent.FileCaller) (ops.FileTransferResult, error), progress func(agent.FileProgress)) (ops.FileTransferResult, error) {
	if runtime.GOOS == "windows" {
		return ops.FileTransferResult{}, proto.NewError(proto.CodeFilesRootUnknown, "file transfer is not available on Windows in this version")
	}
	if filexfer.DefaultResolveMode() != filexfer.ResolveWalk && filexfer.DefaultResolveMode() != filexfer.ResolveOpenat2 {
		if exe, err := os.Executable(); err == nil {
			pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			filexfer.ApplyProbe(filexfer.Probe(pctx, filexfer.ProbeCommand{Path: exe, Args: agent.FilesProbeArgs}, os.TempDir()))
			cancel()
		}
	}
	home, _ := os.UserHomeDir()
	root, err := filexfer.OpenCwdRoot(filexfer.RootOptions{Floor: filexfer.NewFloor(filexfer.FloorConfig{Home: home})})
	if err != nil {
		return ops.FileTransferResult{}, filexfer.ProtoError(err)
	}
	defer func() { _ = root.Close() }()
	c := agent.FileCaller{
		Root: root,
		Admit: func(ctx context.Context, op string, args any) (ops.FileTransferResult, error) {
			var res ops.FileTransferResult
			raw, err := backend.Call(ctx, op, args)
			if err != nil {
				return res, err
			}
			if err := json.Unmarshal(raw, &res); err != nil {
				return res, proto.NewError(proto.CodeInternal, "the Hub sent an admission this client cannot read")
			}
			return res, nil
		},
		Leg: func(ctx context.Context, adm ops.FileTransferResult) (*agent.FileLeg, error) {
			b := backend
			if p, ok := b.(*pinOfferBackend); ok {
				b = p.current()
			}
			lb, ok := b.(fileLegBackend)
			if !ok {
				return nil, proto.NewError(proto.CodeUnsupportedOp, "this Hub backend cannot open a data leg")
			}
			hc, u, h, err := lb.FileLeg(proto.FilesLegPath(adm.TransferID))
			if err != nil {
				return nil, err
			}
			return &agent.FileLeg{Client: hc, URL: u, Header: h, Close: func() {}}, nil
		},
		Receipt: func(ctx context.Context, a ops.FileReceiptArgs) {
			// A REST caller keeps no outbox: one try; the Hub reconciles a
			// missing receipt to unknown.
			rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			_, _ = backend.Call(rctx, ops.OpFileReceipt, a)
		},
		Progress: progress,
	}
	return run(ctx, c)
}

// protoOf returns the wire error of err, or nil.
func protoOf(err error) *proto.Error {
	if ae, ok := agent.AsAPIError(err); ok {
		return ae.ProtoError()
	}
	var pe *proto.Error
	if errors.As(err, &pe) {
		return pe
	}
	return nil
}

func transferError(err error) error {
	if pe := protoOf(err); pe != nil {
		msg := pe.Message
		if msg == "" {
			msg = pe.Code.Description()
		}
		return fmt.Errorf("%s: %s", pe.Code, oneLine(msg))
	}
	return err
}

// progressPrinter reports progress on stderr when it is a terminal.
func progressPrinter(cmd *cobra.Command) func(agent.FileProgress) {
	f, ok := cmd.ErrOrStderr().(*os.File)
	if !ok {
		return func(agent.FileProgress) {}
	}
	if st, err := f.Stat(); err != nil || st.Mode()&os.ModeCharDevice == 0 {
		return func(agent.FileProgress) {}
	}
	return func(p agent.FileProgress) {
		if p.Total > 0 {
			_, _ = fmt.Fprintf(f, "\r%s %d / %d bytes (%d%%)   ", p.Direction, p.Bytes, p.Total, p.Bytes*100/p.Total)
		} else {
			_, _ = fmt.Fprintf(f, "\r%s %d bytes   ", p.Direction, p.Bytes)
		}
	}
}

func printTransfer(cmd *cobra.Command, res ops.FileTransferResult) error {
	if asJSON, _ := cmd.Flags().GetBool(flagJSON); asJSON {
		b, err := json.Marshal(res)
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintln(cmd.OutOrStdout(), string(b))
	} else {
		w := cmd.OutOrStdout()
		_, _ = fmt.Fprintf(w, "%s %s: %s, %d files, %d bytes (%d on the wire)\n", res.Direction, res.TransferID, res.Outcome, res.Files, res.Bytes, res.WireBytes)
		if res.Reason != "" {
			_, _ = fmt.Fprintf(w, "reason: %s\n", res.Reason)
		}
		for _, e := range res.Entries {
			line := "  " + safeText(e.Path)
			if e.SkippedReason != "" {
				line += "  (skipped: " + e.SkippedReason + ")"
			}
			_, _ = fmt.Fprintln(w, line)
		}
		if res.EntriesOmitted > 0 {
			_, _ = fmt.Fprintf(w, "  ... and %d more\n", res.EntriesOmitted)
		}
	}
	if code := outcomeExitCode(res.Outcome); code != ExitOK {
		return &exitCodeError{code}
	}
	return nil
}
