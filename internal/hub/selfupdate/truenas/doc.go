// Package truenas is the host executor that updates the Minion Hub running
// as a TrueNAS custom app (design A65, v3). It is driven by
// `minion hub-updater`, which an owner registers as a TrueNAS Cron Task that
// runs as root on the NAS host. The Hub holds no NAS credential and runs no
// privileged code: it only writes a request file and reads a status file.
//
// One tick (see [Executor.Tick]):
//
//  1. takes the per-app lock (flock; a second run exits at once),
//  2. reconciles an unfinished attempt FIRST, even outside the update window
//     and when the Hub is down,
//  3. reads the bounded, versioned request file (never following links),
//  4. inside the window (or for a valid one-shot request) selects and
//     verifies a release with the signed manifest, pre-pulls
//     repository@digest, binds the previous and candidate references to
//     local image ids and runs the transaction: app.stop, wait until no
//     container of the app runs, zfs snapshot of exactly the configured
//     dataset, app.update with the candidate compose while stopped,
//     app.start, sustained /readyz of the container that runs the candidate
//     image id, commit,
//  5. on failure restores in this order: app.stop, zfs rollback, app.update
//     back to the saved compose, app.start, verify the previous image, and
//     quarantines the failed digest,
//  6. writes a sanitized status file atomically.
//
// Every side effect is announced in the journal (a root-owned directory)
// before it happens, so a killed run is finished or undone by the next tick.
// The middleware, Docker, ZFS and release collaborators are interfaces; the
// production implementations call the TrueNAS built-in client (payload on
// stdin, never in argv), the docker CLI and the zfs CLI.
//
// Owner story: A65 (Hub auto-update on TrueNAS).
package truenas
