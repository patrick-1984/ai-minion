package version

import "strings"

// Release workflow identity (AD-24). The updater and bootstrap.sh verify the
// same signer: the release workflow of this repository, running for the exact
// release tag, authenticated by GitHub Actions OIDC. bootstrap.sh embeds the
// same constants.
const (
	// RepoOwner and RepoName name the GitHub repository that publishes
	// releases.
	RepoOwner = "patrick-1984"
	RepoName  = "ai-minion"

	// RepositoryURI is the source repository URI that Fulcio records in the
	// signing certificate (extension 1.3.6.1.4.1.57264.1.12).
	RepositoryURI = "https://github.com/" + RepoOwner + "/" + RepoName

	// WorkflowPath is the path of the release workflow in the repository.
	WorkflowPath = ".github/workflows/release.yml"

	// OIDCIssuer is the issuer of the GitHub Actions identity token.
	OIDCIssuer = "https://token.actions.githubusercontent.com"
)

// Names of the release assets the updater reads (Story 9.1).
const (
	// ChecksumsAsset lists the SHA-256 of every archive.
	ChecksumsAsset = "checksums.txt"
	// ManifestAsset is release-manifest.json: version, protocol range and
	// artifact digests.
	ManifestAsset = "release-manifest.json"
	// BundleSuffix is appended to a signed asset's name to name its cosign
	// keyless bundle, for example "checksums.txt.sigstore.json".
	BundleSuffix = ".sigstore.json"
)

// BundleName returns the name of the cosign bundle that signs asset.
func BundleName(asset string) string { return asset + BundleSuffix }

// Identity is the signer a release must have been signed by.
type Identity struct {
	// SubjectAlternativeName is the workflow URI with the tag ref, which is
	// the certificate's SAN.
	SubjectAlternativeName string
	// Issuer is the OIDC issuer of the identity.
	Issuer string
	// SourceRef is the git ref the workflow ran for, "refs/tags/<tag>".
	SourceRef string
	// SourceRepositoryURI is the repository the workflow ran in.
	SourceRepositoryURI string
}

// SourceRef returns the git ref of a release tag: "refs/tags/<tag>".
func SourceRef(tag string) string { return "refs/tags/" + tag }

// ReleaseIdentity returns the identity that must have signed the release with
// the given tag, for example "v1.4.2".
func ReleaseIdentity(tag string) Identity {
	return Identity{
		SubjectAlternativeName: strings.Join([]string{RepositoryURI, WorkflowPath}, "/") + "@" + SourceRef(tag),
		Issuer:                 OIDCIssuer,
		SourceRef:              SourceRef(tag),
		SourceRepositoryURI:    RepositoryURI,
	}
}
