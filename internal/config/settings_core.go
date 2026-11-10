package config

// Names of the core settings, for callers that look them up by key.
const (
	KeyDataDir   = "data_dir"
	KeyStateDir  = "state_dir"
	KeyLogLevel  = "log_level"
	KeyLogFormat = "log_format"
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
}
