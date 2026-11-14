# The targets OpenADLC calls by name: hostd runs `make setup` when a task's
# computer starts, and `make ci` runs when the builder runs `openadlc-ci` and in
# .github/workflows/ci.yml.
#
# `ci` checks what a Linux computer can check on its own: the identity service
# (gofmt, vet, tests) and the update manifest's JSON. gosync needs DynamoDB and
# Redis and is checked by hand with docker compose (AGENTS.md).

GO_VERSION := 1.27.0
# The crew's computers are Debian with no Go, so `setup` puts one under the
# home's cache, and every target uses it when no `go` is on the PATH.
GO_HOME := $(HOME)/.cache/ngp-go/go$(GO_VERSION)
GO := $(shell command -v go 2>/dev/null || echo $(GO_HOME)/bin/go)
GOFMT := $(dir $(GO))gofmt

.PHONY: setup ci fmt vet test site

setup:
	@if command -v go >/dev/null 2>&1; then echo "setup: $$(go version)"; \
	elif [ -x "$(GO_HOME)/bin/go" ]; then echo "setup: go $(GO_VERSION) in $(GO_HOME)"; \
	else \
	  case "$$(uname -m)" in x86_64) arch=amd64 ;; aarch64|arm64) arch=arm64 ;; *) echo "setup: no Go download for $$(uname -m)" >&2; exit 1 ;; esac; \
	  mkdir -p "$(GO_HOME)" && \
	  curl -fsSL "https://go.dev/dl/go$(GO_VERSION).linux-$$arch.tar.gz" | tar -xz -C "$(GO_HOME)" --strip-components=1 && \
	  echo "setup: installed go $(GO_VERSION) in $(GO_HOME)"; \
	fi

ci: fmt vet test site
	@echo "ci: green"

fmt:
	@unformatted="$$(cd identity && $(GOFMT) -l .)"; \
	if [ -n "$$unformatted" ]; then echo "fmt: run gofmt -w on:"; echo "$$unformatted"; exit 1; fi; \
	echo "fmt: identity is gofmt-clean"

vet:
	cd identity && $(GO) vet ./...

test:
	cd identity && $(GO) test ./...

# The browser reads this file to decide whether to update: it has to parse.
site:
	@for f in site/update/*.json; do python3 -m json.tool "$$f" > /dev/null || { echo "site: $$f is not valid JSON"; exit 1; }; done; \
	echo "site: update manifests parse"
