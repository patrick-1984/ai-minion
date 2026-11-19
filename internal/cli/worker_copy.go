package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/worker/agent"
)

func init() {
	Register(func(root *cobra.Command) {
		workerCommand(root).AddCommand(newWorkerCopyCmd(defaultWorkerCopyDeps()))
	})
}

// The keys "minion worker copy" copies.
const (
	copyKeyHub    = "hub-key"
	copyKeyWorker = "worker-key"
)

// copyKeyNames are the names of the keys in human text.
var copyKeyNames = map[string]string{copyKeyHub: "Hub key", copyKeyWorker: "Worker key"}

// clipboardTimeout bounds one clipboard program. wl-copy and xclip fork a
// child that serves the selection; the program itself returns at once.
const clipboardTimeout = 10 * time.Second

// workerCopyDeps are the inputs of "minion worker copy" that tests replace.
type workerCopyDeps struct {
	goos       string
	lookupEnv  func(string) (string, bool)
	loadConfig func(stateDir string) (config.FileSource, error)
	// lookPath reports whether a program is installed (exec.LookPath).
	lookPath func(name string) (string, error)
	// runStdin runs name with args, writes stdin to its standard input and
	// waits for it. The copied value is only ever in stdin, never in args.
	runStdin func(ctx context.Context, stdin []byte, name string, args ...string) error
	// hubKeyWords and workerKeyWords read the words of a key in a state
	// directory; an error means there is nothing to copy.
	hubKeyWords    func(ctx context.Context, stateDir string) (string, error)
	workerKeyWords func(stateDir string) (string, error)
}

func defaultWorkerCopyDeps() workerCopyDeps {
	return workerCopyDeps{
		goos:           runtime.GOOS,
		lookupEnv:      os.LookupEnv,
		loadConfig:     loadWorkerConfig,
		lookPath:       exec.LookPath,
		runStdin:       runWithStdin,
		hubKeyWords:    readHubKeyWords,
		workerKeyWords: readWorkerKeyWords,
	}
}

// runWithStdin runs name with args (no shell), stdin on its standard input.
// Its output is discarded: a clipboard program that leaves a child running
// (wl-copy, xclip) must not keep a pipe of this process open.
func runWithStdin(ctx context.Context, stdin []byte, name string, args ...string) error {
	ctx, cancel := context.WithTimeout(ctx, clipboardTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...) //nolint:gosec // a fixed clipboard program; the value goes to stdin
	cmd.Stdin = bytes.NewReader(stdin)
	return cmd.Run()
}

// readHubKeyWords returns the words of the pinned Hub key (hub_key.words of
// "minion worker status --json").
func readHubKeyWords(ctx context.Context, stateDir string) (string, error) {
	hk := agent.ReadHubKey(ctx, stateDir)
	if hk.Words == "" {
		return "", errors.New(hk.Line)
	}
	return hk.Words, nil
}

// readWorkerKeyWords returns the words of this Worker's own key
// (worker_fingerprint_words of "minion worker status --json").
func readWorkerKeyWords(stateDir string) (string, error) {
	k, err := agent.NewEnrollments(nil, stateDir).LoadKey()
	switch {
	case errors.Is(err, agent.ErrNoWorkerKey):
		return "", errors.New("this Worker has no key yet: none is created before the first pairing")
	case err != nil:
		return "", fmt.Errorf("cannot read the Worker key: %w", err)
	}
	return k.Fingerprint().Words(), nil
}

// clipboardProgram is one clipboard program and its fixed arguments.
type clipboardProgram struct {
	name string
	args []string
}

// clipboardPrograms returns the clipboard programs of goos in the order they
// are tried: clip.exe on Windows, pbcopy on macOS, and wl-copy, xclip, xsel
// and (under WSL) clip.exe elsewhere. Each reads the value from stdin.
func clipboardPrograms(goos string) []clipboardProgram {
	switch goos {
	case "windows":
		return []clipboardProgram{{name: "clip.exe"}}
	case "darwin":
		return []clipboardProgram{{name: "pbcopy"}}
	}
	return []clipboardProgram{
		{name: "wl-copy"},
		{name: "xclip", args: []string{"-selection", "clipboard"}},
		{name: "xsel", args: []string{"--clipboard", "--input"}},
		{name: "clip.exe"},
	}
}

// workerCopyResult is the JSON result of "worker copy".
type workerCopyResult struct {
	// Key is hub-key or worker-key.
	Key string `json:"key"`
	// Value is the copied text, present only with --print.
	Value string `json:"value,omitempty"`
	// Copied is true when a clipboard program took the value.
	Copied bool `json:"copied"`
	// Program is the clipboard program that took it.
	Program string `json:"program,omitempty"`
}

func newWorkerCopyCmd(deps workerCopyDeps) *cobra.Command {
	var asJSON, printValue bool
	cmd := &cobra.Command{
		Use:       "copy hub-key|worker-key",
		Short:     "Copy the words of the Hub key or of the Worker key to the clipboard",
		ValidArgs: []string{copyKeyHub, copyKeyWorker},
		Long: `Copy a key of this Worker to the clipboard, as the words the tray and the
status show (hub_key.words and worker_fingerprint_words of "minion worker status
--json"), not the 52-character fingerprint:

  hub-key     the Hub key this Worker pinned (compare it with the Hub's top bar)
  worker-key  this Worker's own key (the Hub shows it on the Worker's page)

The words are written to the standard input of the first clipboard program that
works: clip.exe (Windows), pbcopy (macOS), or wl-copy, xclip, xsel and clip.exe
(WSL) elsewhere. They are never a program argument. Without a working
clipboard program the exit status is 1.

--print also prints the words on stdout (also when nothing could be copied).
--json prints one JSON object {"ok","code","message","key","copied","program"},
with "value" only when --print is given.`,
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) != 1 || (args[0] != copyKeyHub && args[0] != copyKeyWorker) {
				return usagef("worker copy needs one argument: hub-key or worker-key")
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			res, value, err := runWorkerCopy(cmd, deps, args[0])
			if printValue && value != "" {
				res.Value = value
				if !asJSON {
					_, _ = fmt.Fprintln(cmd.OutOrStdout(), value)
				}
			}
			if !asJSON && res.Copied {
				_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Copied the words of the %s with %s.\n", copyKeyNames[args[0]], res.Program)
			}
			return emitResult(cmd.OutOrStdout(), cmd.ErrOrStderr(), asJSON, res, err)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print one JSON result object")
	cmd.Flags().BoolVar(&printValue, "print", false, "also print the words on stdout")
	addNamedSettingFlags(cmd, config.Default, config.KeyStateDir)
	return cmd
}

// runWorkerCopy reads the words of key and hands them to a clipboard
// program. It returns the words also when copying failed, so that --print can
// show them.
func runWorkerCopy(cmd *cobra.Command, deps workerCopyDeps, key string) (workerCopyResult, string, error) {
	res := workerCopyResult{Key: key}
	resolver := &config.Resolver{Role: config.ScopeWorker, Flags: changedFlags{cmd: cmd, reg: config.Default}, LookupEnv: deps.lookupEnv}
	if deps.loadConfig != nil {
		file, err := loadConfigFile(resolver, deps.loadConfig)
		if err != nil {
			return res, "", err
		}
		resolver.File = file
	}
	core, err := config.ResolveWorkerCore(resolver)
	if err != nil {
		return res, "", err
	}
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	var value string
	if key == copyKeyHub {
		value, err = deps.hubKeyWords(ctx, core.StateDir)
	} else {
		value, err = deps.workerKeyWords(core.StateDir)
	}
	if err != nil {
		return res, "", err
	}
	var failures []string
	for _, p := range clipboardPrograms(deps.goos) {
		if _, lerr := deps.lookPath(p.name); lerr != nil {
			continue
		}
		if rerr := deps.runStdin(ctx, []byte(value), p.name, p.args...); rerr != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", p.name, rerr))
			continue
		}
		res.Copied, res.Program = true, p.name
		return res, value, nil
	}
	names := make([]string, 0, 5)
	for _, p := range clipboardPrograms(deps.goos) {
		names = append(names, p.name)
	}
	msg := "no clipboard program found (tried " + strings.Join(names, ", ") + ")"
	if len(failures) > 0 {
		msg = "no clipboard program worked (" + strings.Join(failures, "; ") + ")"
	}
	return res, value, errors.New(msg + "; --print shows the words")
}
