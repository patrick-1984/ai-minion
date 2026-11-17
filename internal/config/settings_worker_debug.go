package config

import (
	"fmt"
	"strconv"
)

// Names of the Worker debug settings (v0.10.0), for callers that look them up
// by key.
const (
	// KeyDebug turns the Worker's log level to debug (--debug, MINION_DEBUG):
	// the same as log_level=debug.
	KeyDebug = "debug"
	// KeyAllowDebugRemote lets the Worker's owner and the Hub's administrators
	// switch debug logging on and read the Worker's recent log through the Hub
	// (worker_debug, worker_logs). Off by default: the local opt-in.
	KeyAllowDebugRemote = "allow_debug_remote"
)

// boolChoices are the accepted spellings of a Worker debug switch.
var boolChoices = []string{"true", "false", "1", "0"}

func init() { declareWorkerDebug(Default) }

func declareWorkerDebug(r *Registry) {
	r.Declare(Setting{
		Key:     KeyDebug,
		Scope:   ScopeWorker,
		Help:    "Debug logging: true (or 1) logs at debug level, like log_level=debug, so that a problem can be diagnosed",
		Default: "false",
		Choices: boolChoices,
	})
	r.Declare(Setting{
		Key:   KeyAllowDebugRemote,
		Scope: ScopeWorker,
		Help: "Let the Worker's owner and the Hub's administrators switch debug logging on for a while and read the " +
			"Worker's recent log through the Hub (true or false; off by default)",
		Default: "false",
		Choices: boolChoices,
	})
}

// ResolveBool resolves a true/false setting (true, false, 1 or 0).
func ResolveBool(r *Resolver, key string) (bool, error) {
	v, err := r.Resolve(key)
	if err != nil {
		return false, err
	}
	if v.Value == "" {
		return false, nil
	}
	on, err := strconv.ParseBool(v.Value)
	if err != nil {
		return false, fmt.Errorf("setting %q (%s): invalid value %q, expected true, false, 1 or 0", key, v.Source, v.Value)
	}
	return on, nil
}

// debugOn reports whether the debug switch is on for r. A resolver whose role
// or registry lacks the switch reports false.
func debugOn(r *Resolver) (bool, error) {
	s, ok := r.registry().Lookup(KeyDebug)
	if !ok || !s.Scope.appliesTo(r.Role) {
		return false, nil
	}
	return ResolveBool(r, KeyDebug)
}
