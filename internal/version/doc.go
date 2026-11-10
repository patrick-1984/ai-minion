// Package version holds the build information of the binary, the release
// channels, the verification of release signatures and the self-update
// behind "minion update" (AD-24). It is part of the shared kernel.
//
// Build information: Version, Commit and Date (all "dev" by default) are set
// through -ldflags by the release pipeline; [Get] returns them as [Info].
//
// Selecting a release: [SemVer] and [ParseVersion] model "vX.Y.Z" and
// "vX.Y.Z-rc.N". A [Selector] works like a Docker tag: "vX.Y.Z" is one exact
// version, "X.Y" and "X" are channels that follow the newest matching release
// and "latest" follows the newest release. [Selector.Resolve] picks the newest
// published [Release]; a channel never resolves to a pre-release (an -rc, or a
// release the host flags as pre-release), only an exact selector can.
//
// Verifying a release: [SigstoreVerifier] (the only user of sigstore-go,
// AD-1) implements [BlobVerifier]. It requires a certificate chain to the
// trusted Fulcio root, the identity of the release workflow for the exact tag
// ([ReleaseIdentity], built from the constants RepoOwner, RepoName,
// RepositoryURI, WorkflowPath and OIDCIssuer), a Rekor entry with a verified
// inclusion proof, and a signature over exactly the signed bytes.
// [ParseManifest] and [ParseChecksums] read the signed release-manifest.json
// and checksums.txt, and [ArchiveName] names the archive of a platform.
//
// Updating: an [Updater] runs one update and reaches the outside world only
// through ports, so that tests use fakes and a private Sigstore:
//
//   - [Source] lists releases and opens assets; [GitHubSource] is the
//     production adapter.
//   - [BlobVerifier] checks one signature and [VerifierFactory] builds it for
//     a run, online or offline ([SigstoreVerifierFactory]).
//   - [TrustedRootProvider] supplies the Sigstore trusted material:
//     [TUFTrustedRoot] in production, [StaticTrustedRoot] for fixed material.
//
// A run resolves the selector, verifies the signed checksums and manifest,
// refuses a downgrade unless allowed, checks the release's protocol range
// against the connected Hub ([NegotiateRanges], A-10), downloads the archive
// and verifies its digest against the signed checksums, stores the verified
// release in the [Cache] (the archive, the unpacked binary and the signed
// metadata) and replaces the running binary with [ReplaceBinary] (atomic, with
// the Windows rename rule). With Options.Offline the newest cached release
// that matches the selector is verified again from the signed files, the
// executable is taken from the cached archive, and nothing touches the
// network.
//
// Outbound connections (AD-19): this package makes network connections only
// during an explicit "minion update", and update.go is the only file that
// imports net/http (TestNoHTTPOutsideUpdate keeps it so). An online update
// contacts two destinations: the release host (the GitHub API and the release
// asset hosts) and the public Sigstore TUF repository,
// tuf-repo-cdn.sigstore.dev, for the trusted root. "minion update --offline"
// contacts nothing.
//
// Owner: Story 1.1 (build info); Story 9.2 (channels, verification, update).
package version
