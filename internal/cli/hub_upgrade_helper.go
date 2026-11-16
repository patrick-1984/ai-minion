package cli

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/patrick-1984/ai-minion/internal/hub/selfupdate"
)

// hub upgrade-helper is the command of the helper container that the Hub
// starts for its Docker self-update (hub_update). It is hidden: people never
// run it.
func init() {
	Register(func(root *cobra.Command) {
		var container, image, channel, from string
		cmd := &cobra.Command{
			Use:    "upgrade-helper",
			Short:  "Replace the Hub container with a verified release (started by the Hub; not for manual use)",
			Hidden: true,
			Args:   cobra.NoArgs,
			RunE: func(cmd *cobra.Command, _ []string) error {
				log := slog.New(slog.NewJSONHandler(cmd.ErrOrStderr(), nil))
				d := selfupdate.NewDocker(selfupdate.DefaultSocket)
				ctx, cancel := context.WithTimeout(cmd.Context(), 15*time.Minute)
				defer cancel()
				old, err := d.Inspect(ctx, container)
				if err == nil {
					// The lock of the update lives in the Hub's data directory,
					// mounted here at the same path; it ends with this helper.
					for _, kv := range old.Env() {
						if len(kv) > 16 && kv[:16] == "MINION_DATA_DIR=" {
							defer func() { _ = os.Remove(filepath.Join(kv[16:], selfupdate.LockFile)) }()
						}
					}
				}
				repo, tag := splitChannel(channel)
				return selfupdate.Replace(ctx, d, selfupdate.ReplaceParams{
					OldID: container, ImageRef: image, ChannelRepo: repo, ChannelTag: tag, From: from, Log: log,
					ReadyTimeout: 2 * time.Minute, StopTimeout: 30 * time.Second,
				})
			},
		}
		cmd.Flags().StringVar(&container, "container", "", "id of the Hub container to replace")
		cmd.Flags().StringVar(&image, "image", "", "verified image, repository@sha256:...")
		cmd.Flags().StringVar(&channel, "channel", "", "repository:tag the Hub container was created from")
		cmd.Flags().StringVar(&from, "from", "", "release the Hub runs now")
		hubCommand(root).AddCommand(cmd)
	})
}

func splitChannel(ref string) (repo, tag string) {
	for i := len(ref) - 1; i >= 0; i-- {
		switch ref[i] {
		case ':':
			return ref[:i], ref[i+1:]
		case '/':
			return ref, ""
		}
	}
	return ref, ""
}
