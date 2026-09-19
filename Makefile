# ccgw development tasks. `make` on its own lists them.
#
# Every target here is a shortcut for something the README already documents;
# none of them is required to build or run the gateway. The one with real
# content is `ci`, which runs the same four checks as the CI `test` job, in the
# same order, so a red pipeline can be reproduced before pushing.

GO ?= go
BINARY ?= ccgw
PKG ?= ./cmd/ccgw

# The cross-build matrix from .github/workflows/ci.yml. ccgw is copied between
# machines as a single binary, so a broken cross-build is a real failure.
PLATFORMS ?= linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64

# Where `verify-picker` points and what row it looks for.
BASE ?= http://127.0.0.1:8787
MODEL ?= GPT-5.6

.DEFAULT_GOAL := help
.PHONY: help build install run test race vet fmt fmt-check ci cross verify-picker verify-picker-first-party clean

help: ## List the available targets
	@grep -hE '^[a-z-]+:.*?## ' $(MAKEFILE_LIST) \
		| awk -F':.*?## ' '{printf "  \033[1m%-14s\033[0m %s\n", $$1, $$2}'

build: ## Build ./ccgw (gitignored, as the README's quick start expects)
	$(GO) build -o $(BINARY) $(PKG)

install: ## Install ccgw into GOBIN, replacing the binary on PATH
	$(GO) install $(PKG)

run: build ## Build, then serve on the configured listen address
	./$(BINARY) serve

test: ## Run the unit tests
	$(GO) test ./...

# -race is the point of running this: the gateway writes an SSE stream from a
# keepalive goroutine while the translator writes to the same StreamWriter.
race: ## Run the tests under the race detector, as CI does
	$(GO) test -race ./...

vet: ## Run go vet
	$(GO) vet ./...

fmt: ## Rewrite sources with gofmt
	@gofmt -l -w .

# CI fails on unformatted code rather than fixing it, so this reports instead
# of rewriting; `make fmt` is the one that edits.
fmt-check: ## Fail if any file is not gofmt-clean
	@unformatted=$$(gofmt -l .); \
	if [ -n "$$unformatted" ]; then \
		echo "These files are not gofmt-clean:"; \
		echo "$$unformatted"; \
		gofmt -d .; \
		exit 1; \
	fi

ci: build vet fmt-check race ## Run the checks CI runs, in CI's order

cross: ## Cross-build every platform CI builds, into dist/
	@for platform in $(PLATFORMS); do \
		goos=$${platform%/*}; goarch=$${platform#*/}; \
		if [ "$$goos" = windows ]; then ext=.exe; else ext=; fi; \
		out=dist/$(BINARY)_$${goos}_$${goarch}$$ext; \
		echo "  $$out"; \
		GOOS=$$goos GOARCH=$$goarch $(GO) build -o $$out $(PKG) || exit 1; \
	done

# The picker is the one behaviour no unit test can reach, and a single run
# proves nothing: a row present with the cache in place may have come from
# somewhere else. Only its disappearance once the cache is removed shows the
# cache put it there, so this runs both halves and insists on 0 then 1, then
# writes the cache back. Needs the gateway already serving and a cwd Claude
# Code trusts. Override with `make verify-picker MODEL="GPT-6 Astra"`.
verify-picker: build ## Prove a model reaches Claude Code's /model picker
	./$(BINARY) sync-picker
	@echo "==> with the cache: expecting the row to be found"
	python3 scripts/verify-picker.py $(BASE) "$(MODEL)"
	./$(BINARY) sync-picker -remove
	@echo "==> without the cache: expecting the row to be gone"
	@if python3 scripts/verify-picker.py $(BASE) "$(MODEL)"; then \
		echo "FAIL: the row survived the cache being removed, so it did not come from the cache"; \
		./$(BINARY) sync-picker; \
		exit 1; \
	fi
	@echo "==> both halves as expected; restoring the cache"
	./$(BINARY) sync-picker

# The assume_first_party arrangement loses gateway discovery, so its rows come
# from a curated settings file instead. Same two-run logic as verify-picker: the
# row must be present with the real file and gone with an empty one, or it came
# from somewhere else. Needs a config with assume_first_party: true, so that
# `ccgw sync-picker` writes the settings file rather than the cache.
verify-picker-first-party: build ## Prove the curated rows reach /model under assume_first_party
	@test -n "$(SETTINGS)" || { \
		echo "SETTINGS=<path> is required - run 'ccgw sync-picker' and use the path it prints"; \
		exit 1; \
	}
	@echo "==> with the curated settings: expecting the row to be found"
	python3 scripts/verify-picker.py $(BASE) "$(MODEL)" --first-party "$(SETTINGS)"
	@control=$${TMPDIR:-/tmp}/ccgw-empty-picker.json; \
	echo '{"modelPicker":{"options":[]}}' > $$control; \
	echo "==> with an empty options list: expecting the row to be gone"; \
	if python3 scripts/verify-picker.py $(BASE) "$(MODEL)" --first-party $$control; then \
		echo "FAIL: the row survived an empty modelPicker, so the settings file did not put it there"; \
		rm -f $$control; \
		exit 1; \
	fi; \
	rm -f $$control
	@echo "==> both halves as expected"

clean: ## Remove build output
	rm -f $(BINARY)
	rm -rf dist
