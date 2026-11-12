# Minion build, test and release helpers (AD-24, AD-25). See CONTRIBUTING.md.
#
# Tool versions are pinned to the architecture spine (Stack table).

MODULE        := github.com/patrick-1984/ai-minion
GOLANGCI_LINT ?= golangci-lint
GOVULNCHECK   ?= go run golang.org/x/vuln/cmd/govulncheck@v1.9.0
GORELEASER    ?= goreleaser

VERSION ?= dev
COMMIT  ?= $(shell git rev-parse --short=12 HEAD 2>/dev/null || echo dev)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

LDFLAGS := -s -w \
	-X $(MODULE)/internal/version.Version=$(VERSION) \
	-X $(MODULE)/internal/version.Commit=$(COMMIT) \
	-X $(MODULE)/internal/version.Date=$(DATE)

# GOOS/GOARCH[/GOARM] triples built by "make cross" (AD-24 targets).
CROSS_TARGETS := linux/amd64 linux/arm64 linux/arm/7 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64

.DEFAULT_GOAL := build
# Extra build tags for the tests, for example TAGS=tmux to include the
# tmux-tagged tests (CI always does).
TAGS ?=
TESTTAGS := $(if $(TAGS),-tags $(TAGS))

# Build contexts (GOOS/GOARCH) linted in addition to the host: files with a GOOS
# or GOARCH suffix or build constraint are invisible to the other contexts.
# The list is the release matrix (CROSS_TARGETS without GOARM). The tmux and e2e
# tagged files are linted in every context (LINT_TAGS).
LINT_TARGETS := linux/amd64 linux/arm64 linux/arm windows/amd64 windows/arm64 darwin/amd64 darwin/arm64
LINT_TAGS    := tmux,e2e

.PHONY: build cross test test-race lint lint-negative check-english check-tmux check-docs check-compose check-skeleton vuln e2e snapshot clean help

help: ## List the targets
	@grep -E '^[a-z-]+:.*## ' $(MAKEFILE_LIST) | sed -E 's/:.*## /\t/'

build: ## Build bin/minion (CGO_ENABLED=0, version injected through -ldflags)
	CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o bin/minion ./cmd/minion

cross: ## Build every release target into bin/cross/ (CGO_ENABLED=0)
	@set -e; for t in $(CROSS_TARGETS); do \
		os=$${t%%/*}; rest=$${t#*/}; arch=$${rest%%/*}; arm=; name=$$os-$$arch; \
		if [ "$$rest" != "$$arch" ]; then arm=$${rest#*/}; name=$$os-armv$$arm; fi; \
		ext=; [ "$$os" = windows ] && ext=.exe; \
		echo "build $$name"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch GOARM=$$arm \
			go build -trimpath -ldflags '$(LDFLAGS)' -o bin/cross/minion-$$name$$ext ./cmd/minion; \
	done

test: ## Run all tests (TAGS=tmux adds the tmux-tagged tests)
	go test -timeout 20m $(TESTTAGS) ./...

test-race: ## Run all tests with the race detector, needs CGO (TAGS=tmux adds the tmux-tagged tests)
	CGO_ENABLED=1 go test -race -timeout 20m $(TESTTAGS) ./...

lint: check-tmux check-english check-docs check-skeleton ## Run go vet, golangci-lint (incl. import rules) for every release GOOS/GOARCH with the tmux and e2e tags, tmux, English, documentation-link and skeleton checks
	go vet -tags $(LINT_TAGS) ./...
	@set -e; for t in $(LINT_TARGETS); do \
		echo "golangci-lint GOOS=$${t%/*} GOARCH=$${t#*/} tags=$(LINT_TAGS)"; \
		GOOS=$${t%/*} GOARCH=$${t#*/} $(GOLANGCI_LINT) run --build-tags $(LINT_TAGS) ./...; \
	done

lint-negative: ## Prove that the import rules reject forbidden imports (scratch copy, no network)
	GOLANGCI_LINT='$(GOLANGCI_LINT)' scripts/lint-negative.sh

check-english: ## Fail on non-English letters in internal/ Go files (every line) and docs/
	scripts/check-english.sh
	scripts/selftest-english.sh

# Tests may use tmux only through internal/testutil/tmuxtest on a private
# socket (AD-25). scripts/checktmux parses the test files (nested and
# multi-line calls included) and fails on kill-server or a direct tmux exec.
check-tmux: ## Fail if a test runs tmux directly or uses kill-server (AD-25)
	go run ./scripts/checktmux internal test cmd

check-docs: ## Fail on a broken relative link, anchor or missing README backlink in the user documentation (no network)
	go run ./scripts/checkdocs .

check-compose: ## Validate deploy/compose/*.yaml with docker compose config (dummy environment; needs the docker CLI with the compose plugin, not a daemon)
	scripts/selftest-compose.sh
	scripts/check-compose.sh

check-skeleton: ## Fail if a package lacks doc.go with purpose and owner story, or if CONTRIBUTING.md and the Makefile or CI disagree
	scripts/check-skeleton.sh

vuln: ## Run govulncheck (golang.org/x/vuln v1.9.0)
	$(GOVULNCHECK) ./...

e2e: ## Run the end-to-end tests (build tag e2e)
	go test -race -tags e2e -count=1 ./test/e2e/...

snapshot: ## goreleaser snapshot build (the pipeline is added by Story 9.1)
	@if [ -f .goreleaser.yaml ]; then \
		$(GORELEASER) release --snapshot --clean; \
	else \
		echo "snapshot: .goreleaser.yaml does not exist yet (added by Story 9.1); nothing to do"; \
	fi

clean: ## Remove build output
	rm -rf bin dist
