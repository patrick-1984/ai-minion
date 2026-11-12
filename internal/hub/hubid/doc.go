// Package hubid is the Hub side of the two-tier Hub identity (0.7.0, I2;
// docs/architecture/proposals/hub-identity-v2.md). It owns the key files of
// the data directory (root.key.age, op.key and their .next files), the
// certificate state of the hub_identity row, the recovery rules of section
// 9.5 and the four operations that need the owner's passphrase: set the
// passphrase, re-issue the certificate, rotate the master key and change the
// passphrase. It has a Go API only: the web page, the commands and the
// handshake wire it in later increments.
//
// One [Service] serves one data directory. Every identity change runs under
// the service's rotation mutex, with the passphrase in memory only for that
// call, and uses [identity.RootSigner] objects that are destroyed on every
// exit path.
package hubid
