// Package setup implements the first-run setup wizard (terminal and local web UI) (AD-19, AD-21).
//
// Story 4.13 (this code) owns three files:
//
//   - wizard.go is the step engine, shared with the web UI of Story 4.14. It
//     holds the answers, enforces the order of the three steps (what to share,
//     the Hub address, the pairing code) and does no input or output of its
//     own. Everything outside of it is a port: [SessionLister], [Persister],
//     [HubChecker] and [Pairing].
//   - conncheck.go checks a Hub address stage by stage (DNS, TCP, TLS,
//     protocol) with results a non-technical user can read.
//   - terminal.go drives the engine as a line-based dialog.
//
// Rules the code keeps:
//
//   - Nothing is saved before the Hub step is done; a wizard that ends earlier
//     changes nothing.
//   - Answers that flags or environment already give are never asked (R85).
//   - Without a terminal the dialog reads no input and offers no pairing.
//   - The share choice goes through the Worker store, whose policy decides what
//     the Hub and local callers see. "Later" shares nothing. Sessions are
//     never renamed, closed or written to, and their content is never read.
//   - The TLS stage never calls a certificate trusted that this computer did
//     not verify. The Worker itself does not depend on the Hub's certificate
//     (A-15): the channel is authenticated inside, by the pin or the pairing code.
//   - Pairing is the Worker's existing flow ([github.com/patrick-1984/ai-minion/internal/worker/agent.Pairer]);
//     the wizard adds no cryptography.
//
// Story 4.14 adds the local web UI on the same engine.
//
// Owner: Story 4.13.
package setup
