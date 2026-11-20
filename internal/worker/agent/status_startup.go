package agent

import (
	"fmt"
	"io"
)

// WriteStartupSummary prints the verified identities and current state after
// installation. The commands belong to the account and manager that started it.
func (r StatusReport) WriteStartupSummary(w io.Writer, commands []string) {
	state := r.Connection.State
	switch {
	case !r.Connection.Live:
		state = "live status unavailable"
	case r.Paused:
		state = "paused"
	case r.Mode == ModeSolo:
		state = "running (Solo)"
	case r.Approval == ApprovalPending:
		state = "waiting for approval"
	case r.PairingState == PairingPending:
		state = "waiting for pairing"
	}
	lines := []string{"The Worker runs in the background. State: " + state}
	if r.HubFingerprintWords != "" {
		lines = append(lines, "", "Hub key (must match the Hub's top bar):", "  "+r.HubFingerprintWords)
	} else if r.Mode == ModeHub {
		lines = append(lines, "Hub key: unavailable")
	}
	if r.Approval == ApprovalPending || r.PairingState == PairingPending {
		lines = append(lines, "", "Worker fingerprint (approve on the Hub's Workers page only if it shows this):",
			"  "+orDash(r.WorkerFingerprintWords, "unavailable"))
	}
	if r.Share.Configured {
		lines = append(lines, "", "Sharing: "+shareSummary(r.Share)+" (change it in the Hub or with minion worker config share)")
	}
	if r.Connection.LastError != "" {
		lines = append(lines, "Last connection error: "+r.Connection.LastError)
	}
	if len(commands) > 0 {
		lines = append(lines, "")
	}
	writeConsoleBox(w, "MINION WORKER", append(lines, commands...)...)
}

// shareSummary is the sharing choice in a few words.
func shareSummary(s StatusShare) string {
	switch s.Mode {
	case "all":
		return "all tmux sessions, also new ones"
	case "none":
		return "no session"
	case "later":
		return "nothing yet, choose in the Hub"
	case "select":
		return fmt.Sprintf("%d chosen session(s)", len(s.Sessions))
	}
	return s.Mode
}
