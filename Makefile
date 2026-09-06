.PHONY: build test fault-test conformance-test incident-e2e test-race vet vuln-check install-shellcheck workflow-shell-check fmt-check schema-check tidy-check release-check ci clean

GO ?= go
GOFMT ?= gofmt
GOVULNCHECK_VERSION ?= v1.6.0
ACTIONLINT_VERSION := v1.7.12
SHELLCHECK_VERSION := 0.11.0
GO_TOOLCHAIN ?= go1.25.13
export GOTOOLCHAIN := $(GO_TOOLCHAIN)

build:
	$(GO) build -trimpath -o bin/agent-loop ./cmd/agent-loop
	$(GO) build -trimpath -o bin/agent-loop-monitor ./monitor/cmd/agent-loop-monitor

test:
	$(GO) test ./...

fault-test:
	$(GO) test ./... -run '^TestFault' -count=1

conformance-test:
	$(GO) test ./internal/application/conformance -count=1

incident-e2e:
	CGO_ENABLED=0 $(GO) test ./internal/application/incidentloop -count=1

test-race:
	@if [ "$$($(GO) env GOHOSTOS)" = darwin ]; then \
		CGO_ENABLED=1 $(GO) test -race -ldflags=-linkmode=external ./...; \
	else \
		$(GO) test -race ./...; \
	fi

vet:
	$(GO) vet ./...

vuln-check:
	$(GO) run golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION) ./...

install-shellcheck:
	@set -eu; \
	platform=$$(uname -s | tr '[:upper:]' '[:lower:]'); \
	arch=$$(uname -m); \
	case "$$arch" in arm64) arch=aarch64 ;; esac; \
	case "$$platform/$$arch" in darwin/aarch64|darwin/x86_64|linux/aarch64|linux/x86_64) ;; \
		*) echo "Unsupported ShellCheck platform: $$platform/$$arch" >&2; exit 1 ;; esac; \
	tmp=$$(mktemp -d); \
	trap 'rm -rf "$$tmp"' EXIT HUP INT TERM; \
	curl -fsSL "https://github.com/koalaman/shellcheck/releases/download/v$(SHELLCHECK_VERSION)/shellcheck-v$(SHELLCHECK_VERSION).$$platform.$$arch.tar.gz" -o "$$tmp/shellcheck.tar.gz"; \
	tar -xzf "$$tmp/shellcheck.tar.gz" -C "$$tmp"; \
	mkdir -p bin; \
	install -m 755 "$$tmp/shellcheck-v$(SHELLCHECK_VERSION)/shellcheck" bin/shellcheck

workflow-shell-check:
	@test -x bin/shellcheck || { echo 'ShellCheck is required; run make install-shellcheck' >&2; exit 1; }
	@version=$$(bin/shellcheck --version) && \
		printf '%s\n' "$$version" | grep -qx 'version: $(SHELLCHECK_VERSION)' || \
		{ echo 'ShellCheck $(SHELLCHECK_VERSION) is required; run make install-shellcheck' >&2; exit 1; }
	SHELLCHECK_OPTS= bin/shellcheck --norc scripts/*.sh
	SHELLCHECK_OPTS= $(GO) run github.com/rhysd/actionlint/cmd/actionlint@$(ACTIONLINT_VERSION) -shellcheck '$(CURDIR)/bin/shellcheck' -pyflakes ''

fmt-check:
	@files="$$($(GOFMT) -l .)"; \
	if [ -n "$$files" ]; then \
		printf '%s\n' "Go files must be formatted with gofmt:" "$$files"; \
		exit 1; \
	fi

schema-check:
	$(GO) test ./internal/adapter/worker -run '^TestPublishedSchemaReferencesRuntimeSchema$$' -count=1

tidy-check:
	$(GO) mod tidy
	git diff --exit-code -- go.mod go.sum

release-check:
	scripts/check-release.sh

ci: workflow-shell-check fmt-check schema-check tidy-check test fault-test conformance-test test-race vet vuln-check build release-check

clean:
	$(GO) clean
	rm -f bin/agent-loop bin/agent-loop-monitor bin/shellcheck
