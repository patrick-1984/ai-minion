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
	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/version"
)

func init() {
	Register(func(root *cobra.Command) {
		var out, dataDir string
		var bo hubBackupOpts
		cmd := &cobra.Command{
			Use:   "backup --out <file>",
			Short: "Write one encrypted archive of the Hub database, keys and certificate",
			Long: `Write one archive of the Hub data directory: a consistent copy of minion.db
(VACUUM INTO, safe while the Hub runs), data.key, the Hub identity key, the
generated TLS certificate and key, and a manifest with the version and the
schema version. The archive is created with mode 0600. An existing file is
never overwritten.

The archive holds the Hub's secret keys, so it is encrypted by default with age
(scrypt passphrase). Choose one:
  --passphrase-file FILE  encrypt with the passphrase in FILE (a private file:
                          owner-only, not a symbolic link; a trailing newline
                          is ignored). Keep the passphrase apart from the
                          archive: without it the backup cannot be restored.
  --unencrypted           write a plain .tar.gz. Anyone who can read the file
                          gets the Hub's keys. A warning is printed.
There is no default: without either flag the command refuses.

The data directory is MINION_DATA_DIR unless --data-dir is given. Restore
with "minion hub restore <archive>" while the Hub is stopped.`,
			Args: cobra.NoArgs,
			RunE: func(cmd *cobra.Command, _ []string) error {
				return runHubBackup(cmd.Context(), cmd.OutOrStdout(), hubBackupDeps{}, dataDir, out, bo)
			},
		}
		cmd.Flags().StringVar(&out, "out", "", "archive file to write (required)")
		cmd.Flags().StringVar(&dataDir, "data-dir", "", "Hub data directory (default: MINION_DATA_DIR or the OS default)")
		cmd.Flags().StringVar(&bo.passphraseFile, "passphrase-file", "", "encrypt the archive with the passphrase in this private file")
		cmd.Flags().BoolVar(&bo.unencrypted, "unencrypted", false, "write a plain, unencrypted archive (not recommended)")
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

// hubBackupOpts are the encryption choices of "minion hub backup".
type hubBackupOpts struct {
	passphraseFile string
	unencrypted    bool
}

// errBackupChoice is the refusal of a backup without an encryption choice.
const errBackupChoice = "a backup holds the Hub's secret keys and must be encrypted: pass --passphrase-file FILE " +
	"(a private file with the passphrase), or --unencrypted to write a plain archive on purpose"

// readPassphraseFile reads a passphrase from a private file: it is opened with
// identity.OpenPrivate (a regular file of the current user that group and
// others cannot access, not a symbolic link). Trailing CR and LF characters are
// removed; an empty passphrase is refused.
func readPassphraseFile(path string) (string, error) {
	f, err := identity.OpenPrivate(path)
	if err != nil {
		return "", fmt.Errorf("passphrase file: %w", err)
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(f, maxPassphraseFile+1))
	if err != nil {
		return "", fmt.Errorf("passphrase file: %w", err)
	}
	if len(b) > maxPassphraseFile {
		return "", fmt.Errorf("passphrase file %s is larger than %d bytes", path, maxPassphraseFile)
	}
	pass := strings.TrimRight(string(b), "\r\n")
	if pass == "" {
		return "", fmt.Errorf("passphrase file %s is empty", path)
	}
	return pass, nil
}

const maxPassphraseFile = 4096

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

func runHubBackup(ctx context.Context, w io.Writer, d hubBackupDeps, dataDirFlag, out string, bo hubBackupOpts) error {
	if bo.unencrypted && bo.passphraseFile != "" {
		return errors.New("--passphrase-file and --unencrypted exclude each other")
	}
	if !bo.unencrypted && bo.passphraseFile == "" {
		return errors.New(errBackupChoice)
	}
	var opts []store.Option
	if bo.passphraseFile != "" {
		pass, err := readPassphraseFile(bo.passphraseFile)
		if err != nil {
			return err
		}
		opts = append(opts, store.WithBackupPassphrase(pass))
	}
	dir, err := resolveDataDir(dataDirFlag, d)
	if err != nil {
		return err
	}
	m, err := store.Backup(ctx, dir, out, version.Version, opts...)
	if err != nil {
		return err
	}
	if bo.unencrypted {
		_, err = fmt.Fprintf(w, "WARNING: %s is NOT encrypted and holds the Hub's secret keys (identity, data and TLS keys); anyone who can read it controls this Hub. Encrypt it or keep it private.\n", out)
		if err != nil {
			return err
		}
	}
	state := "encrypted with age"
	if bo.unencrypted {
		state = "unencrypted"
	}
	_, err = fmt.Fprintf(w, "Backup written to %s (%s, %d files, schema version %d, minion %s).\nThe archive holds the Hub's secret keys; keep it private.\n",
		out, state, len(m.Files)+1, m.SchemaVersion, m.Version)
	return err
}
