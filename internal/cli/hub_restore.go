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
		cmd := &cobra.Command{
			Use:   "restore <archive>",
			Short: "Restore the Hub data directory from a backup archive (Hub stopped)",
			Long: `Restore the Hub data directory from an archive written by "minion hub backup".
Stop the Hub first.

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
				return runHubRestore(cmd.Context(), cmd.OutOrStdout(), hubBackupDeps{}, dataDir, args[0], force)
			},
		}
		cmd.Flags().StringVar(&dataDir, "data-dir", "", "Hub data directory (default: MINION_DATA_DIR or the OS default)")
		cmd.Flags().BoolVar(&force, "force", false, "replace the files of a non-empty data directory")
		hubCommand(root).AddCommand(cmd)
	})
}

func runHubRestore(ctx context.Context, w io.Writer, d hubBackupDeps, dataDirFlag, archive string, force bool) error {
	dir, err := resolveDataDir(dataDirFlag, d)
	if err != nil {
		return err
	}
	res, err := store.Restore(ctx, archive, dir, force)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "Restored %d files into %s (schema version %d, written by minion %s).\nStart the Hub with \"minion hub serve\".\n",
		len(res.Files), res.Dir, res.Manifest.SchemaVersion, res.Manifest.Version); err != nil {
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
