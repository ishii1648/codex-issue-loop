.PHONY: build test fault-test test-race vet fmt-check schema-check tidy-check ci clean

GO ?= go
GOFMT ?= gofmt

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

ci: fmt-check schema-check tidy-check test fault-test test-race vet build

clean:
	$(GO) clean
	rm -f bin/agent-loop
