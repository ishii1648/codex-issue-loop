.PHONY: build test

build:
	go build -trimpath -o bin/agent-loop ./cmd/agent-loop

test:
	go test ./...
