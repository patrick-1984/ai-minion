// Package trayactions is the one action table of the tray app: every control
// of the tray menu with the command line that does the same thing, so that
// every tray action also works through the CLI (owner rule R6, parity).
//
// [All] lists the actions. Each [Action] names the program it runs ("minion"
// or "minion-tray"), its argv with placeholders such as {state_dir} (C7: every
// "minion" mutation is bound to the tray's profile), what goes to its standard
// input (a join token, never an argument), the confirmation it needs, the
// closed set of elevated argv a Windows tray may run ([ElevatedArgv], E1 and
// E2), the kind of effect and the status JSON path (or other observation)
// that proves the effect. [Required] is a separate, hand-maintained list of
// control ids; the table, the tray menu and the documentation are each tested
// against it (C12). [ParityMarkdown] renders the parity table that
// docs/guides/tray.md carries (scripts/gentraydocs writes it) and that
// "minion tray-actions --json" and "minion-tray commands" print.
//
// The package is pure Go and imports only the standard library, so that the
// headless minion binary, the tray and the docs generator can all import it
// (acceptance 7 of W11; TestImportsOnlyStandardLibrary keeps it so).
//
// Owner: Story W11 (tray PR-1: action table, required controls, parity docs).
package trayactions
