package cli

import (
	"fmt"
	"io"

	"github.com/spf13/pflag"

	"github.com/patrick-1984/ai-minion/internal/client"
	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/version"
)

// The Hub client of the op commands and of `minion mcp` (AD-22, Story 6.9).
// With a Hub configured they call it over its REST API with an API key from a
// file; without one they use the Worker's local control socket.

// Flags of the Hub client. There is no flag for the API key on purpose: it is
// read only from the file named by MINION_API_KEY_FILE.
const (
	flagHubURL      = "hub-url"
	flagHubCAFile   = "hub-ca-file"
	flagHubInsecure = "hub-insecure"
	flagHubWorker   = "hub-worker"
)

// hubFlagNames maps a client setting to its flag.
var hubFlagNames = map[string]string{
	config.KeyHubURL:      flagHubURL,
	config.KeyHubCAFile:   flagHubCAFile,
	config.KeyHubInsecure: flagHubInsecure,
	config.KeyHubWorker:   flagHubWorker,
}

// hubFlagHelp is the one-line help of each flag; the settings carry the
// longer text.
var hubFlagHelp = map[string]string{
	config.KeyHubURL:      "call this Hub over its REST API instead of the local Worker; the API key is read from the file named by " + config.APIKeyFileEnv,
	config.KeyHubCAFile:   "PEM file with the private CA of the Hub's certificate",
	config.KeyHubInsecure: "skip the verification of the Hub's certificate for this client only (warns)",
	config.KeyHubWorker:   "id (UUIDv7) of the Worker that runs operations whose arguments name none (sessions_list, session_create)",
}

// addHubFlags adds the Hub client flags to fs. A flag that already exists (an
// argument of an op with the same name) is left alone.
func addHubFlags(fs *pflag.FlagSet) {
	for _, key := range []string{config.KeyHubURL, config.KeyHubCAFile, config.KeyHubInsecure, config.KeyHubWorker} {
		name := hubFlagNames[key]
		if fs.Lookup(name) != nil {
			continue
		}
		s, _ := config.Client.Lookup(key)
		fs.String(name, "", fmt.Sprintf("%s (env %s)", hubFlagHelp[key], s.EnvName()))
		if key == config.KeyHubInsecure {
			fs.Lookup(name).NoOptDefVal = "true"
		}
	}
}

// hubFlags is the [config.FlagSource] of the Hub client flags: only a flag the
// user set is a source, so a flag's empty default never wins over the
// environment.
type hubFlags struct{ fs *pflag.FlagSet }

func (h hubFlags) Lookup(key string) (string, bool, error) {
	name, ok := hubFlagNames[key]
	if !ok || h.fs == nil {
		return "", false, nil
	}
	f := h.fs.Lookup(name)
	if f == nil || !f.Changed {
		return "", false, nil
	}
	return f.Value.String(), true, nil
}

// resolveHubSettings resolves the Hub client settings for one run: flag, then
// environment, then the config file of the Worker's state directory, then the
// default. A config file that cannot be read is an error, not "no Hub": the
// user may have configured the Hub in it, and the command must not run on the
// local Worker instead.
func resolveHubSettings(fs *pflag.FlagSet, lookupEnv func(string) (string, bool), load func(stateDir string) (config.FileSource, error)) (config.ClientHub, error) {
	var file config.FileSource
	if load != nil {
		base := &config.Resolver{Role: config.ScopeWorker, LookupEnv: lookupEnv}
		f, err := loadConfigFile(base, load)
		if err != nil {
			return config.ClientHub{}, fmt.Errorf("read the config file: %w", err)
		}
		file = f
	}
	res := &config.Resolver{
		Registry:  config.Client,
		Role:      config.ScopeWorker,
		Flags:     hubFlags{fs: fs},
		File:      file,
		LookupEnv: lookupEnv,
	}
	return config.ResolveClientHub(res)
}

// hubBackend builds the Hub backend of cfg and sends the permanent warning of
// an insecure client to warn.
func hubBackend(cfg config.ClientHub, warn io.Writer) (client.Backend, error) {
	hc := client.HubConfig{URL: cfg.URL, APIKeyFile: cfg.APIKeyFile, CAFile: cfg.CAFile, Insecure: cfg.Insecure, Worker: cfg.Worker}
	b, _, err := client.Select(hc, nil,
		client.WithHubUserAgent("minion/"+version.Get().Version),
		client.WithHubWarn(func(msg string) { _, _ = fmt.Fprintln(warn, msg) }))
	return b, err
}
