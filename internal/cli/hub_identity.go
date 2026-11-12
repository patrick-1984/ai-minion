package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"

	"github.com/spf13/cobra"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/hub/workers"
)

func init() {
	Register(func(root *cobra.Command) {
		identity := &cobra.Command{
			Use:   "identity",
			Short: "Print the fingerprint of the Hub identity key, or manage the Hub identity",
			Long: `Print the fingerprint of the Hub root (master) key: 52 lowercase base32 characters.

The root key is created when the owner sets the Hub passphrase (or, for a Hub
upgraded from 0.6, is the former identity key) and is stored sealed with that
passphrase in <data dir>/root.key.age. It does not change on restart,
certificate re-issue or restore; only "rotate-master" replaces it. Workers pin
it, and an invitation key (mnk_<fingerprint>.<secret>) carries it. This command
reads the recorded public key and needs neither the passphrase nor the running
Hub. The data directory is MINION_DATA_DIR.

The subcommands status, set-passphrase, reissue, rotate-master and
change-passphrase talk to the running Hub over its local admin socket in the
data directory (see "minion hub identity status --help").`,
			Args: cobra.NoArgs,
			RunE: func(cmd *cobra.Command, _ []string) error {
				return runHubIdentity(cmd.OutOrStdout(), nil)
			},
		}
		addIdentityCommands(identity, defaultIdentityDeps())
		hubCommand(root).AddCommand(identity)
	})
}

// runHubIdentity prints the fingerprint of the identity key in the data
// directory. lookupEnv is replaced by tests.
func runHubIdentity(out io.Writer, lookupEnv func(string) (string, bool)) error {
	res := &config.Resolver{Role: config.ScopeHub, LookupEnv: lookupEnv}
	dir, err := res.Resolve(config.KeyDataDir)
	if err != nil {
		return err
	}
	id, err := workers.ReadIdentity(dir.Value)
	if err == nil {
		_, err = fmt.Fprintln(out, id.Fingerprint())
		return err
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	// Once a passphrase is set the private key file is gone: the public
	// fingerprint comes from the database, read-only. No key and no running
	// Hub are needed.
	fp, derr := store.RecordedFingerprint(context.Background(), dir.Value)
	if derr != nil {
		return fmt.Errorf("%w (and the database could not be read: %v)", err, derr)
	}
	if fp == "" {
		return err
	}
	_, err = fmt.Fprintln(out, fp)
	return err
}
