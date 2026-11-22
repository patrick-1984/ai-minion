// Package presence publishes and reads the local presence record of a running
// Worker daemon (minion.presence/1, docs/schemas/presence.schema.json) and the
// latest verified usage-meters document next to it (A79/A80 read-back).
//
// Where the files live (every name is one file per daemon instance, never one
// shared filename):
//
//	user Worker (any non-root uid, also a systemd User=<user> unit):
//	  <state dir>/presence/<instance>.json         0600, directory 0700
//	  <state dir>/presence/<instance>.usage.json   0600
//	  <default state dir>/presence-index/<hash>.json   0600, directory 0700,
//	      only when the state directory is not the default one: it points to
//	      the presence directory of a relocated profile (--state-dir)
//	root Worker (uid 0), root-owned directories 0755, records 0644:
//	  Linux  /run/minion-presence/<instance>.json (and <instance>.usage.json)
//	  macOS  /var/run/minion-presence/<instance>.json
//	  public files are staged in the root-only directory .stage (0700) of the
//	  same directory and renamed, so no world-readable temporary file exists
//	Windows: the private variant under the user's state directory (protected
//	  DACL); there is no root Worker and no public directory.
//
// The default state directory is the one of [config.PathEnv.StateDir] for the
// account: $XDG_STATE_HOME/minion or ~/.local/state/minion on Linux,
// ~/Library/Application Support/minion on macOS, %LOCALAPPDATA%\minion\state
// on Windows.
//
// The [Publisher] writes a running heartbeat every 30 s (stale after 90 s) and
// a stopped marker on a clean stop; a crash leaves a record that goes stale.
// The [Reader] is the secure consumer: it enumerates the private and the
// public candidates independently, opens each file once, validates the open
// handle (regular file, expected owner, expected mode), reads a bounded
// amount, checks the schema and the age, and reports unavailable, stopped,
// stale and running distinctly. /run/minion (the socket directory) is never
// touched.
package presence
