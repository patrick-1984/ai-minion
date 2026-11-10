// Package config holds the settings registry and its precedence rules
// (flags, environment, files, defaults) (AD-19). It is part of the shared
// kernel and imports no other internal package.
//
// Every setting is declared once in a [Registry] (see [Setting]); the
// environment name, the flag name and the config-file key all derive from the
// one key. A [Resolver] reads the sources for one scope:
//
//   - Hub scope: environment, then the panel (a [PanelSource], implemented by
//     the Hub store), then the default.
//   - Worker scope: flag, then environment, then the config file, then the
//     default.
//
// Secret settings are never read from a plain environment variable (the
// declared exceptions aside); they come from a "<NAME>_FILE" file, the panel
// or the config file. See [ReadSecretFile].
//
// The package also locates the OS data, state and runtime directories
// ([PathEnv]) and reads and writes the Worker config file ([FileSource]).
//
// Owner: Story 1.4 (settings registry, precedence, config file, paths).
package config
