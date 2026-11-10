// Package smoke holds the install smoke tests of the exact release artifacts
// and the publish gate (AD-24, A-13, A-14).
//
// The smoke tests themselves are scripts: deb.sh and rpm.sh (Debian 13 and
// Fedora containers with systemd), bootstrap.sh (alpine and debian), image.sh
// (the image by digest as UID 65532 on fresh, restored and unprepared data
// dirs), macos.sh and windows.ps1. fetch.sh downloads the exact draft assets
// and verifies checksums.txt against its cosign bundle and the release
// workflow identity; channels.sh decides which channel tags a release may
// move. .github/workflows/smoke.yml runs the scripts on clean runners and
// .github/workflows/publish.yml publishes the draft only when every job
// passed (a release candidate as a prerelease that moves no channel tag, a
// final release with the image tags X.Y, X and latest).
//
// The Go tests of this package are the unit tests of that machinery: they run
// the scripts against stubbed docker, gh, cosign and curl commands, run
// macos.sh and the windows.ps1 argument list and environment against a real
// binary on a private tmux server (internal/testutil/tmuxtest), and check the
// workflow files (gating, pinned actions, the token only in the fetch steps,
// the pinned checksums and image digest, no channel move for a candidate).
// The jobs themselves run on the release draft and are exercised by v0.1.0-rc.1
// and v0.1.0.
//
// Owner: Story 9.7.
package smoke
