package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/hub/server"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/hub/workers"
	"github.com/patrick-1984/ai-minion/internal/identity"
)

func init() {
	Register(func(root *cobra.Command) {
		var words bool
		idCmd := &cobra.Command{
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
data directory (see "minion hub identity status --help").

With --words it prints the fingerprint as six words of the BIP39 English word
list (the form people compare), the 52 characters, the ready-made lines for a
Worker, and the SPKI pin of the Hub's TLS certificate for --hub-tls-pin of CLI
and MCP clients.`,
			Args: cobra.NoArgs,
			RunE: func(cmd *cobra.Command, _ []string) error {
				return runHubIdentity(cmd.OutOrStdout(), nil, words)
			},
		}
		idCmd.Flags().BoolVar(&words, "words", false, "print the fingerprint as six words, with the lines to give a Worker and the TLS key pin")
		addIdentityCommands(idCmd, defaultIdentityDeps())
		hubCommand(root).AddCommand(idCmd)
	})
}

// runHubIdentity prints the fingerprint of the identity key in the data
// directory; with words it prints block M10 (see [writeHubIdentityWords]).
// lookupEnv is replaced by tests.
func runHubIdentity(out io.Writer, lookupEnv func(string) (string, bool), words bool) error {
	res := &config.Resolver{Role: config.ScopeHub, LookupEnv: lookupEnv}
	dir, err := res.Resolve(config.KeyDataDir)
	if err != nil {
		return err
	}
	fp, err := hubRootFingerprint(dir.Value)
	if err != nil {
		return err
	}
	if !words {
		_, err = fmt.Fprintln(out, fp)
		return err
	}
	hs, err := config.ResolveHubServer(res)
	if err != nil {
		return err
	}
	return writeHubIdentityWords(out, fp, hs)
}

// hubRootFingerprint returns the 52 characters of the root key recorded in
// dir, or the error that says why there is none yet.
func hubRootFingerprint(dir string) (string, error) {
	id, err := workers.ReadIdentity(dir)
	if err == nil {
		return id.Fingerprint().String(), nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	// Once a passphrase is set the private key file is gone: the public
	// fingerprint comes from the database, read-only. No key and no running
	// Hub are needed.
	fp, derr := store.RecordedFingerprint(context.Background(), dir)
	if derr != nil {
		return "", fmt.Errorf("%w (and the database could not be read: %v)", err, derr)
	}
	if fp == "" {
		return "", err
	}
	return fp, nil
}

// writeHubIdentityWords prints block M10: the words, the 52 characters, the
// lines to give a Worker (always the 52 characters: they are copied, not
// compared) and, when the Hub serves its own TLS certificate, its SPKI pin.
func writeHubIdentityWords(out io.Writer, fp string, hs config.HubServer) error {
	words := identity.Fingerprint(fp).Words()
	url := strings.TrimRight(hs.PublicURL, "/")
	if url == "" {
		url = "<hub address>"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Hub fingerprint:  %s\n", words)
	fmt.Fprintf(&b, "Full:             %s\n\n", fp)
	b.WriteString("For a new Worker (it then accepts only this Hub):\n")
	fmt.Fprintf(&b, "  minion worker join %s --hub-fingerprint %s\n", url, fp)
	fmt.Fprintf(&b, "  MINION_HUB_FINGERPRINT=%s\n", fp)
	b.WriteString("For a Worker that is already paired, run on the Worker:\n")
	fmt.Fprintf(&b, "  minion worker verify-hub %s\n", fp)
	if pin := hubTLSPinLine(hs); pin != "" {
		b.WriteString("\n" + pin)
	}
	_, err := io.WriteString(out, b.String())
	return err
}

// hubTLSPinLine returns the "TLS key pin" lines for the certificate the Hub
// serves (the operator's file, else the generated one in <data>/tls/), or ""
// when TLS is off or there is no certificate file yet.
func hubTLSPinLine(hs config.HubServer) string {
	if hs.TLS == config.TLSOff {
		return ""
	}
	path := hs.CertFile
	if path == "" {
		path = filepath.Join(hs.DataDir, server.TLSDir, server.TLSCertFile)
	}
	hash, err := server.CertFileKeyHash(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return ""
	case err != nil:
		return fmt.Sprintf("TLS key pin: not available (%v)\n", err)
	}
	return "TLS key pin: " + server.TLSKeyPin(hash) + "\n" +
		"CLI and MCP clients that do not trust the Hub's HTTPS certificate\n" +
		"can pin it with --hub-tls-pin and this value.\n"
}
