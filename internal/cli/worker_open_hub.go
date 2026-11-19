package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"time"

	"github.com/spf13/cobra"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/worker/agent"
)

func init() {
	Register(func(root *cobra.Command) {
		workerCommand(root).AddCommand(newWorkerOpenHubCmd(defaultWorkerOpenHubDeps()))
	})
}

// workerOpenHubDeps are the inputs of "minion worker open-hub" that tests
// replace.
type workerOpenHubDeps struct {
	goos       string
	lookupEnv  func(string) (string, bool)
	loadConfig func(stateDir string) (config.FileSource, error)
	// run starts the program name with args and does not wait for it.
	run func(ctx context.Context, name string, args ...string) error
}

func defaultWorkerOpenHubDeps() workerOpenHubDeps {
	return workerOpenHubDeps{
		goos:       runtime.GOOS,
		lookupEnv:  os.LookupEnv,
		loadConfig: loadWorkerConfig,
		run:        startDetached,
	}
}

// startDetached starts name with args (no shell) and leaves it running: a
// browser started by the opener may outlive this command.
func startDetached(_ context.Context, name string, args ...string) error {
	cmd := exec.Command(name, args...) //nolint:gosec // a fixed opener program; the URL is one argument, never a shell word
	if err := cmd.Start(); err != nil {
		return err
	}
	// An opener that cannot dispatch the URL exits nonzero at once: wait a
	// short while for that. One that keeps running (a browser it started) is
	// released.
	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()
	select {
	case err := <-waited:
		if err != nil {
			return fmt.Errorf("%s did not open the URL: %w", name, err)
		}
		return nil
	case <-time.After(openerGrace):
		return nil
	}
}

// openerGrace is how long startDetached waits for an opener to fail.
var openerGrace = 2 * time.Second

// urlOpener returns the program and arguments that open url in the browser
// of the desktop on goos: xdg-open, open, or the URL handler of Windows.
func urlOpener(goos, url string) (string, []string) {
	switch goos {
	case "darwin":
		return "open", []string{url}
	case "windows":
		return "rundll32", []string{"url.dll,FileProtocolHandler", url}
	}
	return "xdg-open", []string{url}
}

// workerOpenHubResult is the JSON result of "worker open-hub".
type workerOpenHubResult struct {
	// URL is the browser URL of the Hub (status hub_web_url).
	URL string `json:"url"`
	// Opened is true when the browser opener was started.
	Opened bool `json:"opened"`
}

func newWorkerOpenHubCmd(deps workerOpenHubDeps) *cobra.Command {
	var asJSON, printOnly bool
	cmd := &cobra.Command{
		Use:   "open-hub",
		Short: "Open the Hub of this Worker in the browser",
		Long: `Open the Hub this Worker is configured for in the browser: the Hub URL with
http or https (ws and wss become http and https), the base path kept, any user
name, password, query and fragment left out. It starts xdg-open (Linux), open
(macOS) or the Windows URL handler with that URL as its only argument.

--print prints the URL and opens nothing. Without a Hub URL (a Solo Worker) it
exits with status 1.

--json prints one JSON object {"ok","code","message","url","opened"}.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			res, err := runWorkerOpenHub(cmd, deps, printOnly)
			if !asJSON && res.URL != "" {
				_, _ = fmt.Fprintln(cmd.OutOrStdout(), res.URL)
			}
			return emitResult(cmd.OutOrStdout(), cmd.ErrOrStderr(), asJSON, res, err)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print one JSON result object")
	cmd.Flags().BoolVar(&printOnly, "print", false, "print the URL, do not open a browser")
	addNamedSettingFlags(cmd, config.Default, config.KeyStateDir, config.KeyServer)
	return cmd
}

func runWorkerOpenHub(cmd *cobra.Command, deps workerOpenHubDeps, printOnly bool) (workerOpenHubResult, error) {
	var res workerOpenHubResult
	resolver := &config.Resolver{Role: config.ScopeWorker, Flags: changedFlags{cmd: cmd, reg: config.Default}, LookupEnv: deps.lookupEnv}
	if deps.loadConfig != nil {
		file, err := loadConfigFile(resolver, deps.loadConfig)
		if err != nil {
			return res, err
		}
		resolver.File = file
	}
	hub, err := config.ResolveWorkerHub(resolver)
	if err != nil {
		return res, err
	}
	if hub.Server == "" {
		return res, errors.New("no Hub URL is configured (a Solo Worker); join a Hub first: minion worker join <url>")
	}
	res.URL = agent.HubWebURL(hub.Server)
	if res.URL == "" {
		return res, errors.New("the configured Hub URL is not an http, https, ws or wss URL")
	}
	if printOnly {
		return res, nil
	}
	name, args := urlOpener(deps.goos, res.URL)
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	if err := deps.run(ctx, name, args...); err != nil {
		return res, fmt.Errorf("cannot start %s to open the Hub (open %s yourself): %w", name, res.URL, err)
	}
	res.Opened = true
	return res, nil
}
