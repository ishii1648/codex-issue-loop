.PHONY: build test fault-test test-race vet vuln-check fmt-check schema-check tidy-check ci clean

GO ?= go
GOFMT ?= gofmt
GOVULNCHECK_VERSION ?= v1.6.0
VULN_GO_TOOLCHAIN ?= go1.25.8

build:
	$(GO) build -trimpath -o bin/agent-loop ./cmd/agent-loop

test:
	$(GO) test ./...

fault-test:
	$(GO) test ./... -run '^TestFault' -count=1

test-race:
	$(GO) test -race ./...

vet:
	$(GO) vet ./...

vuln-check:
	env GOTOOLCHAIN=$(VULN_GO_TOOLCHAIN) $(GO) run golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION) ./...

fmt-check:
	@files="$$($(GOFMT) -l .)"; \
	if [ -n "$$files" ]; then \
		printf '%s\n' "Go files must be formatted with gofmt:" "$$files"; \
		exit 1; \
	fi

schema-check:
	$(GO) test ./internal/worker -run '^TestPublishedSchemaReferencesRuntimeSchema$$' -count=1

tidy-check:
	$(GO) mod tidy
	git diff --exit-code -- go.mod go.sum

ci: fmt-check schema-check tidy-check test fault-test test-race vet vuln-check build

clean:
	$(GO) clean
	rm -f bin/agent-loop
