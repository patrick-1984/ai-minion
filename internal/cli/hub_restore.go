package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"

	"github.com/spf13/cobra"

	"github.com/patrick-1984/ai-minion/internal/hub/store"
)

// containerUID is the user the Docker image runs as (A-14).
const containerUID = 65532

func init() {
	Register(func(root *cobra.Command) {
		var dataDir string
		var force bool
		var passphraseFile string
		var maxSize int64
		cmd := &cobra.Command{
			Use:   "restore <archive>",
			Short: "Restore the Hub data directory from a backup archive (Hub stopped)",
			Long: `Restore the Hub data directory from an archive written by "minion hub backup".
Stop the Hub first.

An archive encrypted with age is detected by its header; give the passphrase
with --passphrase-file FILE (a private file, as for the backup). A plain
archive from an earlier version or from --unencrypted is restored as it is. The
whole archive is decrypted and checked in a temporary area before anything in
the data directory is touched, so a wrong passphrase or a damaged file leaves
the data directory as it was. The data directory is created only after the
whole archive passed. The archive must hold exactly the identity files of its
state (none, plain identity.key, or the sealed root root.key.age with, once a
certificate exists, op.key) and they must match the database. A restore issues
no certificate and asks for no passphrase: the Hub comes back in the state of
the backup, and its sealed root opens with the passphrase of the backup's time. --max-size limits the decrypted size and the total
size of the files (default 64 GiB).

The data directory is MINION_DATA_DIR unless --data-dir is given. A data
directory that already has files is refused unless --force is given; --force
replaces the database, the data key, the identity key and the generated TLS
material and leaves other files alone. An archive written by a newer minion
(newer schema) is refused: there is no database downgrade.

Restored files are created with mode 0600 and belong to the user who runs this
command. For a Docker data directory, run the printed chown on the host so
that the Hub (UID 65532) can use the files.`,
			Args: cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				return runHubRestore(cmd.Context(), cmd.OutOrStdout(), hubBackupDeps{}, dataDir, args[0], force, passphraseFile, maxSize)
			},
		}
		cmd.Flags().StringVar(&dataDir, "data-dir", "", "Hub data directory (default: MINION_DATA_DIR or the OS default)")
		cmd.Flags().BoolVar(&force, "force", false, "replace the files of a non-empty data directory")
		cmd.Flags().StringVar(&passphraseFile, "passphrase-file", "", "private file with the passphrase of an encrypted archive")
		cmd.Flags().Int64Var(&maxSize, "max-size", 0, "limit in bytes on the decrypted size and on the total size of the files of the archive (default 64 GiB)")
		hubCommand(root).AddCommand(cmd)
	})
}

func runHubRestore(ctx context.Context, w io.Writer, d hubBackupDeps, dataDirFlag, archive string, force bool, passphraseFile string, maxSize int64) error {
	var opts []store.Option
	if maxSize > 0 {
		opts = append(opts, store.WithRestoreLimit(maxSize))
	}
	if passphraseFile != "" {
		pass, err := readPassphraseFile(passphraseFile)
		if err != nil {
			return err
		}
		opts = append(opts, store.WithRestorePassphrase(pass))
	}
	dir, err := resolveDataDir(dataDirFlag, d)
	if err != nil {
		return err
	}
	res, err := store.Restore(ctx, archive, dir, force, opts...)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "Restored %d files into %s (archive schema %d, written by minion %s; installed schema %d).\nStart the Hub with \"minion hub serve\".\n",
		len(res.Files), res.Dir, res.Manifest.SchemaVersion, res.Manifest.Version, res.InstalledSchema); err != nil {
		return err
	}
	if err := printIdentityNotes(w, res); err != nil {
		return err
	}
	kind := d.containerDir
	if kind == nil {
		kind = func(dir string) containerKind { return looksLikeContainerDir(dir, os.Geteuid(), fileExists) }
	}
	switch kind(res.Dir) {
	case onContainerHost:
		_, err = fmt.Fprintf(w, "This looks like a container data directory. The Hub image runs as UID %d, so on the host run:\n  chown -R %d:%d %s\n",
			containerUID, containerUID, containerUID, res.Dir)
	case insideContainer:
		// res.Dir is a path inside this container; the host path behind the
		// mount is unknown, and chown -R on the container path could re-own an
		// unrelated tree on the host.
		_, err = fmt.Fprintf(w, "This restore ran inside a container. The Hub image runs as UID %d, so give it the files:\n  chown -R %d:%d <host dir>\nrun it on the host directory mounted at %s\n",
			containerUID, containerUID, containerUID, res.Dir)
	}
	return err
}

// containerKind says whether the restored directory needs the chown reminder
// and how it has to be worded.
type containerKind int

const (
	// notContainer needs no reminder.
	notContainer containerKind = iota
	// onContainerHost: the restore runs on the host (as root, or into /data)
	// and the directory path is a host path.
	onContainerHost
	// insideContainer: the restore runs inside a container, so the directory
	// path is the container's view of a mount, not a host path.
	insideContainer
)

// looksLikeContainerDir reports whether dir is probably the data directory of
// the Docker image and why: a restore run inside a container, or on the host
// into the image's /data or as root (whose files UID 65532 could not read).
// A restore that already runs as the Hub user (UID 65532) needs no reminder.
// exists reports whether a path exists.
func looksLikeContainerDir(dir string, euid int, exists func(string) bool) containerKind {
	if runtime.GOOS == "windows" || euid == containerUID {
		return notContainer
	}
	if exists("/.dockerenv") || exists("/run/.containerenv") {
		return insideContainer
	}
	if filepath.Clean(dir) == "/data" || euid == 0 {
		return onContainerHost
	}
	return notContainer
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// printIdentityNotes tells the operator the identity state of the restored Hub
// and prints the root rollback warning when it applies (contract section 13).
func printIdentityNotes(w io.Writer, res store.RestoreResult) error {
	switch res.IdentityState {
	case store.IdentityStatePlain:
		if _, err := fmt.Fprintln(w, "Identity state: plain. The Hub root is the plain identity.key; set the Hub passphrase on the identity page to seal it."); err != nil {
			return err
		}
	case store.IdentityStateSealedNoCert:
		if _, err := fmt.Fprintf(w, "Identity state: sealed, no certificate (root %s). Re-issue the certificate on the identity page. The sealed root opens with the passphrase of the backup's time.\n", res.RootFingerprint); err != nil {
			return err
		}
	case store.IdentityStateEstablished:
		if _, err := fmt.Fprintf(w, "Identity state: established (root %s). The backed-up certificate is kept; no certificate was issued. The sealed root opens with the passphrase of the backup's time.\n", res.RootFingerprint); err != nil {
			return err
		}
	}
	switch {
	case res.RootRolledBack:
		_, err := fmt.Fprintf(w, "WARNING: %s.\nThe data directory that was replaced had rotated away root %s. Restore a backup made after the rotation, or reset the affected Workers and enroll them again.\n", store.RootRollbackWarning, res.RootFingerprint)
		return err
	case res.IdentityState != "" && res.IdentityState != store.IdentityStateNone:
		_, err := fmt.Fprintf(w, "Check the date: this backup was made with root %s after %d master-key rotation(s). If you rotated the master key after %s, %s. Restore a backup made after the rotation, or reset the affected Workers and enroll them again.\n",
			res.RootFingerprint, res.Transitions, res.Manifest.CreatedAt, store.RootRollbackWarning)
		return err
	}
	return nil
}
