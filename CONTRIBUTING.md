# Contributing to Minion

Minion is one Go binary (`minion`, also installed as `mn`) that runs as Minion
Hub, Minion Worker or Solo. The binding design is in
[`docs/architecture/ARCHITECTURE-SPINE.md`](docs/architecture/ARCHITECTURE-SPINE.md)
(decisions AD-1 to AD-25 and its Amendments section).

All code, comments, UI text, errors, logs and documentation are written in
English.

## Requirements

- Go 1.27.4 (see `go.mod`)
- `make`, `git`, `perl` (for `scripts/check-english.sh`)
- [golangci-lint](https://golangci-lint.run/) v2, built with the Go version of
  this module (`go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.15.0`)
- `tmux` for the tests of the Worker (CI tests the distribution tmux and tmux 3.8)

## Make targets

| Target | What it does |
| --- | --- |
| `make build` | Builds `bin/minion` with `CGO_ENABLED=0`. Version, commit and build date are injected with `-ldflags` (`VERSION`, `COMMIT`, `DATE` can be overridden; the default version is `dev`). |
| `make cross` | Builds linux amd64/arm64/armv7 (`GOARM=7`), darwin amd64/arm64 and windows amd64/arm64 into `bin/cross/`, all with `CGO_ENABLED=0`. |
| `make tray` | Builds `bin/minion-tray`, the optional desktop tray (a separate binary; macOS needs `CGO_ENABLED=1`). See [Desktop tray](docs/guides/tray.md). |
| `make check-headless` | Fails if a package other than `cmd/minion-tray` imports a GUI library, if the armv7 tray build links one, or if goreleaser, `make cross` or the tray workflow would build the tray for the headless targets or for armv7. |
| `make test` | `go test ./...` (`TAGS=tmux` adds the tmux-tagged tests). |
| `make test-race` | `go test -race ./...` (needs CGO; `TAGS=tmux` adds the tmux-tagged tests, which CI always runs). |
| `make lint` | `go vet`, golangci-lint with the import rules (run for every release `GOOS`/`GOARCH` and with the `tmux` and `e2e` build tags, so platform-, architecture- and tag-specific files are covered), the tmux check, the English check, the private-name check, the documentation-link check, the skeleton check and the headless check. |
| `make check-tmux` | Parses the test files and fails on `kill-server` or on tmux started directly in a test (nested and multi-line calls included); tests use `internal/testutil/tmuxtest`. |
| `make check-docs` | Fails on a relative Markdown link or `#anchor` in `README.md`, `CONTRIBUTING.md`, `docs/install`, `docs/guides` and `docs/security.md` that does not resolve, on a page without a link back to the README, and on a page the README does not link to. Needs no network. |
| `make check-compose` | Runs `docker compose config` on `deploy/compose/*.yaml` with dummy variables and dummy secret files in a scratch directory (it only reads the files: no daemon, no pull, no build, nothing started). Needs the docker CLI with the compose plugin; first runs a self test with a stand-in `docker`. |
| `make check-skeleton` | Fails if a Structural Seed package lacks a `doc.go` with purpose and owner story, if this table and the Makefile disagree, or if `ci.yml` lost a required job or the ldflags assertions. |
| `make lint-negative` | Proves that the import rules reject forbidden imports, using a scratch copy of the repository (needs no network; a library that `go.mod` requires is used as it is, only a library that is not in `go.mod` yet is replaced by a local stub). |
| `make check-private-names` | Fails if a tracked or untracked (not ignored) text file contains an owner-internal name: the private domain or a Hub host name, an internal network address, a device or Wi-Fi name or a personal host name. The Android application ID and Kotlin package are allowed (dotted, path and JNI spellings). Use RFC 2606 / RFC 5737 examples (`minion.example`, `192.0.2.10`). The patterns are in `scripts/check-private-names.sh`. |
| `make check-english` | Fails on non-English letters in every line of the `internal/` Go files (including multi-line raw strings) and in `docs/`; exemptions go into `scripts/check-english.allow`. |
| `make vuln` | `govulncheck` (golang.org/x/vuln v1.9.0). |
| `make e2e` | End-to-end tests (build tag `e2e`, `test/e2e`), run with `-race`. The container checks of Story 9.6 (the Hub image and a native Worker, direct and behind a reverse proxy) run through `test/e2e/docker/run.sh`, which needs docker and ko; CI runs both in `e2e.yml`. |
| `make snapshot` | goreleaser snapshot build. It does nothing until Story 9.1 adds `.goreleaser.yaml`. |
| `make clean` | Removes `bin/` and `dist/`. |
| `make openapi` | Writes `docs/api/openapi.json`, the OpenAPI 3.1 description that `ops.OpenAPI` generates from the operation catalog (the document the Hub serves at `GET /api/v1/ops`). Run it after `go test ./internal/ops -run OpenAPI -update` when an operation or one of its types changed. |

A story is done when `make lint test-race vuln cross` is green.

## Import boundaries

`.golangci.yml` enforces the rules of AD-1 with depguard. `make lint` fails when:

- a package imports a library outside its owner (SQLite driver: `hub/store`,
  `worker/store`; yamux and websocket: `transport`; go-oidc and oauth2:
  `hub/auth/oidc`; argon2: `hub/auth/local`; MCP SDK: `mcp`; cobra: `cli`;
  kardianos/service: `worker/service`; go-pty: `worker/term/conpty`;
  sigstore-go: `version`);
- `internal/hub/...` imports `internal/worker/...`, or the reverse;
- a kernel package (`proto`, `ops`, `identity`, `pake`, `config`, `logx`,
  `version`, `ratelimit`, `pipe`, `sqlmigrate`) imports a non-kernel internal
  package (this includes `internal/testutil`; any package that is not in the
  kernel list is non-kernel), or another kernel package (the only exception
  is `ops` importing `proto`).

Adding a top-level package under `internal/` needs a spine update.

## Delivery rules

1. **One story, one pull request, one branch.** Name the branch
   `story/<id>-<slug>` (for example `story/4.6-invite-enroll-worker`) and build
   it in its own git worktree from `main` after every dependency is merged. The
   pull request description lists the story ID, the acceptance criteria with the
   tests that prove them, and the files touched.
2. **Ownership.** A story creates or edits only the files and packages listed
   under **Owns** in its story file. The `doc.go` stubs of Story 1.1 may be
   replaced by the owner of the package. If a story needs a change in a file it
   does not own, it stops and records the need in the pull request description.
3. **Shared append-only files.** `go.mod`, `go.sum` (use exactly the versions of
   the spine Stack table, then `go mod tidy`), `internal/proto/codes.go` (append
   new codes at the end of the block) and `CHANGELOG.md` (one line per story
   under "Unreleased"). Never rewrite existing lines; conflicts are resolved
   mechanically at rebase.
4. **Registration instead of edits.** Hot spots are avoided by registering from
   files the story owns:
   - CLI: each command lives in its own `internal/cli/<group>_<cmd>.go` and
     calls `cli.Register` from `init` (see below).
   - Settings: `internal/config/settings_<area>.go`.
   - Operations: `internal/ops/<group>.go`.
   - Hub modules: `internal/hub/server/mod_<name>.go`.
   - Worker modules: `internal/worker/agent/mod_<name>.go`; op handlers in
     `exec_<group>.go`.
   - Hub API handlers `internal/hub/api/h_<domain>.go`; web pages
     `internal/hub/web/page_<name>.go` plus `templates/<name>.html`.
5. **Definition of done.** All acceptance criteria pass with named tests;
   `make lint test-race` is green; `make vuln` is clean; new strings are
   English; no secret or terminal content reaches logs; tmux is used in tests
   only through `internal/testutil/tmuxtest`; the `CGO_ENABLED=0` cross-build
   passes; security-relevant actions write audit events; the spine conventions
   hold (snake_case codes, UUIDv7, RFC 3339 with milliseconds in JSON).

## The registration pattern for CLI commands

`internal/cli` is the only package that imports cobra. A command is added by a
new file, without touching any other file:

```go
package cli

import "github.com/spf13/cobra"

func init() {
	Register(func(root *cobra.Command) {
		root.AddCommand(&cobra.Command{
			Use:   "example",
			Short: "One line of help",
			RunE:  func(cmd *cobra.Command, args []string) error { return nil },
		})
	})
}
```

Files of a package are initialised in file-name order, so the command order is
deterministic.

## Tests

- Use `go test -race ./...`. Keep packages small, with ports at the edges, so
  that logic is unit-testable.
- tmux is started only by `internal/testutil/tmuxtest` on a private socket
  (`tmux -S <temp>/tmux.sock -f /dev/null`, with `TMUX` and `TMUX_PANE` unset).
  No test touches the default tmux server, and none runs `kill-server` without
  `-S`. `make lint` checks both rules. CI sets `MINION_REQUIRE_TMUX=1`, so a
  missing tmux fails instead of skipping.
- Never put secrets in command-line arguments or logs.

## Continuous integration

`.github/workflows/ci.yml` runs on every pull request: `go vet` and
golangci-lint (with the negative check of the import rules), `go test -race`
once with the distribution tmux and once with tmux 3.8 built from source and
cached, `govulncheck`, the cross-build, the English and documentation-link checks, and the Compose file validation (`make check-compose`). `release.yml` runs the same steps on a tag.

An Android app fix can ship alone: the tag `android-vX.Y.Z` runs
`.github/workflows/android-release.yml`, which tests the commit, builds and
signs the APK through `android-apk.yml` and publishes a release with only
`minion-android-X.Y.Z.apk`, `checksums.txt` and `checksums.txt.sigstore.json`
(never marked latest; `release.yml` and `publish.yml` do not run). Pick an
X.Y.Z between the newest full release and the next one, and never tag a full
`vX.Y.Z` with the same number later.
