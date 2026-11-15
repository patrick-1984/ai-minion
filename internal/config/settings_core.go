package config

import (
	"fmt"
	"strconv"
)

// Names of the core settings, for callers that look them up by key.
const (
	KeyDataDir   = "data_dir"
	KeyStateDir  = "state_dir"
	KeyLogLevel  = "log_level"
	KeyLogFormat = "log_format"
	// KeyVerbose is the verbose switch (-v, --verbose, MINION_VERBOSE): it
	// turns the log level to debug unless log_level is set explicitly.
	KeyVerbose = "verbose"
)

func init() { declareCore(Default) }

// declareCore declares the settings every role has: the directories and the
// log options. Defaults follow the OS conventions of [PathEnv].
func declareCore(r *Registry) {
	sys := SystemPathEnv()
	r.Declare(Setting{
		Key:         KeyDataDir,
		Scope:       ScopeAny,
		Help:        "Directory for durable data such as the Hub database",
		DefaultFunc: sys.DataDir,
	})
	r.Declare(Setting{
		Key:         KeyStateDir,
		Scope:       ScopeAny,
		Help:        "Directory for Worker state: database, config file and logs",
		DefaultFunc: sys.StateDir,
	})
	r.Declare(Setting{
		Key:     KeyLogLevel,
		Scope:   ScopeAny,
		Help:    "Log level: debug, info, warn or error",
		Default: "info",
		Choices: []string{"debug", "info", "warn", "error"},
	})
	r.Declare(Setting{
		Key:     KeyLogFormat,
		Scope:   ScopeAny,
		Help:    "Log format: auto (JSON unless stderr is a terminal), json or text",
		Default: "auto",
		Choices: []string{"auto", "json", "text"},
	})
	r.Declare(Setting{
		Key:     KeyVerbose,
		Scope:   ScopeAny,
		Help:    "Verbose mode: true (or 1) logs at debug level unless log_level is set",
		Default: "false",
	})
}

// ResolveLogLevel returns the effective log level. A log_level that is set
// anywhere (flag, environment, config file, panel) wins; otherwise the
// verbose switch given on the command line (verboseFlag, nil when not given)
// decides, and an explicit false there wins over MINION_VERBOSE; otherwise
// the verbose setting turns the level to debug; otherwise the level is the
// default.
func ResolveLogLevel(r *Resolver, verboseFlag *bool) (string, error) {
	level, err := r.Resolve(KeyLogLevel)
	if err != nil {
		return "", err
	}
	if level.Source != SourceDefault {
		return level.Value, nil
	}
	if verboseFlag != nil {
		if *verboseFlag {
			return "debug", nil
		}
		return level.Value, nil
	}
	if _, ok := r.registry().Lookup(KeyVerbose); !ok {
		return level.Value, nil // a registry without the switch (tests)
	}
	v, err := r.Resolve(KeyVerbose)
	if err != nil {
		return "", err
	}
	on, err := strconv.ParseBool(v.Value)
	if err != nil {
		return "", fmt.Errorf("setting %q (%s): invalid value %q, expected true, false, 1 or 0", KeyVerbose, v.Source, v.Value)
	}
	if on {
		return "debug", nil
	}
	return level.Value, nil
}
