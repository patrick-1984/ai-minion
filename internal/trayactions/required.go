package trayactions

// Required is the hand-maintained list of the tray's control ids (C12). It
// is written out here on purpose, not derived from [All]: the action table,
// the tray menu and the parity documentation are each tested against it, so a
// control that is dropped from the table, or added without a CLI equivalent,
// fails a test instead of passing silently. Change it only together with the
// menu design (06-architecture-v2 §5).
var Required = []string{
	// Status and keys (read from minion worker status --json).
	"status",
	"hubkey.show",
	"workerkey.show",
	"hub.paircode",

	// Pause, timed and until resumed, and resume.
	"pause.30m",
	"pause.2h",
	"pause.indef",
	"resume",

	// Lifecycle.
	"start",
	"stop",
	"restart",

	// Sessions shared with the Hub.
	"share.list",
	"share.all",
	"share.none",
	"share.select",
	"share.add",
	"share.remove",

	// Block new sessions created through Minion.
	"block.on",
	"block.off",

	// Hub setup and keys.
	"hub.open",
	"hub.join",
	"hub.join.resume",
	"hub.pair",
	"hub.cancelpair",
	"hub.leave",

	// Clipboard.
	"copy.hubkey",
	"copy.workerkey",

	// Set up the Worker (minion worker up, in a terminal).
	"setup",

	// The tray itself.
	"autostart.on",
	"autostart.off",
	"about.version",
	"about.commands",
	"about.docs",
	"quit",
}
