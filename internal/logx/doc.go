// Package logx configures structured logging on log/slog and provides the
// Secret type that keeps secrets out of output (AD-20). It is part of the
// shared kernel and imports no other internal package.
//
// [New] builds a logger that writes JSON when the output is not a terminal
// and text when it is (overridable with [Options.Format]). [Secret] renders
// as "[redacted]" through slog, fmt and JSON, so a secret that reaches a log
// call by mistake is not written.
//
// The rule behind it: terminal content, message bodies, passwords, keys and
// secrets are never logged. The only exceptions, printed for the human and
// not logged, are the pairing code and the generated initial admin password.
//
// Owner: Story 1.4 (slog setup, Secret).
package logx
