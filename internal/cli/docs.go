package cli

import (
	"context"
	"fmt"
	"runtime"

	"github.com/spf13/cobra"
)

func init() {
	Register(func(root *cobra.Command) {
		root.AddCommand(newDocsCmd(defaultDocsDeps()))
	})
}

// docsBaseURL is the documentation folder of the main branch on GitHub.
const docsBaseURL = "https://github.com/patrick-1984/ai-minion/blob/main/docs/"

// The guides "minion docs" opens.
const (
	docsTopicTray   = "tray"
	docsTopicWorker = "worker"
)

// docsDeps are the inputs of "minion docs" that tests replace.
type docsDeps struct {
	goos string
	// run starts the program name with args and does not wait for it.
	run func(ctx context.Context, name string, args ...string) error
}

func defaultDocsDeps() docsDeps {
	return docsDeps{goos: runtime.GOOS, run: startDetached}
}

// docsURL returns the guide of topic: the tray guide, or the Worker install
// guide of goos.
func docsURL(topic, goos string) string {
	if topic == docsTopicWorker {
		switch goos {
		case "darwin":
			return docsBaseURL + "install/worker-macos.md"
		case "windows":
			return docsBaseURL + "install/worker-windows-preview.md"
		}
		return docsBaseURL + "install/worker-linux.md"
	}
	return docsBaseURL + "guides/tray.md"
}

// docsResult is the JSON result of "minion docs".
type docsResult struct {
	URL    string `json:"url"`
	Opened bool   `json:"opened"`
}

func newDocsCmd(deps docsDeps) *cobra.Command {
	var asJSON, printOnly bool
	cmd := &cobra.Command{
		Use:       "docs [tray|worker]",
		Short:     "Open the documentation in the browser",
		ValidArgs: []string{docsTopicTray, docsTopicWorker},
		Long: `Open a guide of the documentation on GitHub (main branch) in the browser:

  tray    the tray app guide (docs/guides/tray.md; the default)
  worker  the Worker install guide of this system (Linux, macOS or Windows)

It starts xdg-open (Linux), open (macOS) or the Windows URL handler with the
URL as its only argument, like "minion worker open-hub".

--print prints the URL and opens nothing. --json prints one JSON object
{"ok","code","message","url","opened"}.`,
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) > 1 || (len(args) == 1 && args[0] != docsTopicTray && args[0] != docsTopicWorker) {
				return usagef("docs takes at most one argument: tray or worker")
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			topic := docsTopicTray
			if len(args) == 1 {
				topic = args[0]
			}
			res, err := runDocs(cmd, deps, topic, printOnly)
			if !asJSON {
				_, _ = fmt.Fprintln(cmd.OutOrStdout(), res.URL)
			}
			return emitResult(cmd.OutOrStdout(), cmd.ErrOrStderr(), asJSON, res, err)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print one JSON result object")
	cmd.Flags().BoolVar(&printOnly, "print", false, "print the URL, do not open a browser")
	return cmd
}

func runDocs(cmd *cobra.Command, deps docsDeps, topic string, printOnly bool) (docsResult, error) {
	res := docsResult{URL: docsURL(topic, deps.goos)}
	if printOnly {
		return res, nil
	}
	name, args := urlOpener(deps.goos, res.URL)
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	if err := deps.run(ctx, name, args...); err != nil {
		return res, fmt.Errorf("cannot start %s to open the documentation (open %s yourself): %w", name, res.URL, err)
	}
	res.Opened = true
	return res, nil
}
