// Package dockere2e holds the container end-to-end checks of Story 9.6: the
// real Hub image and the native minion binary, driven through REST. The checks
// carry the build tag dockere2e and run through test/e2e/docker/run.sh (CI job
// "e2e", workflow e2e.yml); this file keeps the package buildable without the
// tag.
//
// Owner: Story 9.6.
package dockere2e
