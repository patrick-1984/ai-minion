#!/usr/bin/env bash
# check-skeleton.sh: guards the repository skeleton of Story 1.1.
#   1. Every package of the spine Structural Seed has a doc.go with a package
#      comment that states its purpose ("// Package <name> ...") and its owner
#      story (a line "// Owner: Story ...").
#   2. The make targets documented in CONTRIBUTING.md are exactly the targets
#      of the Makefile that carry a "##" help text (help itself excluded).
#   3. .github/workflows/ci.yml keeps the required jobs: lint and the negative
#      import check, the race tests on the distribution tmux and on tmux 3.8
#      from source (both with the tmux build tag), the CPace vector tests,
#      govulncheck, the cross-build, the ldflags assertions and the Compose
#      file validation.
#
# Usage: scripts/check-skeleton.sh [repo-root]   (default: the parent of scripts/)
set -euo pipefail

root="${1:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)}"
cd "$root"
fail=0
bad() { echo "check-skeleton: $*" >&2; fail=1; }

# 1. doc.go of every Structural Seed package.
seed=(
  internal/cli internal/proto internal/ops internal/identity internal/pake
  internal/transport internal/config internal/ratelimit internal/logx
  internal/version internal/pipe internal/client internal/mcp
  internal/hub/server internal/hub/store internal/hub/auth/oidc
  internal/hub/auth/local internal/hub/auth/session internal/hub/auth/apikey
  internal/hub/authz internal/hub/workers internal/hub/api internal/hub/web
  internal/hub/audit
  internal/worker/agent internal/worker/term internal/worker/term/tmux
  internal/worker/term/conpty internal/worker/agentdetect internal/worker/store
  internal/worker/localapi internal/worker/setup internal/worker/service
  internal/testutil/tmuxtest
  test/e2e
)
for dir in "${seed[@]}"; do
  f="$dir/doc.go"
  if [ ! -f "$f" ]; then bad "$f is missing"; continue; fi
  grep -Eq '^// Package [a-z0-9]+ [^ ]' "$f" || bad "$f has no package comment (\"// Package <name> <purpose>\")"
  grep -Eq '^// Owner: Stor(y|ies) [0-9]' "$f" || bad "$f does not state its owner story (\"// Owner: Story N.N\")"
  grep -Eq '^package [a-z0-9_]+$' "$f" || bad "$f has no package clause"
done

# 2. CONTRIBUTING.md target table against the Makefile.
mk_targets="$(grep -E '^[a-z][a-z0-9-]*:.*## ' Makefile | sed -E 's/:.*//' | grep -vx help | LC_ALL=C sort)"
doc_targets="$(grep -oE '^\| `make [a-z][a-z0-9-]*`' CONTRIBUTING.md | sed -E 's/^\| `make ([a-z0-9-]+)`/\1/' | LC_ALL=C sort)"
if [ "$mk_targets" != "$doc_targets" ]; then
  bad "CONTRIBUTING.md make targets differ from the Makefile (< Makefile, > CONTRIBUTING.md):"
  diff <(echo "$mk_targets") <(echo "$doc_targets") >&2 || true
fi

# 3. CI composition.
ci=.github/workflows/ci.yml
need() { grep -Fq -- "$1" "$ci" || bad "$ci lacks: $1"; }
need 'run: make lint'
need 'run: make lint-negative'
need 'run: make check-compose'
need 'run: make test-race TAGS=tmux'
need 'run: make vuln'
need 'run: make cross'
need 'TMUX_VERSION: "3.8"'
need 'go test -race -count=1 -v -run Vector ./internal/pake/...'
need 'make build VERSION=ci COMMIT=cafe0123 DATE=2026-01-02T03:04:05Z'
need 'grep -Fx "version: ci"'
need 'grep -Fx "commit: cafe0123"'
need 'grep -Fx "built: 2026-01-02T03:04:05Z"'
if [ "$(grep -Fc 'run: make test-race TAGS=tmux' "$ci")" -lt 2 ]; then
  bad "$ci must run the tmux-tagged race tests in both the distribution and the tmux 3.8 job"
fi

[ "$fail" = 0 ] || exit 1
echo "check-skeleton: ok"
