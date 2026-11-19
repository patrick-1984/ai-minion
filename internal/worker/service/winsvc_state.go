package service

// WindowsServiceState is what a query-only look at the service control
// manager tells about the Worker's Windows service (v0.10.0). It changes
// nothing in the SCM.
type WindowsServiceState struct {
	// Installed is true when the service exists.
	Installed bool
	// Running is true when it runs (or is starting).
	Running bool
	// AutoStart is true when it starts by itself at boot.
	AutoStart bool
	// Account is the account it runs as ("LocalSystem"); empty when unknown.
	Account string
}
