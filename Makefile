# Multi-module monorepo orchestration. Every directory holding a go.mod is an
# independent module; there is no top-level go.mod, so `go test ./...` at the
# root checks nothing — always go through these targets.
#
# Module paths encode "/" as ":" in target names, because "/" is not usable in
# a make target: logger/zapx is addressed as `make test-logger:zapx`.

# Discover modules by their go.mod files. Nested deeper than -maxdepth 4 a
# module silently disappears from every target — run `make modules` after
# adding one and confirm it is listed.
MODULES := $(shell find . -mindepth 2 -maxdepth 4 -type f -name 'go.mod' | cut -c 3- | sed 's|/go.mod$$||' | sort -u | tr / :)

# golangci-lint is pinned and run via `go run` so no pre-installed binary is
# needed (CI and laptops resolve the same version).
GOLANGCI_LINT ?= go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.6.0

# Extra build tags for tests, e.g. `make test TAGS=integration` (dbx and
# idempotency integration tests need a Docker daemon).
TAGS ?=
TESTTAGS := $(if $(TAGS),-tags=$(TAGS),)

# Override per invocation for focused runs, e.g.
# `make test-obsx GO_TEST_ARGS="-race -run TestSetup -v"`.
GO_TEST_ARGS ?= -race

# Which slice of the module list test-chunk runs: N/M = chunk N of M,
# 1-indexed. `make test-chunk CHUNK=2/4` runs the second quarter.
CHUNK ?= 1/1

.PHONY: all modules build tidy fmt vet lint test test-chunk coverage generate-proto proto-breaking

all: ## Run tidy, fmt, vet and lint for all modules
	$(MAKE) $(addprefix all-,$(MODULES))

build: ## go build for all modules (also the CodeQL manual build command)
	$(MAKE) $(addprefix build-,$(MODULES))

test: ## Run the full gate (tidy, fmt, vet, lint, test) for all modules
	$(MAKE) $(addprefix test-,$(MODULES))

test-chunk: ## Run the gate for one slice of the modules (CHUNK=N/M)
	@CHUNK_NUM=$$(echo $(CHUNK) | cut -d'/' -f1); \
	TOTAL_CHUNKS=$$(echo $(CHUNK) | cut -d'/' -f2); \
	MODULES_LIST="$(MODULES)"; \
	TOTAL_MODULES=$$(echo $$MODULES_LIST | tr ' ' '\n' | wc -l); \
	CHUNK_SIZE=$$(( (TOTAL_MODULES + TOTAL_CHUNKS - 1) / TOTAL_CHUNKS )); \
	START_IDX=$$(( (CHUNK_NUM - 1) * CHUNK_SIZE + 1 )); \
	END_IDX=$$(( CHUNK_NUM * CHUNK_SIZE )); \
	if [ $$END_IDX -gt $$TOTAL_MODULES ]; then END_IDX=$$TOTAL_MODULES; fi; \
	CHUNK_MODULES=$$(echo $$MODULES_LIST | tr ' ' '\n' | sed -n "$${START_IDX},$${END_IDX}p" | tr '\n' ' '); \
	echo "Running tests for chunk $(CHUNK): modules $$START_IDX-$$END_IDX of $$TOTAL_MODULES"; \
	echo "Modules: $$CHUNK_MODULES"; \
	for mod in $$CHUNK_MODULES; do \
		$(MAKE) test-$$mod || exit 1; \
	done

tidy: ## go mod tidy for all modules
	$(MAKE) $(addprefix tidy-,$(MODULES))

fmt: ## go fmt for all modules
	$(MAKE) $(addprefix fmt-,$(MODULES))

vet: ## go vet for all modules
	$(MAKE) $(addprefix vet-,$(MODULES))

lint: ## golangci-lint for all modules
	$(MAKE) $(addprefix lint-,$(MODULES))

modules: ## List the modules the Makefile discovered
	@echo $(MODULES) | tr ' ' '\n'

build-%:
	cd $(subst :,/,$*) && go build ./...

tidy-%:
	cd $(subst :,/,$*) && go mod tidy

fmt-%:
	cd $(subst :,/,$*) && go fmt ./...

vet-%:
	cd $(subst :,/,$*) && go vet ./...

lint-%:
	cd $(subst :,/,$*) && $(GOLANGCI_LINT) run --config $(CURDIR)/.golangci.yml --timeout 10m

all-%:
	cd $(subst :,/,$*) && go mod tidy && go fmt ./... && go vet ./... && \
		$(GOLANGCI_LINT) run --config $(CURDIR)/.golangci.yml --timeout 10m

# The single-module gate: tidy, fmt, vet, lint, then race-enabled tests with
# coverage. Steps run sequentially in one recipe so `make -j` parallelises
# across modules, never within one.
test-%:
	cd $(subst :,/,$*) && go mod tidy && go fmt ./... && go vet ./... && \
		$(GOLANGCI_LINT) run --config $(CURDIR)/.golangci.yml --timeout 10m && \
		go test ./... $(GO_TEST_ARGS) $(TESTTAGS) -coverprofile coverage.out

coverage: ## Merge per-module coverage.out files into one root coverage.out
	@echo "mode: atomic" > coverage.out
	@for f in $$(find . -mindepth 2 -type f -name coverage.out); do \
		tail -n +2 $$f >> coverage.out; \
	done
	@echo "merged $$(find . -mindepth 2 -type f -name coverage.out | wc -l | tr -d ' ') module profiles into coverage.out"

generate-proto: ## Regenerate protobuf stubs and lint the contracts
	buf generate && buf lint

proto-breaking: ## Fail on a backward-incompatible proto change vs main
	buf breaking --against 'https://github.com/duynhlab/pkg.git#branch=main'

# Tag and push one module release: `make release-obsx VER=0.36.0` pushes the
# tag obsx/v0.36.0. A pushed tag is immutable (the Go module proxy caches it
# immediately) — a wrong one cannot be fixed, only superseded by a new patch.
# Releases are cut from commits already on origin/main only.
release-%:
	$(eval REL_PATH := $(subst :,/,$*))
	@if [ -z "$(VER)" ]; then echo "usage: make release-<module> VER=x.y.z (no v prefix)"; exit 1; fi
	@echo "$(VER)" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$$' || { echo "VER must be semver without the v prefix: VER=$(VER)"; exit 1; }
	@if ! test -f "$(REL_PATH)/go.mod"; then echo "unknown module $(REL_PATH): no $(REL_PATH)/go.mod"; exit 1; fi
	@if ! git diff --quiet HEAD; then echo "working tree not clean, commit or stash first"; exit 1; fi
	@git fetch -q origin main && git merge-base --is-ancestor HEAD origin/main || { echo "HEAD is not on origin/main; releases are cut from pushed main only"; exit 1; }
	git tag -a "$(REL_PATH)/v$(VER)" -m "$(REL_PATH)/v$(VER)"
	git push origin "$(REL_PATH)/v$(VER)"
