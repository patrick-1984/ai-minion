package config

// File transfer on the Hub (files.v1, v0.24.0). The Worker has its own local
// switch (file_transfer, a Worker setting); the Hub-wide policy therefore has
// its own key, file_transfer_policy, so that the two never share a name.
const (
	// KeyFileTransferPolicy is the Hub-wide file transfer policy, named from
	// the initiating caller: on, pull_only, push_only, off or admins_only
	// (ops.FileModeOn, ops.FilePolicy*). Environment MINION_FILE_TRANSFER_POLICY.
	KeyFileTransferPolicy = "file_transfer_policy"
	// KeyFileTransferRetentionDays is how long the Hub keeps the transfer
	// history (file_transfers records), in days.
	KeyFileTransferRetentionDays = "file_transfer_retention_days"
)

func init() { declareHubFiles(Default) }

func declareHubFiles(r *Registry) {
	r.Declare(Setting{Key: KeyFileTransferPolicy, Scope: ScopeHub, Default: "on",
		Choices: []string{"on", "pull_only", "push_only", "off", "admins_only"},
		Help: "File transfer between session folders on this Hub: on, pull_only, push_only, off, or admins_only " +
			"(only administrators start transfers); pull and push are named from the caller that starts the transfer. " +
			"Each Worker's own file_transfer switch still applies"})
	r.Declare(Setting{Key: KeyFileTransferRetentionDays, Scope: ScopeHub, Default: "90",
		Help: "How many days the Hub keeps the history of file transfers"})
}
