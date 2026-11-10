package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/version"
)

func init() {
	Register(func(root *cobra.Command) {
		var out, dataDir string
		cmd := &cobra.Command{
			Use:   "backup --out <file>",
			Short: "Write one archive of the Hub database, keys and certificate",
			Long: `Write one .tar.gz archive of the Hub data directory: a consistent copy of
minion.db (VACUUM INTO, safe while the Hub runs), data.key, the Hub identity
key, the generated TLS certificate and key, and a manifest with the version
and the schema version. The archive is created with mode 0600 and holds the
Hub's secret keys: store it like a password. An existing file is never
overwritten.

The data directory is MINION_DATA_DIR unless --data-dir is given. Restore
with "minion hub restore <archive>" while the Hub is stopped.`,
			Args: cobra.NoArgs,
			RunE: func(cmd *cobra.Command, _ []string) error {
				return runHubBackup(cmd.Context(), cmd.OutOrStdout(), hubBackupDeps{}, dataDir, out)
			},
		}
		cmd.Flags().StringVar(&out, "out", "", "archive file to write (required)")
		cmd.Flags().StringVar(&dataDir, "data-dir", "", "Hub data directory (default: MINION_DATA_DIR or the OS default)")
		_ = cmd.MarkFlagRequired("out")
		hubCommand(root).AddCommand(cmd)
	})
}

// hubBackupDeps are the inputs of the backup and restore commands that tests
// replace.
type hubBackupDeps struct {
	lookupEnv func(string) (string, bool)
	// containerDir, when not nil, replaces the container data dir heuristic
	// of the restore command.
	containerDir func(dir string) containerKind
}

// resolveDataDir returns flagValue when set, otherwise the Hub data dir
// setting (MINION_DATA_DIR or the OS default). It resolves only that setting,
// so a backup does not depend on the rest of the Hub configuration.
func resolveDataDir(flagValue string, d hubBackupDeps) (string, error) {
	// Trim like "minion hub serve" does (config.ResolveHubServer): a trailing
	// space or CR from an .env file must not select a sibling directory.
	if v := strings.TrimSpace(flagValue); v != "" {
		return v, nil
	}
	res := &config.Resolver{Role: config.ScopeHub, LookupEnv: d.lookupEnv}
	r, err := res.Resolve(config.KeyDataDir)
	if err != nil {
		return "", err
	}
	if v := strings.TrimSpace(r.Value); v != "" {
		return v, nil
	}
	return "", errors.New("the Hub data directory is not set; set MINION_DATA_DIR or pass --data-dir")
}

func runHubBackup(ctx context.Context, w io.Writer, d hubBackupDeps, dataDirFlag, out string) error {
	dir, err := resolveDataDir(dataDirFlag, d)
	if err != nil {
		return err
	}
	m, err := store.Backup(ctx, dir, out, version.Version)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "Backup written to %s (%d files, schema version %d, minion %s).\nThe archive holds the Hub's secret keys; keep it private.\n",
		out, len(m.Files)+1, m.SchemaVersion, m.Version)
	return err
}
