package cli

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/hub/workers"
)

func init() {
	Register(func(root *cobra.Command) {
		hubCommand(root).AddCommand(&cobra.Command{
			Use:   "identity",
			Short: "Print the fingerprint of the Hub identity key",
			Long: `Print the fingerprint of the Hub identity key: 52 lowercase base32 characters.

The Hub creates its Ed25519 identity key in <data dir>/identity.key on the
first start (minion hub serve) and keeps it for good: it does not change on
restart, certificate rotation or restore. Workers pin it, and an invitation key
(mnk_<fingerprint>.<secret>) carries it. The data directory is MINION_DATA_DIR.`,
			Args: cobra.NoArgs,
			RunE: func(cmd *cobra.Command, _ []string) error {
				return runHubIdentity(cmd.OutOrStdout(), nil)
			},
		})
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
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(out, id.Fingerprint())
	return err
}
