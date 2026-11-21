package truenas

import (
	"context"
	"errors"

	"github.com/patrick-1984/ai-minion/internal/version"
)

// VersionReleases implements [ReleaseSource] with the same release selector
// and signature verifier the Hub and `minion update` use.
type VersionReleases struct {
	// NewUpdater builds the updater (GitHub source, Sigstore verifier, a
	// root-owned cache).
	NewUpdater func() (*version.Updater, error)
}

// Verified implements ReleaseSource.
func (v VersionReleases) Verified(ctx context.Context, selector string) (Release, error) {
	sel, err := version.ParseSelector(selector)
	if err != nil {
		return Release{}, err
	}
	up, err := v.NewUpdater()
	if err != nil {
		return Release{}, err
	}
	rel, man, err := up.VerifiedRelease(ctx, sel)
	if err != nil {
		return Release{}, err
	}
	if man.Image == nil {
		return Release{}, errors.New("the signed release manifest names no container image")
	}
	rng, err := man.Range()
	if err != nil {
		return Release{}, err
	}
	return Release{Tag: rel.Tag, Prerelease: rel.Prerelease, Repo: man.Image.Repository, Digest: man.Image.Digest, Range: rng}, nil
}
