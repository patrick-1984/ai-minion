package cli

// `minion apps install|status` (v0.25.0, docs/guides/android-apps.md): install
// another app on an Android Worker from an APK in its Minion files folder
// (copied there with `minion files push`), through the Hub. The generic
// derived commands app_install and app_install_status stay as they are; these
// add the Worker name, the readable output and the exit status.
//
// Exit status of install: 0 installed, needs_confirmation_on_device or
// installing; 3 refused; 1 failed or an error; 2 usage.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/patrick-1984/ai-minion/internal/client"
	"github.com/patrick-1984/ai-minion/internal/ops"
)

func init() {
	Register(func(root *cobra.Command) {
		addAppsCommands(root, defaultOpsDeps())
	})
}

func addAppsCommands(root *cobra.Command, deps opsDeps) {
	apps := &cobra.Command{
		Use:   "apps",
		Short: "Install apps on an Android Worker from its Minion files folder",
		Long: "Install another app on an Android Worker from an APK that was copied into its Minion files folder " +
			"(minion files push), and show the outcome. A first install needs one confirmation on the device.\n\n" +
			"Exit status of install: 0 installed, needs_confirmation_on_device or installing; 3 refused; 1 failed or error; 2 usage.",
		Args:          noArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	apps.AddCommand(newAppsInstall(deps), newAppsStatus(deps))
	root.AddCommand(apps)
}

func appsCommonFlags(cmd *cobra.Command) {
	cmd.Flags().String("target", "", "the Android Worker: its id or its name (required)")
	cmd.Flags().Bool(flagJSON, false, "print the result as JSON (one line)")
	addHubFlags(cmd.Flags())
	cmd.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return &usageError{err} })
}

// appsWorker resolves --target to a Worker id: a UUIDv7 as it is, a name
// through workers_list (it must name exactly one Worker).
func appsWorker(cmd *cobra.Command, backend client.Backend) (string, error) {
	v, _ := cmd.Flags().GetString("target")
	v = strings.TrimSpace(v)
	if v == "" {
		return "", usagef("--target <android-worker> is required")
	}
	if strings.Contains(v, ":") {
		return "", usagef("--target names a Worker, not a session (no \":\")")
	}
	if ops.ValidUUIDv7(v) {
		return v, nil
	}
	raw, err := backend.Call(filesCtx(cmd), ops.OpWorkersList, json.RawMessage(`{}`))
	if err != nil {
		return "", fmt.Errorf("find the Worker %q: %w", v, err)
	}
	var list ops.WorkersListResult
	if err := json.Unmarshal(raw, &list); err != nil {
		return "", errors.New("the result of workers_list cannot be read")
	}
	var found []string
	for _, w := range list.Workers {
		if w.Name == v {
			found = append(found, w.ID)
		}
	}
	switch len(found) {
	case 0:
		return "", fmt.Errorf("no Worker named %q (give its id or a name from minion workers_list)", v)
	case 1:
		return found[0], nil
	}
	return "", fmt.Errorf("%d Workers are named %q: give the Worker's id", len(found), v)
}

func newAppsInstall(deps opsDeps) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "install <path in Minion files>",
		Short: "Install an app on an Android Worker from an APK in its Minion files folder",
		Long: "Install an app on an Android Worker from an APK in its Minion files folder. The path is relative to that folder.\n\n" +
			"Without --allow-new only an update of an installed app is accepted: the same signer and a higher version. " +
			"--allow-new also installs an app that is not installed yet, and reinstalls the installed version. " +
			"A first install needs one press of Install <app> on the device (the Minion screen or its notification).\n\n" +
			"Exit status: 0 installed, needs_confirmation_on_device or installing; 3 refused; 1 failed or error; 2 usage.",
		Args: cobra.MaximumNArgs(1), SilenceUsage: true, SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := pathArg(cmd, args, true)
			if err != nil {
				return err
			}
			if err := ops.ValidAppInstallPath(p); err != nil {
				return usagef("%v", err)
			}
			if v, _ := cmd.Flags().GetString("target"); strings.TrimSpace(v) == "" {
				return usagef("--target <android-worker> is required")
			}
			allowNew, _ := cmd.Flags().GetBool("allow-new")
			op, _ := ops.Lookup(ops.OpAppInstall)
			backend, local, err := pickBackend(cmd, deps, op)
			if err != nil {
				return err
			}
			if local {
				return errors.New("app installs run through the Hub, which authorizes and audits them: set MINION_HUB_URL and MINION_API_KEY_FILE")
			}
			id, err := appsWorker(cmd, backend)
			if err != nil {
				return err
			}
			a := ops.AppInstallArgs{WorkerID: id, Path: p, AllowNew: allowNew}
			raw, _ := json.Marshal(a)
			res, err := backend.Call(filesCtx(cmd), ops.OpAppInstall, json.RawMessage(raw))
			if err != nil {
				return failure(cmd, printer{w: cmd.OutOrStdout()}, op, "", err)
			}
			return printAppInstall(cmd, res)
		},
	}
	appsCommonFlags(cmd)
	cmd.Flags().String("path", "", "the APK, relative to the Minion files folder")
	cmd.Flags().Bool("allow-new", false, "also install an app that is not installed yet (and reinstall the installed version)")
	return cmd
}

// appInstallExitCode is the exit status of an install outcome.
func appInstallExitCode(outcome string) int {
	switch outcome {
	case ops.AppInstallInstalled, ops.AppInstallNeedsConfirmation, ops.AppInstallInstalling:
		return ExitOK
	case ops.AppInstallRefused:
		return ExitFailedNotSent
	}
	return ExitError
}

func printAppInstall(cmd *cobra.Command, raw json.RawMessage) error {
	var res ops.AppInstallResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return errors.New("the result of app_install cannot be read")
	}
	if asJSON, _ := cmd.Flags().GetBool(flagJSON); asJSON {
		_, _ = fmt.Fprintln(cmd.OutOrStdout(), strings.TrimSpace(string(raw)))
	} else {
		writeAppAttempt(cmd.OutOrStdout(), res)
	}
	if code := appInstallExitCode(res.Outcome); code != ExitOK {
		return &exitCodeError{code}
	}
	return nil
}

func writeAppAttempt(w io.Writer, res ops.AppInstallResult) {
	_, _ = fmt.Fprintf(w, "outcome: %s\n", res.Outcome)
	if res.Path != "" {
		_, _ = fmt.Fprintf(w, "file: %s (%d bytes, sha256 %s)\n", safeText(res.Path), res.Size, res.SHA256)
	}
	if res.Package != "" {
		app := safeText(res.Package)
		if res.Label != "" {
			app = safeText(res.Label) + " (" + app + ")"
		}
		_, _ = fmt.Fprintf(w, "app: %s\n", app)
		version := fmt.Sprintf("%d", res.VersionCode)
		if res.VersionName != "" {
			version = safeText(res.VersionName) + " (" + version + ")"
		}
		switch {
		case res.NewInstall:
			version += ", new install"
		case res.InstalledVersionCode > 0:
			version += fmt.Sprintf(", installed before: %d", res.InstalledVersionCode)
		}
		_, _ = fmt.Fprintf(w, "version: %s\n", version)
	}
	if res.SignerSHA256 != "" {
		_, _ = fmt.Fprintf(w, "signer sha256: %s\n", res.SignerSHA256)
	}
	if res.Reason != "" {
		_, _ = fmt.Fprintf(w, "reason: %s\n", safeText(res.Reason))
	}
	if res.Outcome == ops.AppInstallNeedsConfirmation {
		name := res.Label
		if name == "" {
			name = res.Package
		}
		if name == "" {
			name = "the app"
		}
		_, _ = fmt.Fprintf(w, "Confirm the install on the device: open Minion and press Install %s\n", safeText(name))
	}
}

func newAppsStatus(deps opsDeps) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show whether an Android Worker can install apps, its last install and the APK files in Minion files",
		Args:  noArgs, SilenceUsage: true, SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if v, _ := cmd.Flags().GetString("target"); strings.TrimSpace(v) == "" {
				return usagef("--target <android-worker> is required")
			}
			op, _ := ops.Lookup(ops.OpAppInstallStatus)
			backend, local, err := pickBackend(cmd, deps, op)
			if err != nil {
				return err
			}
			if local {
				return errors.New("app installs run through the Hub: set MINION_HUB_URL and MINION_API_KEY_FILE")
			}
			id, err := appsWorker(cmd, backend)
			if err != nil {
				return err
			}
			raw, _ := json.Marshal(ops.AppInstallStatusArgs{WorkerID: id})
			res, err := backend.Call(filesCtx(cmd), ops.OpAppInstallStatus, json.RawMessage(raw))
			if err != nil {
				return failure(cmd, printer{w: cmd.OutOrStdout()}, op, "", err)
			}
			return printAppStatus(cmd, res)
		},
	}
	appsCommonFlags(cmd)
	return cmd
}

func printAppStatus(cmd *cobra.Command, raw json.RawMessage) error {
	if asJSON, _ := cmd.Flags().GetBool(flagJSON); asJSON {
		_, err := fmt.Fprintln(cmd.OutOrStdout(), strings.TrimSpace(string(raw)))
		return err
	}
	var st ops.AppInstallStatus
	if err := json.Unmarshal(raw, &st); err != nil {
		return errors.New("the result of app_install_status cannot be read")
	}
	w := cmd.OutOrStdout()
	if st.Ready {
		_, _ = fmt.Fprintln(w, "ready: yes")
	} else {
		_, _ = fmt.Fprintf(w, "ready: no (%s)\n", safeText(st.BlockReason))
	}
	if st.Last != nil {
		_, _ = fmt.Fprintln(w, "last install:")
		var b strings.Builder
		writeAppAttempt(&b, *st.Last)
		for _, line := range strings.Split(strings.TrimRight(b.String(), "\n"), "\n") {
			_, _ = fmt.Fprintln(w, "  "+line)
		}
	} else {
		_, _ = fmt.Fprintln(w, "last install: none")
	}
	if len(st.Files) == 0 {
		_, _ = fmt.Fprintln(w, "APK files in Minion files: none (copy one with minion files push)")
		return nil
	}
	_, _ = fmt.Fprintln(w, "APK files in Minion files:")
	for _, f := range st.Files {
		_, _ = fmt.Fprintf(w, "  %12d  %s\n", f.Size, safeText(f.Path))
	}
	return nil
}
