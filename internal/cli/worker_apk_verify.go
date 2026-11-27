package cli

// minion worker apk-verify: the Android app's check of a staged APK before it
// installs it (v0.24.0). The app runs it from its read-only native library
// directory as a fresh process, so the check is code of the installed APK, and
// learns the digest and version code it must see in the bytes it streams into
// the PackageInstaller session. It authenticates the staged checksums.txt
// offline, with the Sigstore trusted root the Worker cached when it downloaded
// the APK.

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/version"
)

func init() {
	Register(func(root *cobra.Command) {
		w := workerCommand(root)
		w.AddCommand(newWorkerAPKVerifyCmd(nil))
	})
}

// newWorkerAPKVerifyCmd builds the command; verifiers replaces the Sigstore
// verifier in tests (nil: the cached public-good trusted root).
func newWorkerAPKVerifyCmd(verifiers func(cacheDir string) version.VerifierFactory) *cobra.Command {
	var tag string
	var asJSON bool
	cmd := &cobra.Command{
		Use:    "apk-verify",
		Short:  "Authenticate the staged Android app update (used by the Minion app)",
		Long:   "Verify the APK the Worker staged in <state>/update/apk for the release --tag: the signed checksums.txt\nmust carry the Sigstore signature of the release workflow for that tag and list the APK's SHA-256.\nPrints the tag, version code, SHA-256, size and path (--json for the app). Needs no network.",
		Args:   cobra.NoArgs,
		Hidden: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			if ctx == nil {
				ctx = context.Background()
			}
			res := &config.Resolver{Role: config.ScopeWorker, Registry: config.Default, Flags: changedFlags{cmd: cmd, reg: config.Default}, LookupEnv: os.LookupEnv}
			dir, err := res.Resolve(config.KeyStateDir)
			if err != nil {
				return err
			}
			if dir.Value == "" {
				return errors.New("the state directory is not known; give --state-dir or set MINION_STATE_DIR")
			}
			cache, err := res.Resolve(config.KeyUpdateCacheDir)
			if err != nil {
				return err
			}
			vf := version.SigstoreVerifierFactory(version.TUFTrustedRoot{Dir: cache.Value})
			if verifiers != nil {
				vf = verifiers(cache.Value)
			}
			got, err := version.VerifyStagedAPK(ctx, vf, filepath.Join(dir.Value, "update", "apk"), tag)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if asJSON {
				b, err := json.Marshal(got)
				if err != nil {
					return err
				}
				return writef(out, "%s\n", b)
			}
			return writef(out, "%s verified: %s, version code %d, sha256 %s, %d bytes\n", got.Tag, got.Path, got.VersionCode, got.SHA256, got.Size)
		},
	}
	cmd.Flags().StringVar(&tag, "tag", "", "release tag of the staged APK (vX.Y.Z, vX.Y.Z-rc.N or android-vX.Y.Z)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the result as JSON")
	_ = cmd.MarkFlagRequired("tag")
	addNamedSettingFlags(cmd, config.Default, config.KeyStateDir, config.KeyUpdateCacheDir)
	return cmd
}
