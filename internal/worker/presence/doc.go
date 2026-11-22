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
//	      pointers to the presence directory of a profile that is not in that
//	      default place. They are written to the service's own default and, on
//	      Linux, to ~/.local/state/minion (the default of a login without
//	      XDG_STATE_HOME). A reader also follows the state directories the
//	      account's unit, drop-ins and login entry pin
//	root Worker (uid 0), root-owned directories 0755, records 0644:
//	  Linux  /run/minion-presence/<instance>.json (and <instance>.usage.json)
//	  macOS  /var/run/minion-presence/<instance>.json
//	  public files are staged in the root-only directory .stage (0700) of the
//	  same directory and renamed, so no world-readable temporary file exists.
//	  The staging directory is verified (root-owned, 0700) and cleaned of
//	  foreign entries at every start; temporary files are created O_EXCL and
//	  O_NOFOLLOW under random names. The note that ties a state directory to
//	  its instance is keyed by the device and inode of the physical state
//	  directory, so a reset or replacement through any alias finds it
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
// stale and running distinctly. A record may carry hub_link (connected,
// registration_pending, disconnected, none), the coarse state of the Hub
// connection; the publisher sets it with SetHubLink.
//
// On Windows a program that reads a published file must open it with
// FILE_SHARE_DELETE (share read, write and delete): without it the unlink or
// replacement that follows a reset, a revocation or a heartbeat is blocked.
// The publisher retries a blocked unlink for about a second, keeps it pending
// in a marker file and finishes it with the next heartbeat or start. The
// Reader of this package opens with that sharing. /run/minion (the socket directory) is never
// touched.
package presence
