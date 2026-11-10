// Package ops is the single operation catalog from which REST routes, wire op names, MCP tools and CLI subcommands are derived, together with the screen model and the action contract types (AD-2, AD-14, AD-15). It is part of the shared kernel.
//
// The package is split by concern:
//
//   - catalog.go: the Op declaration, the registry (Register, Lookup, All) and the derived names.
//   - actions.go: the accepted actions (read, operate, message, manage) and accepted action sets.
//   - contract.go: outcomes, guard classes (KEEP, WARN, FORCE), WriteResult, request ids and the shared argument error.
//   - screen.go: the Screen model (UTF-8 lines, SGR style spans, cursor).
//   - target.go: Target addressing and the human form parser (AD-18).
//   - worker_ops.go: the release-1 Worker operations with their argument and result types.
//
// Other stories add operation groups by calling Register from an init function in
// their own file. REST, wire, MCP and CLI pick new operations up from All.
//
// Error and reason codes are declared only in internal/proto/codes.go. This
// package never invents a code; it exposes sentinel errors that the adapter
// edge maps to one API code.
//
// Owner: Story 1.3.
package ops
