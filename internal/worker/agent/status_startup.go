package agent

import "io"

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
	lines := []string{"State: " + state}
	if r.HubFingerprintWords != "" {
		lines = append(lines, "Hub key: "+r.HubFingerprintWords)
	} else if r.Mode == ModeHub {
		lines = append(lines, "Hub key: unavailable")
	}
	if r.Approval == ApprovalPending || r.PairingState == PairingPending {
		lines = append(lines, "Worker fingerprint: "+orDash(r.WorkerFingerprintWords, "unavailable"))
		if r.Approval == ApprovalPending {
			lines = append(lines, "Compare the fingerprint on the Hub's Workers page before approving.")
		}
	}
	if r.Connection.LastError != "" {
		lines = append(lines, "Last connection error: "+r.Connection.LastError)
	}
	if len(commands) > 0 {
		lines = append(lines, "")
	}
	writeConsoleBox(w, "MINION WORKER", append(lines, commands...)...)
}
