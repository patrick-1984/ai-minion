// Package releasecheck tests the release pipeline of Story 9.1 (AD-24): the
// goreleaser, ko and workflow configuration (static checks that run without
// network), the nfpm package scripts, scripts/assert-image-platforms.sh and
// scripts/release-manifest.sh. It contains only tests; they run with the
// normal "go test ./..." and need bash and python3 (they skip without them).
//
// Owner: Story 9.1.
package releasecheck
