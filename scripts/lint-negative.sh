#!/usr/bin/env bash
# lint-negative.sh: proves that the depguard rules of .golangci.yml reject
# forbidden imports (AD-1). It copies the repository to a scratch directory,
# adds one file with a forbidden import per case and asserts that
# golangci-lint fails with a depguard finding that names the expected rule.
# Every rule family is exercised: the Hub/Worker separation, the shared
# kernel (including internal/testutil and the kernel edges) and each library
# of the allow-list (cobra, SQLite, yamux, websocket, go-oidc, oauth2, argon2,
# MCP SDK, kardianos/service, go-pty, sigstore-go). Positive controls prove
# that each library stays allowed in its owner package.
#
# A library that go.mod already requires is used as it is (its real packages
# and the repository's own replace directives stay), so real code that uses
# its API keeps typechecking. Only a library that go.mod does not require (yet)
# is replaced in the scratch copy by a local stub module. No fixture package or
# stub is added to the repository itself.
#
# Usage: scripts/lint-negative.sh [path-to-golangci-lint]
set -euo pipefail

lint="${1:-${GOLANGCI_LINT:-golangci-lint}}"
repo="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
mod="github.com/patrick-1984/ai-minion"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

# Scratch copy without VCS data and build output.
(cd "$repo" && tar --exclude=.git --exclude=./bin --exclude=./dist -cf - .) | (cd "$tmp" && tar -xf -)
cd "$tmp"
# The scratch go.mod may need go.sum and indirect requirement updates once a
# fixture imports a module that is only a transitive dependency of the repository.
export GOFLAGS="${GOFLAGS:+$GOFLAGS }-mod=mod"

# required <module>: true when go.mod already requires the module (a block
# line "<module> <version>" or a "require <module> <version>" line)
required() {
  awk -v m="$1" '$1 == m || ($1 == "require" && $2 == m) { found = 1 } END { exit !found }' go.mod
}

# stub <module> <version> [<subpath> <package name>]...
# Creates a local stub module and wires it into the scratch go.mod, unless
# go.mod already requires the module: the real module and the repository's own
# replace directives are never replaced. The module root gets one file; every
# <subpath> <package name> pair gets a subpackage.
stubn=0
stub() {
  local m="$1" ver="$2" dir real
  shift 2
  if required "$m"; then
    echo "lint-negative: $m is required by go.mod, using the real module"
    return
  fi
  if real="$(go list -m -f '{{.Version}}' "$m" 2>/dev/null)" && [ -n "$real" ]; then
    # A transitive dependency (for example oauth2 through sigstore-go): the
    # real module must be used, a stub would hide packages that the module
    # graph needs. Make the requirement direct in the scratch go.mod.
    echo "lint-negative: $m is in the module graph ($real), using the real module"
    go mod edit -require="$m@$real"
    return
  fi
  stubn=$((stubn + 1))
  dir="zz-stubs/s$stubn"
  mkdir -p "$dir"
  printf 'module %s\n\ngo 1.27\n' "$m" >"$dir/go.mod"
  printf 'package %s\n' "$(basename "${m%/v[0-9]*}" | tr -cd 'a-z0-9')" >"$dir/stub.go"
  while [ "$#" -ge 2 ]; do
    mkdir -p "$dir/$1"
    printf 'package %s\n' "$2" >"$dir/$1/stub.go"
    shift 2
  done
  go mod edit -require="$m@$ver" -replace="$m=./$dir"
}
v0=v0.0.0-00010101000000-000000000000
stub github.com/hashicorp/yamux "$v0"
stub github.com/coder/websocket "$v0"
stub modernc.org/sqlite "$v0"
stub github.com/coreos/go-oidc/v3 v3.0.0-00010101000000-000000000000 oidc oidc
stub golang.org/x/oauth2 "$v0"
stub golang.org/x/crypto "$v0" argon2 argon2
stub github.com/modelcontextprotocol/go-sdk "$v0"
stub github.com/kardianos/service "$v0"
stub github.com/aymanbagabas/go-pty "$v0"
stub github.com/sigstore/sigstore-go "$v0"
stub github.com/Microsoft/go-winio "$v0"

# Build contexts that are linted: files with a GOOS or GOARCH suffix or a build
# constraint are invisible to the other contexts, and so are the files behind
# the tmux and e2e build tags. The list is the release matrix (Makefile:
# CROSS_TARGETS); the tags are linted in every context.
contexts="linux/amd64 linux/arm64 linux/arm windows/amd64 windows/arm64 darwin/amd64 darwin/arm64"
tags="tmux,e2e"

# lint_ <goos> <goarch> <tags>: one lint run in a build context.
lint_() {
  GOOS="$1" GOARCH="$2" "$lint" run --build-tags "$3" ./...
}

# The unmodified copy must be clean for every checked context, otherwise a
# failure below proves nothing.
for ctx in $contexts; do
  if ! lint_ "${ctx%/*}" "${ctx#*/}" "$tags" >/dev/null 2>&1; then
    echo "lint-negative: the scratch copy is not lint-clean for GOOS=${ctx%/*} GOARCH=${ctx#*/} tags=$tags before adding violations" >&2
    lint_ "${ctx%/*}" "${ctx#*/}" "$tags" >&2 || true
    exit 1
  fi
done

# The real dependencies must stay the real ones (the repository's own code uses
# their API, and the baseline above typechecked it): no module that the
# repository's go.mod requires may be replaced by a stub.
for m in $(awk '$1 ~ /\./ && $2 ~ /^v[0-9]/ { print $1 }' "$repo/go.mod"); do
  if grep -Eq "^(replace )?[[:space:]]*${m//./\\.}( [^ ]+)? => \./zz-stubs/" go.mod; then
    echo "lint-negative: FAIL: the required module $m is replaced by a stub in the scratch copy" >&2
    exit 1
  fi
done

fail=0
# case_ <name> <dir under internal/> <imported package> <expected rule> [goos [goarch [tag]]]
# With a goos, goarch or tag the fixture is a file with that GOOS or GOARCH
# suffix or build constraint and the lint runs for that context (such files
# are invisible to the other contexts); the remaining context is linux/amd64
# with no tags.
case_() {
  local name="$1" dir="$2" imp="$3" rule="$4" os="${5:-}" arch="${6:-}" tag="${7:-}" suffix="" pkg out
  [ -z "$tag" ] || suffix="${suffix}_tag"
  [ -z "$os" ] || suffix="${suffix}_$os"
  [ -z "$arch" ] || suffix="${suffix}_$arch"
  local file="$tmp/internal/$2/zz_forbidden$suffix.go"
  pkg="$(basename "$dir")"
  mkdir -p "$(dirname "$file")"
  {
    [ -z "$tag" ] || printf '//go:build %s\n\n' "$tag"
    printf 'package %s\n\nimport _ "%s"\n' "$pkg" "$imp"
  } >"$file"
  if out="$(lint_ "${os:-linux}" "${arch:-amd64}" "$tags" 2>&1)"; then
    echo "lint-negative: FAIL: $name: golangci-lint accepted the forbidden import $imp in internal/$dir" >&2
    fail=1
  elif ! grep -q "depguard" <<<"$out" || ! grep -q "'$rule'" <<<"$out"; then
    echo "lint-negative: FAIL: $name: lint failed but not with depguard rule '$rule'" >&2
    echo "$out" >&2
    fail=1
  else
    echo "lint-negative: ok: $name ($rule)"
  fi
  rm -f "$file"
}

# allowed_ <name> <dir under internal/> <imported package> [<code> [<goos>]]:
# must stay clean (linux/amd64, all tags; with a goos the fixture is a file with
# that GOOS suffix and the lint runs for goos/amd64). Without <code> the fixture
# is a blank import; with <code> it is the given declarations, which use the
# real API of the library.
allowed_() {
  local name="$1" dir="$2" imp="$3" code="${4:-}" os="${5:-}" file="$tmp/internal/$2/zz_allowed${5:+_$5}.go" pkg out
  pkg="$(basename "$dir")"
  mkdir -p "$(dirname "$file")"
  if [ -z "$code" ]; then
    printf 'package %s\n\nimport _ "%s"\n' "$pkg" "$imp" >"$file"
  else
    printf 'package %s\n\nimport "%s"\n\n%s\n' "$pkg" "$imp" "$code" >"$file"
  fi
  if ! out="$(lint_ "${os:-linux}" amd64 "$tags" 2>&1)"; then
    echo "lint-negative: FAIL: $name was rejected" >&2
    echo "$out" >&2
    fail=1
    rm -f "$file"
    return
  fi
  echo "lint-negative: ok: $name is allowed"
  rm -f "$file"
}

# Packages whose names merely start with the name of a kernel package: the
# kernel allow-list matches exactly ("$"), so they stay non-kernel and denied.
for p in configloader protox; do
  mkdir -p "internal/$p"
  printf 'package %s\n' "$p" >"internal/$p/doc.go"
done

# Hub and Worker meet only on the wire.
case_ "hub imports worker" hub/api "$mod/internal/worker/store" hub-not-worker
case_ "worker imports hub" worker/agent "$mod/internal/hub/store" worker-not-hub

# Shared kernel.
case_ "kernel imports non-kernel (config -> transport)" config "$mod/internal/transport" kernel-is-closed
case_ "kernel imports non-kernel (identity -> hub/authz)" identity "$mod/internal/hub/authz" kernel-is-closed
case_ "kernel imports non-kernel (version -> hub/store)" version "$mod/internal/hub/store" kernel-is-closed
case_ "kernel imports testutil (config -> testutil/tmuxtest)" config "$mod/internal/testutil/tmuxtest" kernel-is-closed
case_ "kernel imports another kernel package (config -> version)" config "$mod/internal/version" kernel-edges-config
case_ "kernel imports another kernel package (ops -> identity)" ops "$mod/internal/identity" kernel-edges-ops
case_ "kernel imports a package named like a kernel package (config -> configloader)" config "$mod/internal/configloader" kernel-is-closed
case_ "kernel edge admits only its own package (config -> configloader)" config "$mod/internal/configloader" kernel-edges-config
case_ "kernel imports a package named like a kernel package (ops -> protox)" ops "$mod/internal/protox" kernel-is-closed
case_ "kernel edge admits only its own package (ops -> protox)" ops "$mod/internal/protox" kernel-edges-ops
allowed_ "ops -> proto" ops "$mod/internal/proto"

# Library allow-list: one owner per library.
case_ "cobra in proto" proto github.com/spf13/cobra lib-cobra
case_ "SQLite driver in cli" cli modernc.org/sqlite lib-sqlite
case_ "yamux in cli" cli github.com/hashicorp/yamux lib-transport
case_ "websocket in cli" cli github.com/coder/websocket lib-transport
case_ "go-oidc in cli" cli github.com/coreos/go-oidc/v3/oidc lib-oidc
case_ "oauth2 in cli" cli golang.org/x/oauth2 lib-oidc
case_ "argon2 in cli" cli golang.org/x/crypto/argon2 lib-argon2
case_ "MCP SDK in cli" cli github.com/modelcontextprotocol/go-sdk/mcp lib-mcp
case_ "kardianos/service in cli" cli github.com/kardianos/service lib-service
case_ "go-pty in cli" cli github.com/aymanbagabas/go-pty lib-pty
case_ "sigstore-go in cli" cli github.com/sigstore/sigstore-go/pkg/root lib-sigstore
# go-winio has one importer, worker/localapi (AD-1): the client dials through
# localapi.DialContext. The library only builds on Windows.
case_ "go-winio in cli" cli github.com/Microsoft/go-winio lib-winio windows
case_ "go-winio in client" client github.com/Microsoft/go-winio lib-winio windows

# Platform-specific files are checked in the build context of their platform.
case_ "cobra in a windows-only file of conpty" worker/term/conpty github.com/spf13/cobra lib-cobra windows
case_ "hub imports worker in a darwin-only file" hub/api "$mod/internal/worker/store" hub-not-worker darwin

# Build tags and architectures: the files behind the tmux and e2e tags and the
# architecture-specific files are linted as well.
case_ "hub imports worker behind the tmux tag" hub/api "$mod/internal/worker/store" hub-not-worker "" "" tmux
case_ "hub imports worker behind the e2e tag" hub/api "$mod/internal/worker/store" hub-not-worker "" "" e2e
case_ "cobra in an arm64-only file of proto" proto github.com/spf13/cobra lib-cobra linux arm64
case_ "yamux in a windows/arm64-only file of cli" cli github.com/hashicorp/yamux lib-transport windows arm64
case_ "worker imports hub in a linux/arm-only file" worker/agent "$mod/internal/hub/store" worker-not-hub linux arm

# Positive controls: every library stays allowed in its owner package.
allowed_ "cobra in cli" cli github.com/spf13/cobra
allowed_ "SQLite driver in hub/store" hub/store modernc.org/sqlite
allowed_ "SQLite driver in worker/store" worker/store modernc.org/sqlite
allowed_ "yamux in transport" transport github.com/hashicorp/yamux
allowed_ "websocket in transport" transport github.com/coder/websocket

# Regression (issue 4): a library that go.mod requires is the real module, so
# real API use in its owner package typechecks and stays clean.
allowed_ "real yamux API in transport" transport github.com/hashicorp/yamux 'var _ = yamux.DefaultConfig'
allowed_ "real chacha20poly1305 API in transport" transport golang.org/x/crypto/chacha20poly1305 'var _ = chacha20poly1305.NewX'
allowed_ "real websocket API in transport" transport github.com/coder/websocket 'var _ = websocket.Dial'
allowed_ "go-oidc in hub/auth/oidc" hub/auth/oidc github.com/coreos/go-oidc/v3/oidc
allowed_ "oauth2 in hub/auth/oidc" hub/auth/oidc golang.org/x/oauth2
allowed_ "argon2 in hub/auth/local" hub/auth/local golang.org/x/crypto/argon2
allowed_ "MCP SDK in mcp" mcp github.com/modelcontextprotocol/go-sdk/mcp
allowed_ "kardianos/service in worker/service" worker/service github.com/kardianos/service
allowed_ "go-pty in worker/term/conpty" worker/term/conpty github.com/aymanbagabas/go-pty
allowed_ "sigstore-go in version" version github.com/sigstore/sigstore-go/pkg/root
allowed_ "go-winio in worker/localapi" worker/localapi github.com/Microsoft/go-winio "" windows

exit "$fail"
